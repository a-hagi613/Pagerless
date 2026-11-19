// Command pagerless-server runs the Pagerless control plane: the Page
// reconcilers, the agent report endpoint, and — when it is configured — the
// embedded dashboard. It installs no admission webhook, so it needs no serving
// certificate and holds no cluster-scoped admission permission (ADR-0037).
package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/dashboardauth"
	"github.com/a-hagi613/pagerless/internal/github"
	"github.com/a-hagi613/pagerless/internal/patchresources"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/scale"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/verification"
	"github.com/a-hagi613/pagerless/internal/watcher"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pagerless-server:", err)
		os.Exit(1)
	}
}

// defaultGitHubTokenFile is where the Deployment mounts the write-scoped GitHub token.
const defaultGitHubTokenFile = "/var/run/pagerless/credentials/GITHUB_TOKEN"

// defaultArgoCDTokenFile is where the Argo CD API token is mounted.
const defaultArgoCDTokenFile = "/var/run/secrets/pagerless/argocd/token"

// defaultAlertmanagerTokenFile is the rotating Secret that authenticates webhook deliveries.
const defaultAlertmanagerTokenFile = "/var/run/pagerless/alertmanager/token"

// tokenFromFile returns a source that reads the token at every call, because
// the kubelet rotates a projected token in place.
func tokenFromFile(path string) func() (string, error) {
	return func() (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read the token file %s: %w", path, err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
}

// splitList splits a comma-separated list and drops empty items.
func splitList(s string) []string {
	var items []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func run() error {
	defaults := patchresources.DefaultBounds()
	defaultCPU, defaultMemory := defaults[corev1.ResourceCPU], defaults[corev1.ResourceMemory]
	defaultScale := scale.DefaultBounds()
	// The observation window is five minutes unless the environment shortens it.
	defaultVerificationWindow, err := verification.WindowFromEnvironment(os.Getenv)
	if err != nil {
		return err
	}
	var (
		namespace = flag.String("namespace", os.Getenv("POD_NAMESPACE"),
			"namespace that holds Pages and agent Jobs (default: $POD_NAMESPACE)")
		agentImage    = flag.String("agent-image", "", "agent container image")
		reportURL     = flag.String("report-url", "", "base URL that agent Jobs use to reach the report API")
		reportBind    = flag.String("report-bind-address", ":8080", "address the agent report API listens on")
		metricsBind   = flag.String("metrics-bind-address", ":9090", "address the Prometheus metrics API listens on")
		dashboardBind = flag.String("dashboard-bind-address", "",
			"address the dashboard and its read-only API listen on; empty disables the dashboard")
		grafanaURL = flag.String("grafana-url", "",
			"Grafana base URL that the browser can reach, for example while the Service is port-forwarded; "+
				"empty disables the embedded panels")
		grafanaMetricsDashboard = flag.String("grafana-target-dashboard-uid", server.DefaultMetricsDashboardUID,
			"Grafana dashboard that charts a Target's metrics")
		grafanaLogsDashboard = flag.String("grafana-logs-dashboard-uid", server.DefaultLogsDashboardUID,
			"Grafana dashboard that shows a Target's logs")
		historyPath = flag.String("history-path", "/var/lib/pagerless/history.db",
			"SQLite file that records Page snapshots and steps")
		healthBind   = flag.String("health-probe-bind-address", ":8081", "address the health probes listen on")
		agentAccount = flag.String("agent-service-account", server.DefaultAgentServiceAccount,
			"service account that agent Jobs run as")
		serverAccount = flag.String("server-service-account", server.DefaultServerServiceAccount,
			"service account that pagerless-server runs as")
		agentDeadline     = flag.Duration("agent-deadline", server.DefaultAgentDeadline, "active deadline of an agent Job")
		narrativeDeadline = flag.Duration("narrative-deadline", server.DefaultNarrativeDeadline,
			"active deadline of a Narrative Job")
		sandboxURL = flag.String("sandbox-url", os.Getenv("PAGERLESS_TARGET_REPOSITORY_URL"),
			"git URL of the Target Repository; the git-backed Actions are accepted only when it is set")
		sandboxBranch = flag.String("sandbox-branch", "main",
			"branch of the Target Repository that pull requests target")
		sandboxReadTokenFile = flag.String("sandbox-read-token-file", "",
			"optional file that holds the read-only token for the HTTPS Target Repository")
		patchable = flag.String("patch-namespaces", os.Getenv(agentapi.EnvAllowedNamespaces),
			"comma-separated namespaces whose Targets PatchResources and Rollback may change")
		patchMinCPU = flag.String("patch-min-cpu", defaultCPU.Min.String(),
			"minimum CPU request or limit in PatchResources")
		patchMaxCPU = flag.String("patch-max-cpu", defaultCPU.Max.String(),
			"maximum CPU request or limit in PatchResources")
		patchMinMemory = flag.String("patch-min-memory", defaultMemory.Min.String(),
			"minimum memory request or limit in PatchResources")
		patchMaxMemory = flag.String("patch-max-memory", defaultMemory.Max.String(),
			"maximum memory request or limit in PatchResources")
		scaleMin = flag.Int("scale-min-replicas", int(defaultScale.Min),
			"minimum replica count in Scale")
		scaleMax = flag.Int("scale-max-replicas", int(defaultScale.Max),
			"maximum replica count in Scale")
		githubAPIURL = flag.String("github-api-url", github.DefaultGitHubAPI,
			"base URL of the Target Repository's GitHub API")
		githubRepository = flag.String("github-repository", os.Getenv("PAGERLESS_GITHUB_REPOSITORY"),
			"owner/name of the Target Repository; empty derives it from --sandbox-url")
		githubTokenFile = flag.String("github-token-file", defaultGitHubTokenFile,
			"file that holds the write-scoped token that publishes a Proposal")
		argoCDURL = flag.String("argocd-url", "",
			"base URL of the Argo CD API server, which Verification asks whether the merged change is synced; "+
				"empty skips the GitOps check")
		argoCDTokenFile = flag.String("argocd-token-file", defaultArgoCDTokenFile,
			"file that holds the bearer token that authenticates to the Argo CD API")
		argoCDApplication = flag.String("argocd-application", verification.DefaultApplication,
			"Argo CD application that deploys the Target Repository")
		alertmanagerURL = flag.String("alertmanager-url", "",
			"base URL of Alertmanager; empty disables the alert receiver and the alert-resolution check")
		alertmanagerTokenFile = flag.String("alertmanager-token-file", defaultAlertmanagerTokenFile,
			"file that holds the required bearer token for Alertmanager webhook deliveries")
		verificationWindow = flag.Duration("verification-window", defaultVerificationWindow,
			"how long Verification observes the Target for new restarts and OOMKills")
		alertNamespaces = flag.String("alert-namespaces", "",
			"comma-separated namespaces whose opted-in alerts open Pages (empty: none)")
		targetNamespaces = flag.String("target-namespaces", "default",
			"comma-separated namespaces whose Targets the dashboard's Start request offers")
		dashboardBaseURL = flag.String("dashboard-base-url", "http://localhost:8080",
			"browser-facing base URL of the dashboard, used to build the OIDC redirect URI "+
				"(overridden by $PAGERLESS_DASHBOARD_BASE_URL)")
		oidcIssuer = flag.String("oidc-issuer", "",
			"realm issuer that signs dashboard users in; empty leaves the dashboard disabled")
		oidcClientID = flag.String("oidc-client-id", "pagerless-dashboard",
			"confidential OIDC client for dashboard sign-in")
		oidcClientSecretFile = flag.String("oidc-client-secret-file", "/var/run/pagerless/keycloak/client-secret",
			"file that holds the dashboard client secret")
		approversGroup = flag.String("approvers-group", "pagerless-approvers",
			"realm group whose members may approve through the dashboard")
		dashboardCookieSecure = flag.Bool("dashboard-cookie-secure", true,
			"mark dashboard session cookies Secure; set false only when the browser reaches the dashboard over HTTP")
		watcherEnabled = flag.Bool("watcher", true,
			"watch the cluster for failing Targets, open Pages, and enforce the guardrails")
		watcherInclude = flag.String("watcher-namespaces-include", "",
			"comma-separated namespaces to watch; empty watches every namespace")
		watcherExclude = flag.String("watcher-namespaces-exclude", "",
			"comma-separated namespaces to ignore in addition to the release, kube-public, and kube-node-lease namespaces")
		kubeSystemMode = flag.String("watcher-kube-system-mode", "warn-only",
			"how kube-system is treated: warn-only, watch, or ignore")
		nodePagesMode = flag.String("watcher-node-pages-mode", "watch",
			"how Node faults are treated: watch, warn-only, or ignore")
		clusterID = flag.String("cluster-id", os.Getenv("PAGERLESS_CLUSTER_ID"),
			"stable cluster identifier that every Fingerprint hashes; empty derives it from the kube-system namespace UID")
		maxConcurrent = flag.Int("max-concurrent-investigations", watcher.DefaultMaxConcurrentPages,
			"the number of investigations that may run at once; the rest are queued with a visible reason")
		maxPerHour = flag.Int("max-investigations-per-hour", watcher.DefaultMaxPagesPerHour,
			"the number of investigations that may start per sliding hour")
	)
	zapOptions := zap.Options{}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	bounds, err := parsePatchBounds(*patchMinCPU, *patchMaxCPU, *patchMinMemory, *patchMaxMemory)
	if err != nil {
		return err
	}
	scaleBounds, err := parseScaleBounds(*scaleMin, *scaleMax)
	if err != nil {
		return err
	}

	allowed, err := parseNamespaces(*alertNamespaces)
	if err != nil {
		return err
	}
	targets, err := parseNamespaces(*targetNamespaces)
	if err != nil {
		return err
	}
	if envDashboardBaseURL := strings.TrimSpace(os.Getenv("PAGERLESS_DASHBOARD_BASE_URL")); envDashboardBaseURL != "" {
		*dashboardBaseURL = envDashboardBaseURL
	}
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	ctx := ctrl.SetupSignalHandler()

	listener, err := net.Listen("tcp", *reportBind)
	if err != nil {
		return fmt.Errorf("listen for the report API: %w", err)
	}
	metricsListener, err := net.Listen("tcp", *metricsBind)
	if err != nil {
		return fmt.Errorf("listen for the metrics API: %w", err)
	}
	var sandboxReadToken func() (string, error)
	if *sandboxReadTokenFile != "" {
		sandboxReadToken = tokenFromFile(*sandboxReadTokenFile)
	}

	cfg := server.Config{
		Namespace:                  *namespace,
		AgentImage:                 *agentImage,
		AgentServerURL:             *reportURL,
		AgentRuntime:               agentRuntimeFromEnvironment(),
		ReportListener:             listener,
		MetricsListener:            metricsListener,
		TargetNamespaces:           targets,
		GrafanaURL:                 *grafanaURL,
		GrafanaMetricsDashboardUID: *grafanaMetricsDashboard,
		GrafanaLogsDashboardUID:    *grafanaLogsDashboard,
		HistoryPath:                *historyPath,
		GitHubAPIURL:               *githubAPIURL,
		GitHubRepository:           *githubRepository,
		GitHubToken:                tokenFromFile(*githubTokenFile),
		ArgoCDURL:                  *argoCDURL,
		ArgoCDApplication:          *argoCDApplication,
		AlertmanagerURL:            *alertmanagerURL,
		VerificationWindow:         *verificationWindow,
		AgentServiceAccount:        *agentAccount,
		ServerServiceAccount:       *serverAccount,
		AgentDeadline:              *agentDeadline,
		NarrativeDeadline:          *narrativeDeadline,
		Sandbox: sandbox.Repository{
			URL: *sandboxURL, Branch: *sandboxBranch, ReadToken: sandboxReadToken,
		},
		AllowedNamespaces:  splitList(*patchable),
		PatchBounds:        bounds,
		ScaleBounds:        scaleBounds,
		AlertNamespaces:    allowed,
		HealthProbeAddress: *healthBind,
	}
	if *argoCDURL != "" {
		cfg.ArgoCDToken = tokenFromFile(*argoCDTokenFile)
	}
	if *alertmanagerURL != "" {
		cfg.AlertmanagerToken = tokenFromFile(*alertmanagerTokenFile)
	}
	if *dashboardBind != "" {
		// The dashboard is served only when an identity provider is configured. Its
		// decisions patch Page annotations, which the Page controller reconciles.
		dashboardAuth, err := dashboardauth.New(dashboardauth.Config{
			Issuer:         *oidcIssuer,
			ClientID:       *oidcClientID,
			ClientSecret:   tokenFromFile(*oidcClientSecretFile),
			RedirectURL:    strings.TrimSuffix(*dashboardBaseURL, "/") + "/auth/callback",
			ApproversGroup: *approversGroup,
			SecureCookies:  *dashboardCookieSecure,
		})
		if err != nil {
			return fmt.Errorf("configure dashboard sign-in: %w", err)
		}
		dashboardListener, err := net.Listen("tcp", *dashboardBind)
		if err != nil {
			return fmt.Errorf("listen for the dashboard: %w", err)
		}
		cfg.DashboardAuth = dashboardAuth
		cfg.DashboardListener = dashboardListener
	}
	if *watcherEnabled {
		watchConfig, err := watcherConfig(*watcherInclude, *watcherExclude, *kubeSystemMode, *nodePagesMode,
			*clusterID, *namespace, *maxConcurrent, *maxPerHour)
		if err != nil {
			return err
		}
		cfg.Watcher = &watchConfig
	}

	mgr, err := server.New(restConfig, cfg)
	if err != nil {
		return err
	}
	ctrl.Log.WithName("setup").Info("starting pagerless-server", "namespace", *namespace,
		"alertNamespaces", allowed, "dashboard", cfg.DashboardListener != nil)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}

// parsePatchBounds validates the resource quantities supplied through the server's flags.
func parsePatchBounds(
	cpuMin, cpuMax, memoryMin, memoryMax string,
) (map[corev1.ResourceName]patchresources.Bounds, error) {
	bounds := make(map[corev1.ResourceName]patchresources.Bounds, 2)
	for _, setting := range []struct {
		name     corev1.ResourceName
		min, max string
	}{
		{corev1.ResourceCPU, cpuMin, cpuMax},
		{corev1.ResourceMemory, memoryMin, memoryMax},
	} {
		minimum, err := resource.ParseQuantity(setting.min)
		if err != nil {
			return nil, fmt.Errorf("%s minimum bound is not a resource quantity", setting.name)
		}
		maximum, err := resource.ParseQuantity(setting.max)
		if err != nil {
			return nil, fmt.Errorf("%s maximum bound is not a resource quantity", setting.name)
		}
		if minimum.Sign() <= 0 || maximum.Sign() <= 0 || minimum.Cmp(maximum) > 0 {
			return nil, fmt.Errorf("%s bounds must be positive and minimum must not exceed maximum", setting.name)
		}
		bounds[setting.name] = patchresources.Bounds{Min: minimum, Max: maximum}
	}
	return bounds, nil
}

// parseScaleBounds validates the replica bounds supplied through the server's flags.
func parseScaleBounds(minimum, maximum int) (scale.Bounds, error) {
	if minimum < 0 || maximum < minimum {
		return scale.Bounds{}, errors.New("scale bounds must be non-negative and the minimum must not exceed the maximum")
	}
	if maximum > math.MaxInt32 {
		return scale.Bounds{}, errors.New("scale bounds must fit in a replica count")
	}
	return scale.Bounds{Min: int32(minimum), Max: int32(maximum)}, nil
}

// agentRuntimeFromEnvironment copies the non-secret configuration supplied by the bootstrap.
// Inference credentials are referenced directly by agent Jobs, not read by the server.
func agentRuntimeFromEnvironment() controller.AgentRuntimeConfig {
	return controller.AgentRuntimeConfig{
		Provider:               os.Getenv(agentapi.EnvModelProvider),
		GatewayURL:             os.Getenv(agentapi.EnvMCPGatewayURL),
		KeycloakIssuer:         os.Getenv(agentapi.EnvKeycloakIssuer),
		KeycloakTokenURL:       os.Getenv(agentapi.EnvKeycloakTokenURL),
		KeycloakClientID:       os.Getenv(agentapi.EnvKeycloakClientID),
		ReplayFile:             os.Getenv(agentapi.EnvReplayFile),
		RecordingFile:          os.Getenv(agentapi.EnvRecordingFile),
		RecordingPVC:           os.Getenv("PAGERLESS_RECORDING_PVC"),
		NarrativeReplayFile:    os.Getenv(agentapi.EnvNarrativeReplayFile),
		NarrativeRecordingFile: os.Getenv(controller.NarrativeRecordingFileEnv),
		SandboxOwner:           os.Getenv(agentapi.EnvSandboxOwner),
		SandboxRepo:            os.Getenv(agentapi.EnvSandboxRepo),
		ArgoCDApplication:      os.Getenv(agentapi.EnvArgoCDApplication),
		AllowedNamespaces:      os.Getenv(agentapi.EnvAllowedNamespaces),
		AWSRegion:              os.Getenv("AWS_REGION"),
	}
}

// parseNamespaces splits the comma-separated list of namespaces and checks that
// each one is a valid namespace name.
func parseNamespaces(list string) ([]string, error) {
	var namespaces []string
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return nil, fmt.Errorf("namespace %q: %s", name, strings.Join(errs, "; "))
		}
		namespaces = append(namespaces, name)
	}
	return namespaces, nil
}

