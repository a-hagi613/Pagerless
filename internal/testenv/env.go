// Package testenv is the envtest harness for pagerless-server. It starts a real
// API server and etcd, installs the CRDs and the RBAC that charts/pagerless
// renders, and runs pagerless-server in process under its own service account
// identity, so a test exercises the same permissions a cluster grants.
//
// Tests drive pagerless-server only through its public interfaces: they create
// Pages with the Kubernetes API and play the agent through the agent API,
// using a stand-in agent whose service account token the API server issues.
// Later tickets extend the harness with options and helpers; the Sandbox slot
// is a local bare git repository.
package testenv

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/dashboardapi"
	"github.com/a-hagi613/pagerless/internal/history"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/watcher"
)

const (
	// Namespace is the namespace in which pagerless-server runs in the harness, as in a cluster.
	Namespace = "pagerless"
	// SandboxNamespace is the namespace whose Targets the harness's executable Actions may change.
	SandboxNamespace = "sandbox"
	// AgentImage is the agent image that Jobs reference. No container ever runs in envtest.
	AgentImage = "registry.invalid/pagerless-agent:test"
	// AlertmanagerToken is the deterministic bearer token used by alert receiver seam tests.
	AlertmanagerToken = "pagerless-alertmanager-test-token"
	// ClusterID is the stable cluster identifier the watcher hashes into every Fingerprint.
	ClusterID = "cluster-test"

	pollInterval = 50 * time.Millisecond
	waitTimeout  = 30 * time.Second
)

// StartTime is the instant at which the harness's fake clock starts.
var StartTime = time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)

// Env is a running harness.
type Env struct {
	// Config is the admin REST configuration of the test API server.
	Config *rest.Config
	// Client is an admin client for the test API server.
	Client client.Client
	// Clientset is an admin clientset for the test API server.
	Clientset kubernetes.Interface
	// ServerConfig is the REST configuration that pagerless-server runs with:
	// the identity and therefore the permissions of its service account.
	ServerConfig *rest.Config
	// ReportURL is the base URL of the agent API.
	ReportURL string
	// MetricsURL is the base URL of the Prometheus metrics API.
	MetricsURL string
	// DashboardURL is the base URL of the dashboard and its read-only API.
	DashboardURL string
	// HistoryPath is the SQLite file the server appends history to.
	HistoryPath string
	// History is the server's history, opened for reading by the test.
	History *history.Store
	// Clock is the clock that pagerless-server reads, so tests can control timestamps.
	Clock *clocktesting.FakeClock
	// Sandbox is a local bare git repository that stands in for the Sandbox.
	Sandbox *BareRepo
	// GitHub is the fake GitHub API that pagerless-executor opens pull requests on. Its repository is Sandbox.
	GitHub *FakeGitHub
	// ArgoCD is the fake Argo CD API that pagerless-server asks whether the merged change is synced.
	ArgoCD *FakeArgoCD
	// Alertmanager is the fake Alertmanager API that pagerless-server asks whether the triggering alert resolved.
	Alertmanager *FakeAlertmanager
	// Dashboard is the signed-in session that the dashboard tests send.
	Dashboard DashboardSession

	auth *fakeAuth
	t    testing.TB
}

type options struct {
	validators        func(*Env) map[v1alpha1.Action]proposal.ActionValidator
	proposalPoll      time.Duration
	pullRequestPoll   time.Duration
	verificationPoll  time.Duration
	retentionPoll     time.Duration
	agentDeadline     time.Duration
	agentRuntime      controller.AgentRuntimeConfig
	alertNamespaces   []string
	alertToken        func() (string, error)
	dashboardUser     string
	dashboardApprover bool
	dashboardAuth     dashboardapi.Auth
	dashboardAddress  string
	watcher           *watcher.Config
}

// WithWatcher enables the cluster watcher with the given configuration. The
// harness fills the fields a test must not have to set: the release namespace,
// the fake clock, and a stable cluster identifier, so a test's Fingerprints are
// reproducible.
func WithWatcher(cfg watcher.Config) Option {
	return func(o *options) { o.watcher = &cfg }
}

// Option customizes the harness.
type Option func(*options)

// WithValidators sets the Actions that Validation accepts. The function runs
// after the API server and the Sandbox exist, and receives the harness so
// that validators can use them.
func WithValidators(f func(*Env) map[v1alpha1.Action]proposal.ActionValidator) Option {
	return func(o *options) { o.validators = f }
}

