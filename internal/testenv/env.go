// Package testenv is the envtest harness for pagerless-server (seam 2 of the
// Phase 1 spec). It starts a real API server and etcd, installs the CRDs, the
// webhook configuration, and the RBAC from deploy/pagerless-server, and runs
// pagerless-server in process under its own service account identity, so a
// test exercises the same permissions and admission path as a cluster does.
//
// Tests drive pagerless-server only through its public interfaces: they create
// Triages with the Kubernetes API and play the agent through the agent API,
// using a stand-in agent whose service account token the API server issues.
// Later tickets extend the harness with options and helpers; the Sandbox slot
// is a local bare git repository.
package testenv

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/history"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/server"
)

const (
	// Namespace is the namespace in which pagerless-server runs in the harness, as in a cluster.
	Namespace = "pagerless"
	// AgentImage is the agent image that Jobs reference. No container ever runs in envtest.
	AgentImage = "registry.invalid/pagerless-agent:test"
	// AlertmanagerToken is the deterministic bearer token used by alert receiver seam tests.
	AlertmanagerToken = "pagerless-alertmanager-test-token"

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
	// ExecutorURL is the base URL of pagerless-executor, which runs in the harness under its own service account.
	ExecutorURL string
	// WebhookInstall describes the installed webhooks, for tests that call them directly.
	WebhookInstall *envtest.WebhookInstallOptions

	t testing.TB
}

