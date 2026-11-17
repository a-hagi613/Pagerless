// Command pagerless-server runs the Pagerless control plane: the Triage
// reconciler, the agent report endpoint, and the admission webhooks.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/verification"
	"github.com/a-hagi613/pagerless/internal/webhooktls"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pagerless-server:", err)
		os.Exit(1)
	}
}

// defaultExecutorTokenFile is where the Deployment projects the token for the Executor.
const defaultExecutorTokenFile = "/var/run/secrets/pagerless/executor/token"

// defaultArgoCDTokenFile is where the Argo CD API token is mounted.
const defaultArgoCDTokenFile = "/var/run/secrets/pagerless/argocd/token"

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
	var (
		namespace = flag.String("namespace", os.Getenv("POD_NAMESPACE"),
			"namespace that holds Triages and agent Jobs (default: $POD_NAMESPACE)")
		agentImage     = flag.String("agent-image", "", "agent container image")
		reportURL      = flag.String("report-url", "", "base URL that agent Jobs use to reach the report API")
		reportBind     = flag.String("report-bind-address", ":8080", "address the agent report API listens on")
		healthBind     = flag.String("health-probe-bind-address", ":8081", "address the health probes listen on")
		webhookPort    = flag.Int("webhook-port", 9443, "port the admission webhooks listen on")
		webhookCertDir = flag.String("webhook-cert-dir", "/var/run/pagerless/webhook-certs",
			"directory that receives the generated webhook serving certificate")
		webhookService = flag.String("webhook-service", "pagerless-server",
			"name of the Service that fronts the admission webhooks")
		webhookConfig = flag.String("webhook-config", "pagerless-server",
			"name of the ValidatingWebhookConfiguration that receives the certificate authority")
		mutatingWebhookConfig = flag.String("mutating-webhook-config", "pagerless-server",
			"name of the MutatingWebhookConfiguration that receives the certificate authority")
		agentAccount = flag.String("agent-service-account", server.DefaultAgentServiceAccount,
			"service account that agent Jobs run as")
		serverAccount = flag.String("server-service-account", server.DefaultServerServiceAccount,
			"service account that pagerless-server runs as")
		agentDeadline = flag.Duration("agent-deadline", server.DefaultAgentDeadline, "active deadline of an agent Job")
		sandboxURL    = flag.String("sandbox-url", os.Getenv("PAGERLESS_SANDBOX_URL"),
			"git URL of the Sandbox; PatchResources is accepted only when it is set")
		sandboxBranch = flag.String("sandbox-branch", "main", "branch of the Sandbox that Argo CD deploys")
		patchable     = flag.String("patch-namespaces", os.Getenv(agentapi.EnvAllowedNamespaces),
			"comma-separated namespaces whose Workloads PatchResources may change")
		executorURL = flag.String("executor-url", "",
			"base URL of pagerless-executor (default: http://pagerless-executor.<namespace>.svc:8080)")
		executorTokenFile = flag.String("executor-token-file", defaultExecutorTokenFile,
			"file that holds the projected service account token, with audience "+executorapi.Audience+
				", that authenticates the server to the Executor")
		argoCDURL = flag.String("argocd-url", "http://argocd-server.argocd.svc",
			"base URL of the Argo CD API server, which Verification asks whether the merged change is synced")
		argoCDTokenFile = flag.String("argocd-token-file", defaultArgoCDTokenFile,
			"file that holds the bearer token that authenticates to the Argo CD API")
		argoCDApplication = flag.String("argocd-application", verification.DefaultApplication,
			"Argo CD application that deploys the Sandbox")
		alertmanagerURL = flag.String("alertmanager-url", "http://alertmanager.monitoring.svc:9093",
			"base URL of Alertmanager, which Verification asks whether the triggering alert has resolved")
		verificationWindow = flag.Duration("verification-window", verification.DefaultWindow,
			"how long Verification observes the Workload for new restarts and OOMKills")
		alertNamespaces = flag.String("alert-namespaces", "",
			"comma-separated namespaces whose opted-in alerts start Triages (empty: none)")
	)
	zapOptions := zap.Options{}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	allowed, err := parseNamespaces(*alertNamespaces)
	if err != nil {
		return err
	}
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	ctx := ctrl.SetupSignalHandler()

	scheme, err := server.Scheme()
	if err != nil {
		return err
	}
	plain, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	if _, err := webhooktls.Provision(ctx, plain, webhooktls.Options{
		ServiceName:        *webhookService,
		Namespace:          *namespace,
		ConfigName:         *webhookConfig,
		MutatingConfigName: *mutatingWebhookConfig,
		CertDir:            *webhookCertDir,
		Now:                time.Now(),
	}); err != nil {
		return fmt.Errorf("provision webhook TLS: %w", err)
	}

	if *executorURL == "" {
		*executorURL = fmt.Sprintf("http://pagerless-executor.%s.svc:8080", *namespace)
	}
	listener, err := net.Listen("tcp", *reportBind)
	if err != nil {
		return fmt.Errorf("listen for the report API: %w", err)
	}
	mgr, err := server.New(restConfig, server.Config{
		Namespace:            *namespace,
		AgentImage:           *agentImage,
		AgentServerURL:       *reportURL,
		AgentRuntime:         agentRuntimeFromEnvironment(),
		ReportListener:       listener,
		ExecutorURL:          *executorURL,
		ExecutorToken:        tokenFromFile(*executorTokenFile),
		ArgoCDURL:            *argoCDURL,
		ArgoCDToken:          tokenFromFile(*argoCDTokenFile),
		ArgoCDApplication:    *argoCDApplication,
		AlertmanagerURL:      *alertmanagerURL,
		VerificationWindow:   *verificationWindow,
		AgentServiceAccount:  *agentAccount,
		ServerServiceAccount: *serverAccount,
		AgentDeadline:        *agentDeadline,
		Sandbox:              sandbox.Repository{URL: *sandboxURL, Branch: *sandboxBranch},
		AllowedNamespaces:    splitList(*patchable),
		AlertNamespaces:      allowed,
		WebhookPort:          *webhookPort,
		WebhookCertDir:       *webhookCertDir,
		HealthProbeAddress:   *healthBind,
	})
	if err != nil {
		return err
	}
	ctrl.Log.WithName("setup").Info("starting pagerless-server", "namespace", *namespace, "alertNamespaces", allowed)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}

