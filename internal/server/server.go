// Package server assembles pagerless-server: the Triage, TriageApproval, and execution reconcilers, the
// agent report endpoint, and the admission webhooks, run by one manager (ADR-0005).
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	admissionpkg "github.com/a-hagi613/pagerless/internal/admission"
	"github.com/a-hagi613/pagerless/internal/alerts"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/patchresources"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/reportapi"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/triage"
)

const (
	// DefaultAgentServiceAccount is the read-only service account the agent runs as.
	DefaultAgentServiceAccount = "pagerless-agent"
	// DefaultServerServiceAccount is pagerless-server's own service account.
	DefaultServerServiceAccount = "pagerless-server"
	// DefaultAgentDeadline is the agent Job's active deadline: the five-minute budget of ADR-0010.
	DefaultAgentDeadline = 5 * time.Minute
	// shutdownTimeout bounds how long the report API drains when the server stops.
	shutdownTimeout = 10 * time.Second
	// readHeaderTimeout bounds how long a client may take to send request headers.
	readHeaderTimeout = 10 * time.Second
)

// Config configures pagerless-server.
type Config struct {
	// Namespace holds Triages, agent Jobs, and the agent service account. It is required.
	Namespace string
	// AgentImage is the agent container image. It is required.
	AgentImage string
	// AgentServerURL is the base URL that agent Jobs use to reach the report API. It is required.
	AgentServerURL string
	// ReportListener is the listener the report API serves on. It is required.
	ReportListener net.Listener
	// ExecutorURL is the base URL of pagerless-executor. It is required.
	ExecutorURL string
	// ExecutorToken returns the projected service account token, with the audience
	// executorapi.Audience, that authenticates the server to the Executor. It is required.
	ExecutorToken func() (string, error)

	// AgentServiceAccount defaults to DefaultAgentServiceAccount.
	AgentServiceAccount string
	// ServerServiceAccount defaults to DefaultServerServiceAccount.
	ServerServiceAccount string
	// AgentDeadline defaults to DefaultAgentDeadline.
	AgentDeadline time.Duration
	// AlertNamespaces are the namespaces whose opted-in alerts may start Triages. Empty accepts none.
	AlertNamespaces []string
	// Clock defaults to the real clock.
	Clock clock.Clock
	// Validators are the Actions that Validation accepts, which replaces the
	// defaults below. When it is nil, Validation accepts NoAction, and also
	// PatchResources if Sandbox is set.
	Validators map[v1alpha1.Action]proposal.ActionValidator
	// Sandbox is the repository that PatchResources Proposals are rendered against.
	Sandbox sandbox.Repository
	// AllowedNamespaces are the namespaces whose Workloads PatchResources may change.
	AllowedNamespaces []string
	// PatchBounds are the permitted ranges of PatchResources requests and limits.
	// It defaults to patchresources.DefaultBounds.
	PatchBounds map[corev1.ResourceName]patchresources.Bounds
	// ProposalPollInterval is how often a Proposed Triage is checked for a Stale Proposal.
	// It defaults to controller.DefaultProposalPollInterval.
	ProposalPollInterval time.Duration
	// ExecutionPollInterval is how often the Executor is asked whether an open pull request has merged.
	// It defaults to controller.DefaultExecutionPollInterval.
	ExecutionPollInterval time.Duration

	// WebhookHost, WebhookPort, and WebhookCertDir configure the admission webhook server.
	WebhookHost    string
	WebhookPort    int
	WebhookCertDir string
	// HealthProbeAddress is the address of the health probes, or empty to disable them.
	HealthProbeAddress string
}

// Scheme returns the scheme that pagerless-server uses.
func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register pagerless.dev types: %w", err)
	}
	return scheme, nil
}

func (c Config) validate() error {
	switch {
	case c.Namespace == "":
		return errors.New("namespace is required")
	case c.AgentImage == "":
		return errors.New("agent image is required")
	case c.AgentServerURL == "":
		return errors.New("agent server URL is required")
	case c.ReportListener == nil:
		return errors.New("report listener is required")
	case c.ExecutorURL == "":
		return errors.New("executor URL is required")
	case c.ExecutorToken == nil:
		return errors.New("executor token source is required")
	}
	return nil
}

func (c Config) withDefaults() Config {
	if c.AgentServiceAccount == "" {
		c.AgentServiceAccount = DefaultAgentServiceAccount
	}
	if c.ServerServiceAccount == "" {
		c.ServerServiceAccount = DefaultServerServiceAccount
	}
	if c.AgentDeadline == 0 {
		c.AgentDeadline = DefaultAgentDeadline
	}
	if c.Clock == nil {
		c.Clock = clock.RealClock{}
	}
	if c.PatchBounds == nil {
		c.PatchBounds = patchresources.DefaultBounds()
	}
	if c.ProposalPollInterval == 0 {
		c.ProposalPollInterval = controller.DefaultProposalPollInterval
	}
	if c.ExecutionPollInterval == 0 {
		c.ExecutionPollInterval = controller.DefaultExecutionPollInterval
	}
	return c
}