// WithAgentDeadline sets the agent Job's active deadline.
func WithAgentDeadline(d time.Duration) Option {
	return func(o *options) { o.agentDeadline = d }
}

// WithAlertNamespaces sets the namespaces whose opted-in alerts start Pages.
func WithAlertNamespaces(namespaces ...string) Option {
	return func(o *options) { o.alertNamespaces = namespaces }
}

// WithAlertmanagerToken sets the alert receiver's rotating token source.
func WithAlertmanagerToken(source func() (string, error)) Option {
	return func(o *options) { o.alertToken = source }
}

// WithDashboardAuth replaces the harness's stand-in sign-in with a real one,
// such as the Keycloak flow that the smoke test drives through a browser.
func WithDashboardAuth(auth dashboardapi.Auth) Option {
	return func(o *options) { o.dashboardAuth = auth }
}

// WithDashboardAddress binds the dashboard listener to a fixed address, so a
// browser can be pointed at a URL that is known before the harness starts.
func WithDashboardAddress(address string) Option {
	return func(o *options) { o.dashboardAddress = address }
}

// WithRetentionPollInterval sets how often the retention controller checks a terminal Page. The fake clock only
// advances when a test steps it, so a test that observes the deletion boundary sets a short interval; every other
// test keeps the server's one-minute default and does no retention work.
func WithRetentionPollInterval(d time.Duration) Option {
	return func(o *options) { o.retentionPoll = d }
}

