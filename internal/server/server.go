// Package server assembles pagerless-server: the Triage, TriageApproval, execution, and verification reconcilers, the
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	webassets "github.com/a-hagi613/pagerless/dashboard"
	admissionpkg "github.com/a-hagi613/pagerless/internal/admission"
	"github.com/a-hagi613/pagerless/internal/alerts"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/dashboardapi"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/history"
	"github.com/a-hagi613/pagerless/internal/metrics"
	"github.com/a-hagi613/pagerless/internal/patchresources"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/remediation"
	"github.com/a-hagi613/pagerless/internal/reportapi"
	"github.com/a-hagi613/pagerless/internal/rollback"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/scale"
	"github.com/a-hagi613/pagerless/internal/triage"
	"github.com/a-hagi613/pagerless/internal/verification"
)

const (
	// DefaultAgentServiceAccount is the read-only service account the agent runs as.
	DefaultAgentServiceAccount = "pagerless-agent"
	// DefaultServerServiceAccount is pagerless-server's own service account.
	DefaultServerServiceAccount = "pagerless-server"
	// DefaultAgentDeadline is the agent Job's active deadline: the five-minute budget of ADR-0010.
	DefaultAgentDeadline = 5 * time.Minute
	// DefaultNarrativeDeadline is the Narrative Job's active deadline: one short model call, not an investigation.
	DefaultNarrativeDeadline = 2 * time.Minute
	// DefaultMetricsDashboardUID is the Grafana dashboard that charts a Workload's metrics.
	DefaultMetricsDashboardUID = dashboardapi.DefaultMetricsDashboardUID
	// DefaultLogsDashboardUID is the Grafana dashboard that shows a Workload's logs.
	DefaultLogsDashboardUID = dashboardapi.DefaultLogsDashboardUID
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
	// AgentRuntime configures the model provider and read-only MCP services of agent Jobs.
	AgentRuntime controller.AgentRuntimeConfig
	// ReportListener is the listener the report API serves on. It is required.
	ReportListener net.Listener
	// MetricsListener is the listener the Prometheus metrics API serves on. It is required.
	MetricsListener net.Listener
	// DashboardListener is the listener the dashboard and its read-only API serve on. It is required.
	DashboardListener net.Listener
	// DashboardAuth authenticates dashboard users and serves the sign-in routes. It is required.
	DashboardAuth dashboardapi.Auth
	// WorkloadNamespaces are the namespaces whose Workloads Start Triage offers.
	WorkloadNamespaces []string
	// Approvers answers whether a user that pagerless-server asserts may approve. An asserted approver is
	// refused when it is nil.
	Approvers admissionpkg.ApproverGroups
	// GrafanaURL is the Grafana base URL that the browser can reach, for example http://localhost:3000 while the
	// Grafana Service is port-forwarded. Empty disables the embedded Grafana panels.
	GrafanaURL string
	// GrafanaMetricsDashboardUID is the Grafana dashboard that charts the Workload's metrics.
	// It defaults to dashboardapi.DefaultMetricsDashboardUID.
	GrafanaMetricsDashboardUID string
	// GrafanaLogsDashboardUID is the Grafana dashboard that shows the Workload's logs.
	// It defaults to dashboardapi.DefaultLogsDashboardUID.
	GrafanaLogsDashboardUID string
	// HistoryPath is the SQLite file that records Triage snapshots and steps. It is required.
	HistoryPath string
	// ExecutorURL is the base URL of pagerless-executor. It is required.
	ExecutorURL string
	// ExecutorToken returns the projected service account token, with the audience
	// executorapi.Audience, that authenticates the server to the Executor. It is required.
	ExecutorToken func() (string, error)
	// ArgoCDURL is the base URL of the Argo CD API server, which Verification asks whether the merged change is
	// synced. It is required.
	ArgoCDURL string
	// ArgoCDToken returns the bearer token that authenticates to the Argo CD API. It is required.
	ArgoCDToken func() (string, error)
	// AlertmanagerURL is the base URL of Alertmanager, which Verification asks whether the triggering alert has
	// resolved. It is required.
	AlertmanagerURL string
	// AlertmanagerToken reads the bearer token required by the alert receiver. It is required.
	AlertmanagerToken func() (string, error)

	// AgentServiceAccount defaults to DefaultAgentServiceAccount.
	AgentServiceAccount string
	// ServerServiceAccount defaults to DefaultServerServiceAccount.
	ServerServiceAccount string
	// AgentDeadline defaults to DefaultAgentDeadline.
	AgentDeadline time.Duration
	// NarrativeDeadline is the Narrative Job's active deadline. It defaults to DefaultNarrativeDeadline.
	NarrativeDeadline time.Duration
	// AlertNamespaces are the namespaces whose opted-in alerts may start Triages. Empty accepts none.
	AlertNamespaces []string
	// Clock defaults to the real clock.
	Clock clock.Clock
	// Validators are the Actions that Validation accepts, which replaces the
	// defaults below. When it is nil, Validation accepts NoAction and
	// RestartWorkload, and also Scale, PatchResources, and Rollback if Sandbox is set.
	Validators map[v1alpha1.Action]proposal.ActionValidator
	// Sandbox is the repository that the pull request Actions are rendered against.
	Sandbox sandbox.Repository
	// AllowedNamespaces are the namespaces whose Workloads the executable Actions may change.
	AllowedNamespaces []string
	// PatchBounds are the permitted ranges of PatchResources requests and limits.
	// It defaults to patchresources.DefaultBounds.
	PatchBounds map[corev1.ResourceName]patchresources.Bounds
	// ScaleBounds are the permitted replica counts of the Scale Action.
	// It defaults to scale.DefaultBounds.
	ScaleBounds scale.Bounds
	// ProposalPollInterval is how often a Proposed Triage is checked for a Stale Proposal.
	// It defaults to controller.DefaultProposalPollInterval.
	ProposalPollInterval time.Duration
	// ExecutionPollInterval is how often the Executor is asked whether an open pull request has merged.
	// It defaults to controller.DefaultExecutionPollInterval.
	ExecutionPollInterval time.Duration
	// ArgoCDApplication is the Argo CD application that deploys the Sandbox. It defaults to
	// verification.DefaultApplication.
	ArgoCDApplication string
	// VerificationWindow is how long Verification observes the Workload for new restarts and OOMKills.
	// It defaults to verification.DefaultWindow, five minutes.
	VerificationWindow time.Duration
	// VerificationPollInterval is how often Verification checks a Triage.
	// It defaults to controller.DefaultVerificationPollInterval.
	VerificationPollInterval time.Duration
	// Retention is how long a terminal Triage is kept before it is deleted.
	// It defaults to controller.DefaultRetention, seven days.
	Retention time.Duration
	// RetentionPollInterval bounds how long the retention controller waits before it checks a terminal Triage
	// again. It defaults to controller.DefaultRetentionPollInterval.
	RetentionPollInterval time.Duration

	// WebhookHost, WebhookPort, and WebhookCertDir configure the admission webhook server.
	WebhookHost    string
	WebhookPort    int
	WebhookCertDir string
	// WebhookListener is an already-bound listener that the webhook server
	// serves on. When it is set, the webhook server does not bind WebhookHost
	// and WebhookPort when the manager starts; the listener carries the port
	// for the whole serving span, so nothing can take it in between.
	WebhookListener net.Listener
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
	case c.ArgoCDURL == "":
		return errors.New("an Argo CD URL is required")
	case c.ArgoCDToken == nil:
		return errors.New("an Argo CD token source is required")
	case c.AlertmanagerURL == "":
		return errors.New("an Alertmanager URL is required")
	case c.AlertmanagerToken == nil:
		return errors.New("an Alertmanager token source is required")
	case c.WebhookListener != nil && c.WebhookCertDir == "":
		return errors.New("a webhook certificate directory is required when a webhook listener is given")
	}
	if err := c.AgentRuntime.ValidateRecording(); err != nil {
		return err
	}
	switch {
	case c.HistoryPath == "":
		return errors.New("a history path is required")
	case c.MetricsListener == nil:
		return errors.New("a metrics listener is required")
	case c.DashboardListener == nil:
		return errors.New("a dashboard listener is required")
	case c.DashboardAuth == nil:
		return errors.New("dashboard authentication is required")
	}
	// A Job cannot authenticate to Keycloak or reach the MCP gateway without this.
	return c.AgentRuntime.ValidateGateway()
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
	if c.NarrativeDeadline == 0 {
		c.NarrativeDeadline = DefaultNarrativeDeadline
	}
	if c.Clock == nil {
		c.Clock = clock.RealClock{}
	}
	if c.PatchBounds == nil {
		c.PatchBounds = patchresources.DefaultBounds()
	}
	if c.ScaleBounds == (scale.Bounds{}) {
		c.ScaleBounds = scale.DefaultBounds()
	}
	if c.ProposalPollInterval == 0 {
		c.ProposalPollInterval = controller.DefaultProposalPollInterval
	}
	if c.ExecutionPollInterval == 0 {
		c.ExecutionPollInterval = controller.DefaultExecutionPollInterval
	}
	if c.ArgoCDApplication == "" {
		c.ArgoCDApplication = verification.DefaultApplication
	}
	if c.VerificationWindow == 0 {
		c.VerificationWindow = verification.DefaultWindow
	}
	if c.VerificationPollInterval == 0 {
		c.VerificationPollInterval = controller.DefaultVerificationPollInterval
	}
	if c.Retention == 0 {
		c.Retention = controller.DefaultRetention
	}
	if c.RetentionPollInterval == 0 {
		c.RetentionPollInterval = controller.DefaultRetentionPollInterval
	}
	if c.GrafanaMetricsDashboardUID == "" {
		c.GrafanaMetricsDashboardUID = dashboardapi.DefaultMetricsDashboardUID
	}
	if c.GrafanaLogsDashboardUID == "" {
		c.GrafanaLogsDashboardUID = dashboardapi.DefaultLogsDashboardUID
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
		WebhookServer:          webhookServer(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	historyStore, err := history.Open(cfg.HistoryPath)
	if err != nil {
		return nil, fmt.Errorf("open the Triage history: %w", err)
	}
	if err := mgr.Add(closeHistory(historyStore)); err != nil {
		_ = historyStore.Close()
		return nil, fmt.Errorf("add the Triage history: %w", err)
	}
	metricsAPI := metrics.New(mgr.GetClient())
	broker := dashboardapi.NewBroker()
	recorder := &recorder{history: historyStore, metrics: metricsAPI, dashboard: broker}

	validators := cfg.Validators
	if validators == nil {
		validators = proposal.DefaultValidators()
		shared := remediation.Config{
			Reader:            mgr.GetAPIReader(),
			Sandbox:           cfg.Sandbox,
			AllowedNamespaces: cfg.AllowedNamespaces,
		}
		validators[v1alpha1.ActionRestartWorkload] = remediation.NewRestart(shared)
		if cfg.Sandbox.URL != "" {
			validators[v1alpha1.ActionScale] = scale.New(scale.Config{
				Reader:            mgr.GetAPIReader(),
				Writer:            mgr.GetClient(),
				Sandbox:           cfg.Sandbox,
				AllowedNamespaces: cfg.AllowedNamespaces,
				Bounds:            cfg.ScaleBounds,
			})
			validators[v1alpha1.ActionPatchResources] = patchresources.New(patchresources.Config{
				Reader:            mgr.GetAPIReader(),
				Writer:            mgr.GetClient(),
				Sandbox:           cfg.Sandbox,
				AllowedNamespaces: cfg.AllowedNamespaces,
				Bounds:            cfg.PatchBounds,
			})
			validators[v1alpha1.ActionRollback] = rollback.New(rollback.Config{
				Reader:            mgr.GetAPIReader(),
				Sandbox:           cfg.Sandbox,
				AllowedNamespaces: cfg.AllowedNamespaces,
			})
		}
	}
	proposals := proposal.NewPipeline(cfg.Clock, validators)

	store := &triage.StatusStore{
		Client:   mgr.GetClient(),
		Reader:   mgr.GetAPIReader(),
		Clock:    cfg.Clock,
		Recorder: recorder,
	}
	reconciler := &controller.TriageReconciler{
		Client: mgr.GetClient(),
		Scheme: scheme,
		Status: store,
		Job: controller.AgentJobConfig{
			Image:          cfg.AgentImage,
			ServiceAccount: cfg.AgentServiceAccount,
			ServerURL:      cfg.AgentServerURL,
			Deadline:       int64(cfg.AgentDeadline.Seconds()),
			Runtime:        cfg.AgentRuntime,
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

	verifications := &controller.VerificationReconciler{
		Client: mgr.GetClient(),
		Scheme: scheme,
		Status: store,
		Verifier: &verification.Verifier{
			Reader:       mgr.GetAPIReader(),
			Applications: &verification.ArgoCD{BaseURL: cfg.ArgoCDURL, Token: cfg.ArgoCDToken},
			Alerts:       &verification.Alertmanager{BaseURL: cfg.AlertmanagerURL, Clock: cfg.Clock},
			Application:  cfg.ArgoCDApplication,
		},
		Clock:        cfg.Clock,
		Window:       cfg.VerificationWindow,
		PollInterval: cfg.VerificationPollInterval,
		NarrativeJob: controller.AgentJobConfig{
			Image:          cfg.AgentImage,
			ServiceAccount: cfg.AgentServiceAccount,
			ServerURL:      cfg.AgentServerURL,
			Deadline:       int64(cfg.NarrativeDeadline.Seconds()),
			Runtime:        cfg.AgentRuntime,
		},
	}
	if err := verifications.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the verification controller: %w", err)
	}

	retention := &controller.RetentionReconciler{
		Client:       mgr.GetClient(),
		Reader:       mgr.GetAPIReader(),
		Clock:        cfg.Clock,
		Retention:    cfg.Retention,
		PollInterval: cfg.RetentionPollInterval,
	}
	if err := retention.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the retention controller: %w", err)
	}

	webhooks := mgr.GetWebhookServer()
	serverUsername := fmt.Sprintf("system:serviceaccount:%s:%s", cfg.Namespace, cfg.ServerServiceAccount)
	webhooks.Register(admissionpkg.CreatePath, &webhook.Admission{Handler: &admissionpkg.CreateValidator{
		Reader:    mgr.GetAPIReader(),
		Namespace: cfg.Namespace,
	}})
	webhooks.Register(admissionpkg.ApprovalStampPath, &webhook.Admission{Handler: admissionpkg.ApprovalStamper{
		ServerUsername: serverUsername,
	}})
	webhooks.Register(admissionpkg.ApprovalValidatePath, &webhook.Admission{Handler: &admissionpkg.ApprovalValidator{
		Reader:         mgr.GetAPIReader(),
		Namespace:      cfg.Namespace,
		Proposals:      proposals,
		Clock:          cfg.Clock,
		ServerUsername: serverUsername,
		Approvers:      cfg.Approvers,
	}})
	webhooks.Register(admissionpkg.StatusPath, &webhook.Admission{Handler: &admissionpkg.StatusGuard{
		ServerUsername: serverUsername,
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
		Token:      cfg.AlertmanagerToken,
	}
	mux := http.NewServeMux()
	mux.Handle(alerts.Path, receiver)
	mux.Handle("/", handler)
	if err := mgr.Add(serve("report API", cfg.ReportListener, mux)); err != nil {
		return nil, fmt.Errorf("add the report API: %w", err)
	}
	if err := mgr.Add(serve("metrics API", cfg.MetricsListener, metricsAPI.Handler())); err != nil {
		return nil, fmt.Errorf("add the metrics API: %w", err)
	}
	// The dashboard reads through the manager's client, so it inherits the server's RBAC. Its write routes run as
	// the server's service account and assert the dashboard user on the TriageApproval (ADR-0020).
	dashboard := dashboardapi.New(dashboardapi.Config{
		Namespace:          cfg.Namespace,
		Reader:             mgr.GetClient(),
		Writer:             mgr.GetClient(),
		Workloads:          mgr.GetAPIReader(),
		WorkloadNamespaces: cfg.WorkloadNamespaces,
		Auth:               cfg.DashboardAuth,
		History:            historyStore,
		Broker:             broker,
		Clock:              cfg.Clock,
		Grafana: dashboardapi.GrafanaConfig{
			BaseURL:             cfg.GrafanaURL,
			MetricsDashboardUID: cfg.GrafanaMetricsDashboardUID,
			LogsDashboardUID:    cfg.GrafanaLogsDashboardUID,
		},
		Assets: webassets.Assets(),
	})
	if err := mgr.Add(serve("dashboard", cfg.DashboardListener, dashboard)); err != nil {
		return nil, fmt.Errorf("add the dashboard: %w", err)
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

// serve returns a runnable that serves handler on listener until the manager stops.
func serve(name string, listener net.Listener, handler http.Handler) manager.Runnable {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	return manager.RunnableFunc(func(ctx context.Context) error {
		errc := make(chan error, 1)
		go func() { errc <- srv.Serve(listener) }()
		select {
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("serve the %s: %w", name, err)
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		}
	})
}

// closeHistory closes the history database when the manager stops.
func closeHistory(store *history.Store) manager.Runnable {
	return manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return store.Close()
	})
}

// recorder writes the append-only history, updates the Prometheus metrics, and fans live events out to the
// dashboard after a status write. A history or metrics failure is logged and never fails the request: the Triage
// resource is the source of truth (ADR-0001).
type recorder struct {
	history   *history.Store
	metrics   *metrics.Metrics
	dashboard *dashboardapi.Broker
}

// PhaseChanged records a Triage snapshot on every Phase change, updates the metrics, and notifies open event
// streams. A terminal Phase also ends the streams, because no further change can occur.
func (r *recorder) PhaseChanged(ctx context.Context, before, after *v1alpha1.Triage, now time.Time) {
	r.metrics.ObservePhaseChange(before, after, now)
	if before.Status.Phase == after.Status.Phase {
		return
	}
	if err := r.history.AppendSnapshot(ctx, after, now); err != nil {
		logf.FromContext(ctx).Error(err, "record the Triage snapshot", "triage", after.Name)
	}
	if r.dashboard == nil {
		return
	}
	phase := dashboardapi.PhaseEvent{Phase: after.Status.Phase, Terminal: after.Status.Phase.IsTerminal()}
	r.dashboard.Publish(after.Name, dashboardapi.EventPhase, phase)
	if phase.Terminal {
		r.dashboard.Publish(after.Name, dashboardapi.EventEnd, phase)
	}
}

// StepRecorded appends one step event to the history and sends it to open event streams.
func (r *recorder) StepRecorded(
	ctx context.Context, tr *v1alpha1.Triage, step v1alpha1.Step, finding *v1alpha1.Finding,
) {
	if err := r.history.AppendStep(ctx, tr, step, finding); err != nil {
		logf.FromContext(ctx).Error(err, "record the Triage step", "triage", tr.Name)
	}
	if r.dashboard != nil {
		r.dashboard.Publish(tr.Name, dashboardapi.EventStep, dashboardapi.StepEvent{
			Step: step, Finding: finding, StepCount: tr.Status.StepCount,
		})
	}
}
