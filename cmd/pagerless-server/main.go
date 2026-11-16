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

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/webhooktls"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pagerless-server:", err)
		os.Exit(1)
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
		agentAccount = flag.String("agent-service-account", server.DefaultAgentServiceAccount,
			"service account that agent Jobs run as")
		serverAccount = flag.String("server-service-account", server.DefaultServerServiceAccount,
			"service account that pagerless-server runs as")
		agentDeadline = flag.Duration("agent-deadline", server.DefaultAgentDeadline, "active deadline of an agent Job")
		sandboxURL    = flag.String("sandbox-url", "",
			"git URL of the Sandbox; PatchResources is accepted only when it is set")
		sandboxBranch = flag.String("sandbox-branch", "main", "branch of the Sandbox that Argo CD deploys")
		patchable     = flag.String("patch-namespaces", "",
			"comma-separated namespaces whose Workloads PatchResources may change")
	)
	zapOptions := zap.Options{}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

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
		ServiceName: *webhookService,
		Namespace:   *namespace,
		ConfigName:  *webhookConfig,
		CertDir:     *webhookCertDir,
		Now:         time.Now(),
	}); err != nil {
		return fmt.Errorf("provision webhook TLS: %w", err)
	}

	listener, err := net.Listen("tcp", *reportBind)
	if err != nil {
		return fmt.Errorf("listen for the report API: %w", err)
	}
	mgr, err := server.New(restConfig, server.Config{
		Namespace:            *namespace,
		AgentImage:           *agentImage,
		AgentServerURL:       *reportURL,
		ReportListener:       listener,
		AgentServiceAccount:  *agentAccount,
		ServerServiceAccount: *serverAccount,
		AgentDeadline:        *agentDeadline,
		Sandbox:              sandbox.Repository{URL: *sandboxURL, Branch: *sandboxBranch},
		AllowedNamespaces:    splitList(*patchable),
		WebhookPort:          *webhookPort,
		WebhookCertDir:       *webhookCertDir,
		HealthProbeAddress:   *healthBind,
	})
	if err != nil {
		return err
	}
	ctrl.Log.WithName("setup").Info("starting pagerless-server", "namespace", *namespace)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}
