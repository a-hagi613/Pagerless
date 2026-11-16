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
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
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
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/server"
)

const (
	// Namespace is the namespace in which pagerless-server runs in the harness, as in a cluster.
	Namespace = "pagerless"
	// AgentImage is the agent image that Jobs reference. No container ever runs in envtest.
	AgentImage = "registry.invalid/pagerless-agent:test"

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
	// Clock is the clock that pagerless-server reads, so tests can control timestamps.
	Clock *clocktesting.FakeClock
	// Sandbox is a local bare git repository that stands in for the Sandbox.
	Sandbox *BareRepo
	// WebhookInstall describes the installed webhooks, for tests that call them directly.
	WebhookInstall *envtest.WebhookInstallOptions
}

type options struct {
	validators    func(*Env) map[v1alpha1.Action]proposal.ActionValidator
	agentDeadline time.Duration
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

// Start runs the API server, installs pagerless-server's manifests, and runs
// pagerless-server. It stops everything when the test ends.
func Start(t testing.TB, opts ...Option) *Env {
	t.Helper()
	var o options
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
	for _, manifest := range []string{"namespace.yaml", "serviceaccounts.yaml", "rbac.yaml"} {
		if err := applyManifestFile(ctx, c, filepath.Join(deployDir, manifest)); err != nil {
			t.Fatalf("apply %s: %v", manifest, err)
		}
	}

	serverConfig := rest.CopyConfig(cfg)
	serverConfig.Impersonate = rest.ImpersonationConfig{
		UserName: fmt.Sprintf("system:serviceaccount:%s:%s", Namespace, server.DefaultServerServiceAccount),
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the report API: %v", err)
	}
	env := &Env{
		Config:         cfg,
		Client:         c,
		Clientset:      clientset,
		ServerConfig:   serverConfig,
		ReportURL:      "http://" + listener.Addr().String(),
		Clock:          clocktesting.NewFakeClock(StartTime),
		WebhookInstall: &testEnv.WebhookInstallOptions,
	}
	env.Sandbox = NewBareRepo(t)

	validators := proposal.DefaultValidators()
	if o.validators != nil {
		validators = o.validators(env)
	}
	mgr, err := server.New(serverConfig, server.Config{
		Namespace:      Namespace,
		AgentImage:     AgentImage,
		AgentServerURL: env.ReportURL,
		ReportListener: listener,
		AgentDeadline:  o.agentDeadline,
		Clock:          env.Clock,
		Validators:     validators,
		WebhookHost:    testEnv.WebhookInstallOptions.LocalServingHost,
		WebhookPort:    testEnv.WebhookInstallOptions.LocalServingPort,
		WebhookCertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
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
	return env
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
