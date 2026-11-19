// Package server assembles pagerless-server: the Page, pull request, and
// verification reconcilers, the agent report endpoint, and the dashboard API,
// run by one manager (ADR-0030). The standalone chart installs no admission
// webhook, so no webhook server is configured.
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	webassets "github.com/a-hagi613/pagerless/dashboard"
	"github.com/a-hagi613/pagerless/internal/alerts"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/credentials"
	"github.com/a-hagi613/pagerless/internal/dashboardapi"
	"github.com/a-hagi613/pagerless/internal/dashboardauth"
	"github.com/a-hagi613/pagerless/internal/github"
	"github.com/a-hagi613/pagerless/internal/history"
	"github.com/a-hagi613/pagerless/internal/metrics"
	"github.com/a-hagi613/pagerless/internal/page"
	"github.com/a-hagi613/pagerless/internal/patchresources"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/publish"
	"github.com/a-hagi613/pagerless/internal/reportapi"
	"github.com/a-hagi613/pagerless/internal/rollback"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/scale"
	"github.com/a-hagi613/pagerless/internal/verification"
	"github.com/a-hagi613/pagerless/internal/watcher"
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
	// DefaultMetricsDashboardUID is the Grafana dashboard that charts a Target's metrics.
	DefaultMetricsDashboardUID = dashboardapi.DefaultMetricsDashboardUID
	// DefaultLogsDashboardUID is the Grafana dashboard that shows a Target's logs.
	DefaultLogsDashboardUID = dashboardapi.DefaultLogsDashboardUID
	// shutdownTimeout bounds how long the report API drains when the server stops.
	shutdownTimeout = 10 * time.Second
	// readHeaderTimeout bounds how long a client may take to send request headers.
	readHeaderTimeout = 10 * time.Second
)