// watcherConfig validates the watcher flags into a configuration.
func watcherConfig(include, exclude, kubeSystem, nodePages, clusterID, namespace string,
	maxConcurrent, maxPerHour int,
) (watcher.Config, error) {
	included, err := parseNamespaces(include)
	if err != nil {
		return watcher.Config{}, fmt.Errorf("watcher include list: %w", err)
	}
	excluded, err := parseNamespaces(exclude)
	if err != nil {
		return watcher.Config{}, fmt.Errorf("watcher exclude list: %w", err)
	}
	kubeSystemMode, err := parseNamespaceMode(kubeSystem)
	if err != nil {
		return watcher.Config{}, fmt.Errorf("--watcher-kube-system-mode: %w", err)
	}
	nodeMode, err := parseNamespaceMode(nodePages)
	if err != nil {
		return watcher.Config{}, fmt.Errorf("--watcher-node-pages-mode: %w", err)
	}
	if maxConcurrent < 1 {
		return watcher.Config{}, errors.New("--max-concurrent-investigations must be at least 1")
	}
	if maxPerHour < 0 {
		return watcher.Config{}, errors.New("--max-investigations-per-hour must not be negative")
	}
	return watcher.Config{
		Namespace:          namespace,
		ClusterID:          clusterID,
		IncludeNamespaces:  included,
		ExcludeNamespaces:  excluded,
		KubeSystemMode:     kubeSystemMode,
		NodePagesMode:      nodeMode,
		MaxConcurrentPages: maxConcurrent,
		MaxPagesPerHour:    maxPerHour,
	}, nil
}

// parseNamespaceMode validates one watcher mode flag.
func parseNamespaceMode(mode string) (watcher.NamespaceMode, error) {
	switch parsed := watcher.NamespaceMode(strings.TrimSpace(mode)); parsed {
	case watcher.ModeWatch, watcher.ModeWarnOnly, watcher.ModeIgnore:
		return parsed, nil
	default:
		return "", fmt.Errorf("%q is not one of watch, warn-only, or ignore", mode)
	}
}