// Start runs the API server, installs the manifests of pagerless-server and
// pagerless-executor, and runs both. It stops everything when the test ends.
func Start(t testing.TB, opts ...Option) *Env {
	t.Helper()
	o := options{
		pullRequestPoll:   defaultPullRequestPoll,
		verificationPoll:  defaultVerificationPoll,
		dashboardUser:     DashboardUser,
		dashboardApprover: true,
	}
	o.alertToken = func() (string, error) { return AlertmanagerToken, nil }
	for _, opt := range opts {
		opt(&o)
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set; run the tests with `make test-go`, which downloads the envtest binaries")
	}
	// Only errors reach the test output, so that a passing run stays quiet.
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.Level(zapcore.ErrorLevel)))

	root := repoRoot(t)
	rendered := chart(t, root)
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "charts", "pagerless", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		_ = testEnv.Stop()
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	scheme, err := server.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("create clientset: %v", err)
	}
	ctx := t.Context()
	for _, namespace := range []string{Namespace, SandboxNamespace} {
		if err := c.Create(ctx, &corev1.Namespace{Name: namespace}); err != nil && !errors.IsAlreadyExists(err) {
			t.Fatalf("create namespace %s: %v", namespace, err)
		}
	}
	if err := applyManifestFile(ctx, c, rendered.objects); err != nil {
		t.Fatalf("apply the rendered chart: %v", err)
	}

	serverConfig := rest.CopyConfig(cfg)
	// The default client-side rate limit would throttle tests that poll quickly.
	serverConfig.QPS, serverConfig.Burst = -1, 0
	serverConfig.Impersonate = rest.ImpersonationConfig{
		UserName: fmt.Sprintf("system:serviceaccount:%s:%s", Namespace, server.DefaultServerServiceAccount),
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the report API: %v", err)
	}
	metricsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the metrics API: %v", err)
	}
	t.Cleanup(func() { _ = metricsListener.Close() })
	dashboardListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the dashboard: %v", err)
	}
	if o.dashboardAddress != "" {
		_ = dashboardListener.Close()
		dashboardListener, err = net.Listen("tcp", o.dashboardAddress)
		if err != nil {
			t.Fatalf("listen for the dashboard on %s: %v", o.dashboardAddress, err)
		}
	}
	t.Cleanup(func() { _ = dashboardListener.Close() })
	historyPath := filepath.Join(t.TempDir(), "history.db")
	env := &Env{
		Config:       cfg,
		Client:       c,
		Clientset:    clientset,
		ServerConfig: serverConfig,
		ReportURL:    "http://" + listener.Addr().String(),
		MetricsURL:   "http://" + metricsListener.Addr().String(),
		DashboardURL: "http://" + dashboardListener.Addr().String(),
		HistoryPath:  historyPath,
		Clock:        clocktesting.NewFakeClock(StartTime),
		auth:         newFakeAuth(),
		t:            t,
	}
	env.Dashboard = env.auth.signIn(o.dashboardUser, o.dashboardApprover)
	auth := dashboardapi.Auth(env.auth)
	if o.dashboardAuth != nil {
		auth = o.dashboardAuth
	}
	env.Sandbox = NewBareRepo(t)
	env.GitHub = NewFakeGitHub(t, env.Sandbox)
	env.ArgoCD = NewFakeArgoCD(t)
	env.Alertmanager = NewFakeAlertmanager(t)
	validators := proposal.DefaultValidators()
	if o.validators != nil {
		validators = o.validators(env)
	}
	agentRuntime := o.agentRuntime
	if agentRuntime.GatewayURL == "" {
		agentRuntime.GatewayURL = "http://mcp-gateway.agentgateway-system.svc.cluster.local"
	}
	if agentRuntime.KeycloakIssuer == "" {
		agentRuntime.KeycloakIssuer = "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless"
	}
	if agentRuntime.KeycloakTokenURL == "" {
		agentRuntime.KeycloakTokenURL = agentRuntime.KeycloakIssuer + "/protocol/openid-connect/token"
	}
	if agentRuntime.KeycloakClientID == "" {
		agentRuntime.KeycloakClientID = controller.DefaultKeycloakClientID
	}
	mgr, err := server.New(serverConfig, server.Config{
		Namespace:                Namespace,
		AgentImage:               AgentImage,
		AgentServerURL:           env.ReportURL,
		AgentRuntime:             agentRuntime,
		ReportListener:           listener,
		MetricsListener:          metricsListener,
		DashboardListener:        dashboardListener,
		GrafanaURL:               "http://grafana.monitoring.test",
		HistoryPath:              historyPath,
		Sandbox:                  env.SandboxRepository(),
		GitHubAPIURL:             env.GitHub.URL,
		GitHubRepository:         GitHubRepository,
		GitHubToken:              func() (string, error) { return env.GitHub.Token(), nil },
		AgentDeadline:            o.agentDeadline,
		ProposalPollInterval:     o.proposalPoll,
		PullRequestPollInterval:  o.pullRequestPoll,
		ArgoCDURL:                env.ArgoCD.URL,
		ArgoCDToken:              func() (string, error) { return env.ArgoCD.Token(), nil },
		AlertmanagerURL:          env.Alertmanager.URL,
		AlertmanagerToken:        o.alertToken,
		VerificationPollInterval: o.verificationPoll,
		RetentionPollInterval:    o.retentionPoll,
		AlertNamespaces:          o.alertNamespaces,
		TargetNamespaces:         []string{SandboxNamespace},
		DashboardAuth:            auth,
		Clock:                    env.Clock,
		Validators:               validators,
		Watcher:                  watcherConfig(o.watcher, env.Clock),
	})
	if err != nil {
		t.Fatalf("create pagerless-server: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run pagerless-server: %v", err)
		}
	})
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("pagerless-server cache did not sync")
	}
	env.History, err = history.OpenReader(historyPath)
	if err != nil {
		t.Fatalf("open the Page history for reading: %v", err)
	}
	t.Cleanup(func() {
		if err := env.History.Close(); err != nil {
			t.Errorf("close the Page history: %v", err)
		}
	})
	waitForMetrics(t, env.MetricsURL)
	return env
}

// waitForMetrics waits until the metrics API answers, so that the first scrape in a test cannot race the manager.
func waitForMetrics(t testing.TB, baseURL string) {
	t.Helper()
	httpClient := &http.Client{Timeout: time.Second}
	err := wait.PollUntilContextTimeout(
		t.Context(), pollInterval, waitTimeout, true, func(context.Context) (bool, error) {
			response, err := httpClient.Get(baseURL + "/metrics")
			if err != nil {
				return false, nil
			}
			defer func() { _ = response.Body.Close() }()
			return response.StatusCode == http.StatusOK, nil
		},
	)
	if err != nil {
		t.Fatalf("the metrics API at %s did not start: %v", baseURL, err)
	}
}

// watcherConfig fills the watcher settings the harness owns: the release
// namespace, the fake clock every test steps, and a fixed cluster identifier so
// a Fingerprint is reproducible across runs. It returns nil when the test did
// not enable the watcher, which keeps every existing suite unaffected.
func watcherConfig(cfg *watcher.Config, clock *clocktesting.FakeClock) *watcher.Config {
	if cfg == nil {
		return nil
	}
	filled := *cfg
	filled.Namespace = Namespace
	filled.ClusterID = ClusterID
	filled.Clock = clock
	return &filled
}