// Config configures pagerless-server.
type Config struct {
	// Namespace holds Pages, agent Jobs, and the agent service account. It is required.
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
	// DashboardListener is the listener the dashboard and its read-only API serve on.
	// The dashboard is served only when it is set together with DashboardAuth.
	DashboardListener net.Listener
	// DashboardAuth authenticates dashboard users and serves the sign-in routes. When
	// it is nil the dashboard is not served.
	DashboardAuth dashboardapi.Auth
	// DashboardAuthConfig builds DashboardAuth inside the manager, so that session
	// revocation is recorded in the Page history's SQLite file. It is used only when
	// DashboardAuth is nil.
	DashboardAuthConfig *dashboardauth.Config
	// TargetNamespaces are the namespaces whose Targets the dashboard's Start request offers.
	TargetNamespaces []string
	// GrafanaURL is the Grafana base URL that the browser can reach, for example http://localhost:3000 while the
	// Grafana Service is port-forwarded. Empty disables the embedded Grafana panels.
	GrafanaURL string
	// GrafanaMetricsDashboardUID is the Grafana dashboard that charts the Target's metrics.
	// It defaults to dashboardapi.DefaultMetricsDashboardUID.
	GrafanaMetricsDashboardUID string
	// GrafanaLogsDashboardUID is the Grafana dashboard that shows the Target's logs.
	// It defaults to dashboardapi.DefaultLogsDashboardUID.
	GrafanaLogsDashboardUID string
	// HistoryPath is the SQLite file that records Page snapshots and steps. It is required.
	HistoryPath string
	// ArgoCDURL is the base URL of the Argo CD API server, which Verification asks
	// whether the merged change is synced. Empty skips the GitOps check.
	ArgoCDURL string
	// ArgoCDToken returns the bearer token that authenticates to the Argo CD API.
	ArgoCDToken func() (string, error)
	// AlertmanagerURL is the base URL of Alertmanager. Empty disables both the
	// inbound alert receiver and the alert-resolution check.
	AlertmanagerURL string
	// AlertmanagerToken reads the bearer token required by the alert receiver.
	AlertmanagerToken func() (string, error)

	// AgentServiceAccount defaults to DefaultAgentServiceAccount.
	AgentServiceAccount string
	// ServerServiceAccount defaults to DefaultServerServiceAccount.
	ServerServiceAccount string
	// AgentDeadline defaults to DefaultAgentDeadline.
	AgentDeadline time.Duration
	// NarrativeDeadline is the Narrative Job's active deadline. It defaults to DefaultNarrativeDeadline.
	NarrativeDeadline time.Duration
	// AlertNamespaces are the namespaces whose opted-in alerts may open Pages. Empty accepts none.
	AlertNamespaces []string
	// Clock defaults to the real clock.
	Clock clock.Clock
	// Validators are the Actions that Validation accepts, which replaces the
	// defaults below. When it is nil, Validation accepts NoAction, and also
	// Scale, PatchResources, and Rollback if Sandbox is set.
	Validators map[v1alpha1.Action]proposal.ActionValidator
	// Sandbox is the repository that the pull request Actions are rendered against and published to.
	Sandbox sandbox.Repository
	// GitHubAPIURL is the base URL of the target repository's GitHub API. It defaults to
	// github.DefaultGitHubAPI.
	GitHubAPIURL string
	// GitHubRepository is the target repository's owner/name. It is required when Sandbox is set.
	GitHubRepository string
	// GitHubToken returns the write-scoped token that publishes a Proposal. It is required when Sandbox is set.
	GitHubToken func() (string, error)
	// AllowedNamespaces are the namespaces whose Targets the executable Actions may change.
	AllowedNamespaces []string
	// PatchBounds are the permitted ranges of PatchResources requests and limits.
	// It defaults to patchresources.DefaultBounds.
	PatchBounds map[corev1.ResourceName]patchresources.Bounds
	// ScaleBounds are the permitted replica counts of the Scale Action.
	// It defaults to scale.DefaultBounds.
	ScaleBounds scale.Bounds
	// ProposalPollInterval is how often a Proposed Page is checked for a Stale Proposal.
	// It defaults to controller.DefaultProposalPollInterval.
	ProposalPollInterval time.Duration
	// PullRequestPollInterval is how often an open pull request is checked for a merge.
	// It defaults to controller.DefaultPullRequestPollInterval.
	PullRequestPollInterval time.Duration
	// ArgoCDApplication is the Argo CD application that deploys the Target Repository. It defaults to
	// verification.DefaultApplication.
	ArgoCDApplication string
	// VerificationWindow is how long Verification observes the Target for new restarts and OOMKills.
	// It defaults to verification.DefaultWindow, five minutes.
	VerificationWindow time.Duration
	// VerificationPollInterval is how often Verification checks a Page.
	// It defaults to controller.DefaultVerificationPollInterval.
	VerificationPollInterval time.Duration
	// Retention is how long a terminal Page is kept before it is deleted.
	// It defaults to controller.DefaultRetention, seven days.
	Retention time.Duration
	// RetentionPollInterval bounds how long the retention controller waits before it checks a terminal Page
	// again. It defaults to controller.DefaultRetentionPollInterval.
	RetentionPollInterval time.Duration
	// HealthProbeAddress is the address of the health probes, or empty to disable them.
	HealthProbeAddress string
	// Watcher enables the cluster watcher. A nil Watcher leaves the server a pure
	// Page controller, which is what the unit suites want; pagerless-server always
	// configures it.
	Watcher *watcher.Config
	// CredentialsSecretName is the credentials Secret the settings route reads and writes.
	// It defaults to pagerless-credentials.
	CredentialsSecretName string
	// CredentialsExternalManager is the secret manager that owns the credentials Secret.
	// Empty means Pagerless owns it and may write; otherwise the Portal and the CLI
	// report the settings as read-only.
	CredentialsExternalManager string
	// CredentialsMountPath is where the credentials Secret is mounted, used to read
	// an externally managed Secret without Secret RBAC.
	CredentialsMountPath string
	// Version is the release version that the Portal reports. It defaults to "dev".
	Version string
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
	}
	if err := c.AgentRuntime.ValidateRecording(); err != nil {
		return err
	}
	switch {
	case c.HistoryPath == "":
		return errors.New("a history path is required")
	case c.MetricsListener == nil:
		return errors.New("a metrics listener is required")
	}
	if c.DashboardListener != nil && c.DashboardAuth == nil && c.DashboardAuthConfig == nil {
		return errors.New("dashboard authentication is required when a dashboard listener is given")
	}
	if c.ArgoCDURL != "" && c.ArgoCDToken == nil {
		return errors.New("an Argo CD token source is required when an Argo CD URL is given")
	}
	if c.AlertmanagerURL != "" && c.AlertmanagerToken == nil {
		return errors.New("an Alertmanager token source is required when an Alertmanager URL is given")
	}
	if c.Sandbox.URL != "" {
		switch {
		case c.GitHubRepository == "":
			return errors.New("a GitHub repository is required when a Sandbox is configured")
		case c.GitHubToken == nil:
			return errors.New("a GitHub token source is required when a Sandbox is configured")
		}
	}
	// A Job cannot authenticate to the model provider without this.
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
	if c.PullRequestPollInterval == 0 {
		c.PullRequestPollInterval = controller.DefaultPullRequestPollInterval
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
	if c.CredentialsSecretName == "" {
		c.CredentialsSecretName = "pagerless-credentials"
	}
	if c.Version == "" {
		c.Version = "dev"
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
		HealthProbeBindAddress: "",
		Cache:                  cacheOptions(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	credentialsStore, err := credentials.New(credentials.Config{
		Client:          mgr.GetClient(),
		Namespace:       cfg.Namespace,
		SecretName:      cfg.CredentialsSecretName,
		ExternalManager: cfg.CredentialsExternalManager,
		MountPath:       cfg.CredentialsMountPath,
	})
	if err != nil {
		return nil, fmt.Errorf("configure the credentials store: %w", err)
	}

	historyStore, err := history.Open(cfg.HistoryPath)
	if err != nil {
		return nil, fmt.Errorf("open the Page history: %w", err)
	}
	if err := mgr.Add(closeHistory(historyStore)); err != nil {
		_ = historyStore.Close()
		return nil, fmt.Errorf("add the Page history: %w", err)
	}
	dashboardAuth := cfg.DashboardAuth
	if dashboardAuth == nil && cfg.DashboardAuthConfig != nil {
		authConfig := *cfg.DashboardAuthConfig
		authConfig.Revocations = historyStore
		dashboardAuth, err = dashboardauth.New(authConfig)
		if err != nil {
			_ = historyStore.Close()
			return nil, fmt.Errorf("configure Portal authentication: %w", err)
		}
	}
	metricsAPI := metrics.New(mgr.GetClient())
	broker := dashboardapi.NewBroker()
	recorder := &recorder{history: historyStore, metrics: metricsAPI, dashboard: broker}

	validators := cfg.Validators
	if validators == nil {
		validators = proposal.DefaultValidators()
		if cfg.Sandbox.URL != "" {
			validators[v1alpha1.ActionScale] = scale.New(scale.Config{
				Reader:            mgr.GetAPIReader(),
				Sandbox:           cfg.Sandbox,
				AllowedNamespaces: cfg.AllowedNamespaces,
				Bounds:            cfg.ScaleBounds,
			})
			validators[v1alpha1.ActionPatchResources] = patchresources.New(patchresources.Config{
				Reader:            mgr.GetAPIReader(),
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

	store := &page.StatusStore{
		Client:   mgr.GetClient(),
		Reader:   mgr.GetAPIReader(),
		Clock:    cfg.Clock,
		Recorder: recorder,
	}
	if cfg.Watcher != nil {
		clusterWatcher, err := watcher.New(mgr, historyStore.DB(), *cfg.Watcher)
		if err != nil {
			return nil, fmt.Errorf("create the cluster watcher: %w", err)
		}
		if err := clusterWatcher.Setup(mgr, store); err != nil {
			return nil, fmt.Errorf("set up the cluster watcher: %w", err)
		}
	}
	pages := &controller.PageReconciler{
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
	if err := pages.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("set up the Page controller: %w", err)
	}

	if cfg.Sandbox.URL != "" {
		pullRequests := &controller.PullRequestReconciler{
			Client: mgr.GetClient(),
			Status: store,
			Clock:  cfg.Clock,
			Publisher: publish.New(publish.Config{
				Sandbox: cfg.Sandbox,
				GitHub: &github.GitHub{
					APIURL:     cfg.GitHubAPIURL,
					Repository: cfg.GitHubRepository,
					Token:      cfg.GitHubToken,
				},
				Token: cfg.GitHubToken,
			}),
			PollInterval: cfg.PullRequestPollInterval,
		}
		if err := pullRequests.SetupWithManager(mgr); err != nil {
			return nil, fmt.Errorf("set up the pull request controller: %w", err)
		}
	}

	verifier := &verification.Verifier{
		Reader:      mgr.GetAPIReader(),
		Application: cfg.ArgoCDApplication,
	}
	if cfg.ArgoCDURL != "" {
		verifier.Applications = &verification.ArgoCD{BaseURL: cfg.ArgoCDURL, Token: cfg.ArgoCDToken}
	}
	if cfg.AlertmanagerURL != "" {
		verifier.Alerts = &verification.Alertmanager{BaseURL: cfg.AlertmanagerURL, Clock: cfg.Clock}
	}
	verifications := &controller.VerificationReconciler{
		Client:       mgr.GetClient(),
		Scheme:       scheme,
		Status:       store,
		Verifier:     verifier,
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
	mux := http.NewServeMux()
	if cfg.AlertmanagerURL != "" {
		receiver := &alerts.Receiver{
			Client:     mgr.GetClient(),
			Store:      store,
			Namespace:  cfg.Namespace,
			Namespaces: cfg.AlertNamespaces,
			Token:      cfg.AlertmanagerToken,
		}
		mux.Handle(alerts.Path, receiver)
	}
	mux.Handle("/", handler)
	if err := mgr.Add(serve("report API", cfg.ReportListener, mux)); err != nil {
		return nil, fmt.Errorf("add the report API: %w", err)
	}
	if err := mgr.Add(serve("metrics API", cfg.MetricsListener, metricsAPI.Handler())); err != nil {
		return nil, fmt.Errorf("add the metrics API: %w", err)
	}
	if cfg.DashboardListener != nil {
		// The dashboard reads through the manager's client, so it inherits the server's
		// RBAC. Its decision route patches the Page's decision annotations, which the
		// Page controller reconciles into status (ADR-0031).
		dashboard := dashboardapi.New(dashboardapi.Config{
			Namespace:        cfg.Namespace,
			Reader:           mgr.GetClient(),
			Writer:           mgr.GetClient(),
			Targets:          mgr.GetAPIReader(),
			TargetNamespaces: cfg.TargetNamespaces,
			Auth:             dashboardAuth,
			History:          historyStore,
			Broker:           broker,
			Clock:            cfg.Clock,
			Grafana: dashboardapi.GrafanaConfig{
				BaseURL:             cfg.GrafanaURL,
				MetricsDashboardUID: cfg.GrafanaMetricsDashboardUID,
				LogsDashboardUID:    cfg.GrafanaLogsDashboardUID,
			},
			Credentials: credentialsStore,
			Assets:      webassets.Assets(),
			Version:     cfg.Version,
		})
		if err := mgr.Add(serve("dashboard", cfg.DashboardListener, dashboard)); err != nil {
			return nil, fmt.Errorf("add the dashboard: %w", err)
		}
	}

	if cfg.HealthProbeAddress != "" {
		healthListener, err := net.Listen("tcp", cfg.HealthProbeAddress)
		if err != nil {
			return nil, fmt.Errorf("listen for health probes: %w", err)
		}
		probes := readiness{Credentials: credentialsStore}
		if err := mgr.Add(serveHealth(healthListener, probes.handler())); err != nil {
			return nil, fmt.Errorf("add the health probes: %w", err)
		}
	}
	return mgr, nil
}

// cacheOptions returns the manager's cache configuration. Pages and the objects
// the page lifecycle reads stay in the release namespace, while the watcher's
// observed kinds are cached across the namespaces the installation may read: a
// watcher scoped to its own namespace would see no workload worth paging on.
func cacheOptions(cfg Config) cache.Options {
	options := cache.Options{DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}}}
	if cfg.Watcher == nil {
		return options
	}
	options.ByObject = watcher.CacheByObject(cfg.Watcher.IncludeNamespaces, cfg.Watcher.NodePagesMode)
	return options
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
// dashboard after a status write. A history or metrics failure is logged and never fails the request: the Page
// resource is the source of truth (ADR-0001).
type recorder struct {
	history   *history.Store
	metrics   *metrics.Metrics
	dashboard *dashboardapi.Broker
}

// PhaseChanged records a Page snapshot on every Phase change, updates the metrics, and notifies open event
// streams. A terminal Phase also ends the streams, because no further change can occur.
func (r *recorder) PhaseChanged(ctx context.Context, before, after *v1alpha1.Page, now time.Time) {
	r.metrics.ObservePhaseChange(before, after, now)
	if before.Status.Phase == after.Status.Phase {
		return
	}
	if err := r.history.AppendSnapshot(ctx, after, now); err != nil {
		logf.FromContext(ctx).Error(err, "record the Page snapshot", "page", after.Name)
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
	ctx context.Context, p *v1alpha1.Page, step v1alpha1.Step, finding *v1alpha1.Finding,
) {
	if err := r.history.AppendStep(ctx, p, step, finding); err != nil {
		logf.FromContext(ctx).Error(err, "record the Page step", "page", p.Name)
	}
	if r.dashboard != nil {
		r.dashboard.Publish(p.Name, dashboardapi.EventStep, dashboardapi.StepEvent{
			Step: step, Finding: finding, StepCount: p.Status.StepCount,
		})
	}
}
