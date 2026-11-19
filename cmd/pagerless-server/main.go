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

// defaultAdminTokenFile is where the chart mounts the bootstrap token Secret.
const defaultAdminTokenFile = "/var/run/pagerless/auth/admin-token"

// defaultSessionKeyFile is where the chart mounts the session signing key Secret.
const defaultSessionKeyFile = "/var/run/pagerless/auth/session-key"

// defaultCredentialsMountPath is where the chart mounts the credentials Secret.
const defaultCredentialsMountPath = "/var/run/pagerless/credentials"

// version is the release version, set at link time by the image build.
var version = "dev"

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

// bytesFromFile returns a source that reads the file at every call, because the
// kubelet replaces a mounted Secret in place. The value is not trimmed: a
// signing key is used exactly as the chart generated it, and the reader trims
// surrounding whitespace when it validates the length.
func bytesFromFile(path string) func() ([]byte, error) {
	return func() ([]byte, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read the key file %s: %w", path, err)
		}
		return raw, nil
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
		patchable = flag.String("patch-namespaces", os.Getenv("PAGERLESS_ALLOWED_NAMESPACES"),
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
		connectorsConfig = flag.String("connectors-config", "",
			"path of the Connector configuration ConfigMap; empty disables every external Connector")
		webhookAlertmanagerTokenFile = flag.String("webhook-alertmanager-token-file", "",
			"file that holds the bearer token of the Alertmanager webhook dialect; empty disables it")
		webhookGrafanaSecretFile = flag.String("webhook-grafana-secret-file", "",
			"file that holds the HMAC-SHA256 secret of the Grafana webhook dialect; empty disables it")
		webhookDatadogTokenFile = flag.String("webhook-datadog-token-file", "",
			"file that holds the bearer token of the Datadog webhook dialect; empty disables it")
		webhookGenericTokenFile = flag.String("webhook-generic-token-file", "",
			"file that holds the bearer token of the generic webhook dialect; empty disables it")
		verificationWindow = flag.Duration("verification-window", defaultVerificationWindow,
			"how long Verification observes the Target for new restarts and OOMKills")
		alertNamespaces = flag.String("alert-namespaces", "",
			"comma-separated namespaces whose opted-in alerts open Pages (empty: none)")
		targetNamespaces = flag.String("target-namespaces", "default",
			"comma-separated namespaces whose Targets the dashboard's Start request offers")
		dashboardBaseURL = flag.String("dashboard-base-url", "http://localhost:8080",
			"browser-facing base URL of the Portal, used to scope the session cookie and mark it Secure "+
				"(overridden by $PAGERLESS_DASHBOARD_BASE_URL)")
		authMode = flag.String("auth-mode", os.Getenv("PAGERLESS_AUTH_MODE"),
			`Portal authentication mode: "token" (default) or "disabled"`)
		authAdminTokenFile = flag.String("auth-admin-token-file", defaultAdminTokenFile,
			"file that holds the bootstrap token that signs the operator in")
		authSessionKeyFile = flag.String("auth-session-key-file", defaultSessionKeyFile,
			"file that holds the HMAC-SHA256 key that signs session cookies")
		authBearerToken = flag.Bool("auth-bearer-token", false,
			"accept Authorization: Bearer <bootstrap token> on /api/* for scripted calls")
		authIdentityHeader = flag.String("auth-identity-header", "",
			"trusted identity header (such as X-Auth-Request-User) that names the operator in disabled mode")
		authSessionTTL = flag.Duration("auth-session-ttl", 0,
			"lifetime of a signed session cookie (default: 8h)")
		dashboardCookieSecure = flag.String("dashboard-cookie-secure", "",
			`mark session cookies Secure: "" derives it from --dashboard-base-url, or pass "true"/"false"`)
		credentialsSecret = flag.String("credentials-secret", "pagerless-credentials",
			"Secret that holds the AI, Target Repository, and connector credentials")
		credentialsExternalManager = flag.String("credentials-external-manager", "",
			"secret manager that owns the credentials Secret; empty lets the Portal and CLI write it")
		credentialsMountPath = flag.String("credentials-mount-path", defaultCredentialsMountPath,
			"directory where the credentials Secret is mounted")
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
		ConnectorsPath:             *connectorsConfig,
		VerificationWindow:         *verificationWindow,
		AgentServiceAccount:        *agentAccount,
		ServerServiceAccount:       *serverAccount,
		AgentDeadline:              *agentDeadline,
		NarrativeDeadline:          *narrativeDeadline,
		Sandbox: sandbox.Repository{
			URL: *sandboxURL, Branch: *sandboxBranch, ReadToken: sandboxReadToken,
		},
		AllowedNamespaces:          splitList(*patchable),
		PatchBounds:                bounds,
		ScaleBounds:                scaleBounds,
		AlertNamespaces:            allowed,
		HealthProbeAddress:         *healthBind,
		CredentialsSecretName:      *credentialsSecret,
		CredentialsExternalManager: *credentialsExternalManager,
		CredentialsMountPath:       *credentialsMountPath,
		Version:                    version,
	}
	if *argoCDURL != "" {
		cfg.ArgoCDToken = tokenFromFile(*argoCDTokenFile)
	}
	if *alertmanagerURL != "" {
		cfg.AlertmanagerToken = tokenFromFile(*alertmanagerTokenFile)
	}
	if *webhookAlertmanagerTokenFile != "" {
		cfg.AlertmanagerWebhookToken = tokenFromFile(*webhookAlertmanagerTokenFile)
	}
	if *webhookGrafanaSecretFile != "" {
		cfg.GrafanaWebhookSecret = tokenFromFile(*webhookGrafanaSecretFile)
	}
	if *webhookDatadogTokenFile != "" {
		cfg.DatadogWebhookToken = tokenFromFile(*webhookDatadogTokenFile)
	}
	if *webhookGenericTokenFile != "" {
		cfg.GenericWebhookToken = tokenFromFile(*webhookGenericTokenFile)
	}
	if *dashboardBind != "" {
		// The Portal authenticates with the bootstrap token and a signed session
		// cookie (ADR-0032). Its decisions patch Page annotations, which the Page
		// controller reconciles.
		dashboardListener, err := net.Listen("tcp", *dashboardBind)
		if err != nil {
			return fmt.Errorf("listen for the dashboard: %w", err)
		}
		cfg.DashboardAuthConfig = &dashboardauth.Config{
			Mode:           *authMode,
			AdminToken:     tokenFromFile(*authAdminTokenFile),
			SessionKey:     bytesFromFile(*authSessionKeyFile),
			BaseURL:        *dashboardBaseURL,
			CookieSecure:   *dashboardCookieSecure,
			BearerToken:    *authBearerToken,
			IdentityHeader: *authIdentityHeader,
			SessionTTL:     *authSessionTTL,
		}
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

// agentRuntimeFromEnvironment copies the non-secret configuration supplied by the
// bootstrap or the chart. The model API key never reaches the server as a value:
// agent Jobs mount the credentials Secret's key directly.
func agentRuntimeFromEnvironment() controller.AgentRuntimeConfig {
	return controller.AgentRuntimeConfig{
		ModelBaseURL:               os.Getenv("AI_BASE_URL"),
		ModelName:                  os.Getenv("AI_MODEL"),
		ModelKeySecret:             os.Getenv("PAGERLESS_CREDENTIALS_SECRET"),
		ModelKeyKey:                os.Getenv("PAGERLESS_MODEL_API_KEY_KEY"),
		ModelHeaders:               os.Getenv("PAGERLESS_MODEL_HEADERS"),
		ModelStructuredOutput:      os.Getenv("PAGERLESS_MODEL_STRUCTURED_OUTPUT"),
		ModelMaxTokensField:        os.Getenv("PAGERLESS_MODEL_MAX_TOKENS_FIELD"),
		ModelSupportsTemperature:   os.Getenv("PAGERLESS_MODEL_SUPPORTS_TEMPERATURE"),
		AllowInsecureModelEndpoint: os.Getenv("PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT") == "1",
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