// New builds the manager that runs pagerless-server against the cluster that restConfig points at.
func New(restConfig *rest.Config, cfg Config) (ctrl.Manager, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	cfg = cfg.withDefaults()
	scheme, err := Scheme()
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: cfg.HealthProbeAddress,
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}}},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    cfg.WebhookHost,
			Port:    cfg.WebhookPort,
			CertDir: cfg.WebhookCertDir,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	validators := cfg.Validators
	if validators == nil {
		validators = proposal.DefaultValidators()
		if cfg.Sandbox.URL != "" {
			validators[v1alpha1.ActionPatchResources] = patchresources.New(patchresources.Config{
				Reader:            mgr.GetAPIReader(),
				Writer:            mgr.GetClient(),
				Sandbox:           cfg.Sandbox,
				AllowedNamespaces: cfg.AllowedNamespaces,
				Bounds:            cfg.PatchBounds,
			})
		}
	}
	proposals := proposal.NewPipeline(cfg.Clock, validators)

	store := &triage.StatusStore{Client: mgr.GetClient(), Reader: mgr.GetAPIReader()}
	reconciler := &controller.TriageReconciler{
		Client: mgr.GetClient(),
		Scheme: scheme,
		Status: store,
		Job: controller.AgentJobConfig{
			Image:          cfg.AgentImage,
			ServiceAccount: cfg.AgentServiceAccount,
			ServerURL:      cfg.AgentServerURL,
			Deadline:       int64(cfg.AgentDeadline.Seconds()),
		},
		Proposals:    proposals,
		Clock:        cfg.Clock,
		PollInterval: cfg.ProposalPollInterval,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the Triage controller: %w", err)
	}

	approvals := &controller.ApprovalReconciler{
		Client:    mgr.GetClient(),
		Status:    store,
		Proposals: proposals,
		Clock:     cfg.Clock,
	}
	if err := approvals.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the TriageApproval controller: %w", err)
	}

	executions := &controller.ExecutionReconciler{
		Client:       mgr.GetClient(),
		Status:       store,
		Executor:     &executorapi.Client{BaseURL: cfg.ExecutorURL, Token: cfg.ExecutorToken},
		Clock:        cfg.Clock,
		PollInterval: cfg.ExecutionPollInterval,
	}
	if err := executions.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the execution controller: %w", err)
	}

	webhooks := mgr.GetWebhookServer()
	webhooks.Register(admissionpkg.CreatePath, &webhook.Admission{Handler: &admissionpkg.CreateValidator{
		Reader:    mgr.GetAPIReader(),
		Namespace: cfg.Namespace,
	}})
	webhooks.Register(admissionpkg.ApprovalStampPath, &webhook.Admission{Handler: admissionpkg.ApprovalStamper{}})
	webhooks.Register(admissionpkg.ApprovalValidatePath, &webhook.Admission{Handler: &admissionpkg.ApprovalValidator{
		Reader:    mgr.GetAPIReader(),
		Namespace: cfg.Namespace,
		Proposals: proposals,
		Clock:     cfg.Clock,
	}})
	webhooks.Register(admissionpkg.StatusPath, &webhook.Admission{Handler: &admissionpkg.StatusGuard{
		ServerUsername: fmt.Sprintf("system:serviceaccount:%s:%s", cfg.Namespace, cfg.ServerServiceAccount),
	}})

	handler := &reportapi.Handler{
		Auth: &reportapi.Authenticator{
			Client:         mgr.GetClient(),
			Reader:         mgr.GetAPIReader(),
			Namespace:      cfg.Namespace,
			ServiceAccount: cfg.AgentServiceAccount,
		},
		Status:    store,
		Proposals: proposals,
		Clock:     cfg.Clock,
	}
	receiver := &alerts.Receiver{
		Client:     mgr.GetClient(),
		Store:      store,
		Namespace:  cfg.Namespace,
		Namespaces: cfg.AlertNamespaces,
	}
	mux := http.NewServeMux()
	mux.Handle(alerts.Path, receiver)
	mux.Handle("/", handler)
	if err := mgr.Add(reportAPI(cfg.ReportListener, mux)); err != nil {
		return nil, fmt.Errorf("add the report API: %w", err)
	}

	if cfg.HealthProbeAddress != "" {
		if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
			return nil, fmt.Errorf("add health check: %w", err)
		}
		if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
			return nil, fmt.Errorf("add readiness check: %w", err)
		}
	}
	return mgr, nil
}

// reportAPI returns a runnable that serves the agent API until the manager stops.
func reportAPI(listener net.Listener, handler http.Handler) manager.Runnable {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	return manager.RunnableFunc(func(ctx context.Context) error {
		errc := make(chan error, 1)
		go func() { errc <- srv.Serve(listener) }()
		select {
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("serve the report API: %w", err)
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		}
	})
}