type options struct {
	validators       func(*Env) map[v1alpha1.Action]proposal.ActionValidator
	proposalPoll     time.Duration
	executionPoll    time.Duration
	verificationPoll time.Duration
	retentionPoll    time.Duration
	agentDeadline    time.Duration
	agentRuntime     controller.AgentRuntimeConfig
	alertNamespaces  []string
	alertToken       func() (string, error)
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

// WithAlertNamespaces sets the namespaces whose opted-in alerts start Triages.
func WithAlertNamespaces(namespaces ...string) Option {
	return func(o *options) { o.alertNamespaces = namespaces }
}

// WithAlertmanagerToken sets the alert receiver's rotating token source.
func WithAlertmanagerToken(source func() (string, error)) Option {
	return func(o *options) { o.alertToken = source }
}

// WithRetentionPollInterval sets how often the retention controller checks a terminal Triage. The fake clock only
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
		executionPoll:    defaultExecutionPoll,
		verificationPoll: defaultVerificationPoll,
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
	deployDir := filepath.Join(root, "deploy", "pagerless-server")
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(deployDir, "crds")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{Paths: []string{filepath.Join(deployDir, "webhook.yaml")}},
	}
	// Serve the webhooks on a listener that is bound before the API server starts. envtest chooses a port by
	// binding and immediately closing a listener, and the webhook server does not start until the manager does,
	// so an unbound port can be taken by another listener or connection in between, after which the webhook
	// server never starts. Serving the webhooks on the listener that reserved the port leaves no gap for that.
	webhookListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the webhook server: %v", err)
	}
	t.Cleanup(func() { _ = webhookListener.Close() })
	testEnv.WebhookInstallOptions.LocalServingHost = "127.0.0.1"
	testEnv.WebhookInstallOptions.LocalServingPort = webhookListener.Addr().(*net.TCPAddr).Port
	cfg, err := testEnv.Start()
	if err != nil {
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
	if err := c.Create(ctx, &corev1.Namespace{Name: "sandbox"}); err != nil {
		t.Fatalf("create the allowlisted Workload namespace: %v", err)
	}
	executorDir := filepath.Join(root, "deploy", "pagerless-executor")
	for _, manifest := range []string{
		filepath.Join(deployDir, "namespace.yaml"),
		filepath.Join(deployDir, "serviceaccounts.yaml"),
		filepath.Join(deployDir, "rbac.yaml"),
		filepath.Join(deployDir, "dry-run-policy.yaml"),
		filepath.Join(deployDir, "agent-job-policy.yaml"),
		filepath.Join(executorDir, "serviceaccounts.yaml"),
		filepath.Join(executorDir, "rbac.yaml"),
	} {
		if err := applyManifestFile(ctx, c, manifest); err != nil {
			t.Fatalf("apply %s: %v", manifest, err)
		}
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
	t.Cleanup(func() { _ = dashboardListener.Close() })
	historyPath := filepath.Join(t.TempDir(), "history.db")
	env := &Env{
		Config:         cfg,
		Client:         c,
		Clientset:      clientset,
		ServerConfig:   serverConfig,
		ReportURL:      "http://" + listener.Addr().String(),
		MetricsURL:     "http://" + metricsListener.Addr().String(),
		DashboardURL:   "http://" + dashboardListener.Addr().String(),
		HistoryPath:    historyPath,
		Clock:          clocktesting.NewFakeClock(StartTime),
		WebhookInstall: &testEnv.WebhookInstallOptions,
		t:              t,
	}
	env.Sandbox = NewBareRepo(t)
	env.GitHub = NewFakeGitHub(t, env.Sandbox)
	env.ArgoCD = NewFakeArgoCD(t)
	env.Alertmanager = NewFakeAlertmanager(t)
	env.ExecutorURL = startExecutor(t, env)

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
	executorToken := func() (string, error) {
		return env.issueToken(context.Background(), Namespace, server.DefaultServerServiceAccount, executorapi.Audience)
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
		ExecutorURL:              env.ExecutorURL,
		ExecutorToken:            executorToken,
		AgentDeadline:            o.agentDeadline,
		ProposalPollInterval:     o.proposalPoll,
		ExecutionPollInterval:    o.executionPoll,
		ArgoCDURL:                env.ArgoCD.URL,
		ArgoCDToken:              func() (string, error) { return env.ArgoCD.Token(), nil },
		AlertmanagerURL:          env.Alertmanager.URL,
		AlertmanagerToken:        o.alertToken,
		VerificationPollInterval: o.verificationPoll,
		RetentionPollInterval:    o.retentionPoll,
		AlertNamespaces:          o.alertNamespaces,
		Clock:                    env.Clock,
		Validators:               validators,
		WebhookCertDir:           testEnv.WebhookInstallOptions.LocalServingCertDir,
		WebhookListener:          webhookListener,
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
	waitForWebhooks(t, &testEnv.WebhookInstallOptions)
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("pagerless-server cache did not sync")
	}
	env.History, err = history.OpenReader(historyPath)
	if err != nil {
		t.Fatalf("open the Triage history for reading: %v", err)
	}
	t.Cleanup(func() {
		if err := env.History.Close(); err != nil {
			t.Errorf("close the Triage history: %v", err)
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

// WaitForGone polls until the Triage no longer exists.
func (e *Env) WaitForGone(t testing.TB, name string) {
	t.Helper()
	key := types.NamespacedName{Namespace: Namespace, Name: name}
	err := wait.PollUntilContextTimeout(
		t.Context(), pollInterval, waitTimeout, true, func(ctx context.Context) (bool, error) {
			var tr v1alpha1.Triage
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
		t.Fatalf("waiting for Triage %s to be deleted: %v", name, err)
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

func waitForWebhooks(t testing.TB, install *envtest.WebhookInstallOptions) {
	t.Helper()
	addr := net.JoinHostPort(install.LocalServingHost, strconv.Itoa(install.LocalServingPort))
	err := wait.PollUntilContextTimeout(t.Context(), pollInterval, waitTimeout, true, func(context.Context) (bool, error) {
		dialer := &net.Dialer{Timeout: time.Second}
		// The probe only checks that the server accepts connections.
		probe := &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, probe)
		if err != nil {
			return false, nil
		}
		_ = conn.Close()
		return true, nil
	})
	if err != nil {
		t.Fatalf("webhook server at %s did not start: %v", addr, err)
	}
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

// WaitFor polls until the Triage satisfies the condition and returns it. The
// description names the condition in the failure message.
func (e *Env) WaitFor(
	t testing.TB, name, description string, condition func(*v1alpha1.Triage) bool,
) *v1alpha1.Triage {
	t.Helper()
	var last v1alpha1.Triage
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
		t.Fatalf("Triage %s: waiting until %s: %v (last status %+v)", name, description, err, last.Status)
	}
	return &last
}

// WaitForPhase polls until the Triage is in the phase.
func (e *Env) WaitForPhase(t testing.TB, name string, phase v1alpha1.Phase) *v1alpha1.Triage {
	t.Helper()
	inPhase := func(tr *v1alpha1.Triage) bool { return tr.Status.Phase == phase }
	return e.WaitFor(t, name, "phase is "+string(phase), inPhase)
}

// WaitForApproved polls until the Triage holds an Approval: it is Approved,
// or Executing once pagerless-server has handed an Approved Proposal that is
// delivered by pull request to the Executor, which happens within moments.
func (e *Env) WaitForApproved(t testing.TB, name string) *v1alpha1.Triage {
	t.Helper()
	approved := func(tr *v1alpha1.Triage) bool {
		return tr.Status.Phase == v1alpha1.PhaseApproved || tr.Status.Phase == v1alpha1.PhaseExecuting
	}
	return e.WaitFor(t, name, "phase is Approved or Executing", approved)
}

// Get returns the Triage as the API server holds it.
func (e *Env) Get(t testing.TB, name string) *v1alpha1.Triage {
	t.Helper()
	var tr v1alpha1.Triage
	if err := e.Client.Get(t.Context(), types.NamespacedName{Namespace: Namespace, Name: name}, &tr); err != nil {
		t.Fatalf("get Triage %s: %v", name, err)
	}
	return &tr
}

// NewTriage returns a Triage for a Workload, ready to create.
func NewTriage(workloadName string) *v1alpha1.Triage {
	return &v1alpha1.Triage{
		GenerateName: "triage-", Namespace: Namespace,
		Spec: v1alpha1.TriageSpec{
			Workload: v1alpha1.WorkloadReference{
				Kind:      v1alpha1.WorkloadKindDeployment,
				Namespace: "sandbox",
				Name:      workloadName,
			},
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI},
		},
	}
}

// CreateTriage creates a Triage as the admin user and returns it.
func (e *Env) CreateTriage(t testing.TB, tr *v1alpha1.Triage) *v1alpha1.Triage {
	t.Helper()
	if err := e.Client.Create(t.Context(), tr); err != nil {
		t.Fatalf("create Triage: %v", err)
	}
	return tr
}