// agentRuntimeFromEnvironment copies the non-secret configuration supplied by the bootstrap.
// Provider and MCP credentials are referenced directly by agent Jobs, not read by the server.
func agentRuntimeFromEnvironment() controller.AgentRuntimeConfig {
	return controller.AgentRuntimeConfig{
		Provider:          os.Getenv(agentapi.EnvModelProvider),
		KubernetesURL:     os.Getenv(agentapi.EnvMCPKubernetesURL),
		GrafanaURL:        os.Getenv(agentapi.EnvMCPGrafanaURL),
		ArgoCDURL:         os.Getenv(agentapi.EnvMCPArgoCDURL),
		GitHubURL:         os.Getenv(agentapi.EnvMCPGitHubURL),
		ReplayFile:        os.Getenv(agentapi.EnvReplayFile),
		RecordingFile:     os.Getenv(agentapi.EnvRecordingFile),
		SandboxOwner:      os.Getenv(agentapi.EnvSandboxOwner),
		SandboxRepo:       os.Getenv(agentapi.EnvSandboxRepo),
		ArgoCDApplication: os.Getenv(agentapi.EnvArgoCDApplication),
		AllowedNamespaces: os.Getenv(agentapi.EnvAllowedNamespaces),
		AWSRegion:         os.Getenv("AWS_REGION"),
	}
}

// parseNamespaces splits the comma-separated list of alert namespaces and
// checks that each one is a valid namespace name.
func parseNamespaces(list string) ([]string, error) {
	var namespaces []string
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return nil, fmt.Errorf("alert namespace %q: %s", name, strings.Join(errs, "; "))
		}
		namespaces = append(namespaces, name)
	}
	return namespaces, nil
}