// WaitForGone polls until the Page no longer exists.
func (e *Env) WaitForGone(t testing.TB, name string) {
	t.Helper()
	key := types.NamespacedName{Namespace: Namespace, Name: name}
	err := wait.PollUntilContextTimeout(
		t.Context(), pollInterval, waitTimeout, true, func(ctx context.Context) (bool, error) {
			var tr v1alpha1.Page
			if err := e.Client.Get(ctx, key, &tr); err != nil {
				if errors.IsNotFound(err) {
					return true, nil
				}
				return false, err
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("waiting for Page %s to be deleted: %v", name, err)
	}
}

func repoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the harness source file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// applyManifestFile creates every object in a multi-document YAML file.
func applyManifestFile(ctx context.Context, c client.Client, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	for doc := range strings.SplitSeq(string(raw), "\n---") {
		var obj unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			return fmt.Errorf("decode manifest document: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Create(ctx, &obj); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// WaitFor polls until the Page satisfies the condition and returns it. The
// description names the condition in the failure message.
func (e *Env) WaitFor(
	t testing.TB, name, description string, condition func(*v1alpha1.Page) bool,
) *v1alpha1.Page {
	t.Helper()
	var last v1alpha1.Page
	key := types.NamespacedName{Namespace: Namespace, Name: name}
	poll := func(ctx context.Context) (bool, error) {
		if err := e.Client.Get(ctx, key, &last); err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return condition(&last), nil
	}
	if err := wait.PollUntilContextTimeout(t.Context(), pollInterval, waitTimeout, true, poll); err != nil {
		t.Fatalf("Page %s: waiting until %s: %v (last status %+v)", name, description, err, last.Status)
	}
	return &last
}

// WaitForPhase polls until the Page is in the phase.
func (e *Env) WaitForPhase(t testing.TB, name string, phase v1alpha1.Phase) *v1alpha1.Page {
	t.Helper()
	inPhase := func(tr *v1alpha1.Page) bool { return tr.Status.Phase == phase }
	return e.WaitFor(t, name, "phase is "+string(phase), inPhase)
}

// WaitForApproved polls until the Page holds an Approval: it is Approved,
// or Executing once pagerless-server has handed an Approved Proposal that is
// delivered by pull request to the Executor, which happens within moments.
func (e *Env) WaitForApproved(t testing.TB, name string) *v1alpha1.Page {
	t.Helper()
	approved := func(tr *v1alpha1.Page) bool {
		return tr.Status.Phase == v1alpha1.PhaseApproved || tr.Status.Phase == v1alpha1.PhasePullRequestOpen
	}
	return e.WaitFor(t, name, "phase is Approved or Executing", approved)
}

// RequirePhaseStays asserts that a Page stays in the phase for long enough that
// a controller pass would have moved it had it been going to.
func (e *Env) RequirePhaseStays(t testing.TB, name string, phase v1alpha1.Phase) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := e.Get(t, name).Status.Phase; got != phase {
			t.Fatalf("phase = %s, want it to stay %s", got, phase)
		}
		time.Sleep(pollInterval)
	}
}

// Get returns the Page as the API server holds it.
func (e *Env) Get(t testing.TB, name string) *v1alpha1.Page {
	t.Helper()
	var tr v1alpha1.Page
	if err := e.Client.Get(t.Context(), types.NamespacedName{Namespace: Namespace, Name: name}, &tr); err != nil {
		t.Fatalf("get Page %s: %v", name, err)
	}
	return &tr
}

// NewPage returns a Page for the Deployment that targetName names. The
// Fingerprint is the same for every Page a test creates for the same Target, so
// a second Page for one Target is a duplicate the controller supersedes.
func NewPage(targetName string) *v1alpha1.Page {
	target := v1alpha1.TargetReference{
		APIVersion: "apps/v1",
		Kind:       v1alpha1.TargetKindDeployment,
		Namespace:  SandboxNamespace,
		Name:       targetName,
	}
	return &v1alpha1.Page{
		GenerateName: "page-", Namespace: Namespace,
		Spec: v1alpha1.PageSpec{
			Target:      target,
			Fingerprint: v1alpha1.Fingerprint(target, "TestFailure"),
			Trigger:     v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI},
		},
	}
}

// CreatePage creates a Page as the admin user and returns it.
func (e *Env) CreatePage(t testing.TB, tr *v1alpha1.Page) *v1alpha1.Page {
	t.Helper()
	if err := e.Client.Create(t.Context(), tr); err != nil {
		t.Fatalf("create Page: %v", err)
	}
	return tr
}
