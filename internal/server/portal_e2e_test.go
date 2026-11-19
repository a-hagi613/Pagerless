package server_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/credentials"
	"github.com/a-hagi613/pagerless/internal/dashboardauth"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	// portalE2EAddress is fixed so the Playwright suite can be pointed at it before the browser starts.
	portalE2EAddress = "127.0.0.1:18082"
	// portalE2EToken is the bootstrap token the suite signs in with.
	portalE2EToken = "portal-e2e-bootstrap-token"
	// shopNamespace is the second namespace whose Page makes the matrix show two tiles.
	shopNamespace = "shop"
)

// TestPortalBrowserFlows drives the built Portal against a real pagerless-server: envtest's API server,
// the server's own controllers, its dashboard listener, and its real bootstrap-token sign-in. Nothing in
// the browser run is mocked. Enable it with PAGERLESS_PORTAL_E2E=1 after `make dashboard-build`, so the
// embedded dist is current:
//
//	PAGERLESS_PORTAL_E2E=1 make portal-e2e
func TestPortalBrowserFlows(t *testing.T) {
	if os.Getenv("PAGERLESS_PORTAL_E2E") != "1" {
		t.Skip("set PAGERLESS_PORTAL_E2E=1 to run the browser suite against the real server")
	}
	root := repositoryRoot(t)
	if _, err := os.Stat(filepath.Join(root, "dashboard", "dist", "index.html")); err != nil {
		t.Fatalf("the dashboard build is missing: run `make dashboard-build` first: %v", err)
	}

	startPortalServer(t, dashboardauth.ModeToken)

	suite := exec.CommandContext(t.Context(), "npm", "run", "test:e2e")
	suite.Dir = filepath.Join(root, "dashboard")
	suite.Env = append(os.Environ(),
		"PAGERLESS_PORTAL_URL=http://"+portalE2EAddress,
		"PAGERLESS_BOOTSTRAP_TOKEN="+portalE2EToken,
		"CI=1",
	)
	output, err := suite.CombinedOutput()
	t.Logf("playwright output:\n%s", output)
	if err != nil {
		t.Fatalf("the Playwright suite failed: %v", err)
	}
}

// startPortalServer runs the real server with the requested authentication mode and the Portal's seed data.
func startPortalServer(t *testing.T, mode string) *testenv.Env {
	t.Helper()
	auth, err := dashboardauth.New(dashboardauth.Config{
		Mode:       mode,
		AdminToken: func() (string, error) { return portalE2EToken, nil },
		SessionKey: func() ([]byte, error) { return bytes.Repeat([]byte("p"), 32), nil },
	})
	if err != nil {
		t.Fatalf("create the real sign-in: %v", err)
	}
	env := testenv.Start(t,
		withRemediations(),
		testenv.WithDashboardAddress(portalE2EAddress),
		testenv.WithDashboardAuth(auth),
	)
	seedCredentials(t, env)
	seedPortalPages(t, env)
	return env
}

// TestPortalServesForInspection holds the same server open so a design detector can scan the live page.
//
//	PAGERLESS_PORTAL_INSPECT=10m go test -run TestPortalServesForInspection ./internal/server
func TestPortalServesForInspection(t *testing.T) {
	raw := os.Getenv("PAGERLESS_PORTAL_INSPECT")
	if raw == "" {
		t.Skip("set PAGERLESS_PORTAL_INSPECT=<duration> to hold the server open")
	}
	hold, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("PAGERLESS_PORTAL_INSPECT = %q: %v", raw, err)
	}
	// The detector loads the page without a browser session, so this harness runs the fenced disabled
	// mode; the design tokens, components, and both palettes are the same.
	startPortalServer(t, dashboardauth.ModeDisabled)
	t.Logf("serving the Portal at http://%s", portalE2EAddress)
	time.Sleep(hold)
}

// seedCredentials writes the Secret the settings route reads, with the values the onboarding redirect and
// the save flow need. Without it the Portal correctly reports a missing Secret.
func seedCredentials(t *testing.T, env *testenv.Env) {
	t.Helper()
	data := map[string][]byte{}
	for _, key := range credentials.Keys() {
		data[key] = []byte("")
	}
	data[credentials.KeyAIBaseURL] = []byte("https://models.example/v1")
	data[credentials.KeyAIModel] = []byte("e2e-model")
	data[credentials.KeyAIAPIKey] = []byte("sk-e2e")
	data[credentials.KeyGitHubRepo] = []byte("acme/shop")
	data[credentials.KeyGitHubToken] = []byte("ghp-e2e")
	var secret corev1.Secret
	if err := env.Client.Get(t.Context(),
		client.ObjectKey{Namespace: testenv.Namespace, Name: "pagerless-credentials"}, &secret); err != nil {
		t.Fatalf("read the credentials Secret the chart installed: %v", err)
	}
	secret.Data = data
	if err := env.Client.Update(t.Context(), &secret); err != nil {
		t.Fatalf("seed the credentials Secret: %v", err)
	}
}

// seedPortalPages creates the cluster view the suite drives: two Proposed Pages per namespace (one to
// approve, one to dismiss), an Investigating Page whose trace keeps growing, and a Node Page for the node
// tile. Each browser project signs in with its own context and acts on its own pair.
func seedPortalPages(t *testing.T, env *testenv.Env) {
	t.Helper()
	// Every Page that produces a Proposal targets the sandbox, which is the one namespace the harness's
	// validators may change. The two browser projects act on their own pair.
	for _, name := range []string{"checkout", "cart", "search", "billing"} {
		deployPortalTarget(t, env, testenv.SandboxNamespace, name)
		proposed(t, env, testenv.SandboxNamespace, name, 3)
	}

	// A second namespace makes the matrix a real choice. This Page is Investigating and never reports, so
	// it narrows the inbox without changing any other flow.
	deployPortalTarget(t, env, shopNamespace, "audit")
	startPortalPage(t, env, shopNamespace, "audit")

	// The live Page the suite watches: the stream must append the steps posted below.
	deployPortalTarget(t, env, testenv.SandboxNamespace, "payments")
	name, agent := startPortalPage(t, env, testenv.SandboxNamespace, "payments")
	for i, event := range []agentapi.StepEvent{
		{Kind: v1alpha1.StepKindProgress, Summary: "gathered the Target status"},
		{Kind: v1alpha1.StepKindToolCall, Tool: "pods.logs", Summary: "read the previous container logs"},
		{Kind: v1alpha1.StepKindFinding, Summary: "the container was OOMKilled", Finding: &logFinding},
	} {
		if result, err := agent.API.PostEvent(t.Context(), event); err != nil || result.StatusCode != http.StatusAccepted {
			t.Fatalf("post step %d: %+v, %v, want 202", i, result, err)
		}
	}
	// Keep the trace growing while the suite runs, so a browser proves the stream appends rather than
	// rendering one snapshot. The goroutine stops when the test ends.
	go func() {
		for i := 0; i < 20; i++ {
			select {
			case <-t.Context().Done():
				return
			case <-time.After(2 * time.Second):
			}
			_, _ = agent.API.PostEvent(t.Context(), agentapi.StepEvent{
				Kind:    v1alpha1.StepKindProgress,
				Summary: fmt.Sprintf("re-checking the Target, observation %d", i+1),
			})
		}
	}()
	_ = name

	// A Node Page makes the node tile real rather than decorative. The Node does not exist in envtest, so
	// the Page is read as a Pending or Failed Page whose Target is the node.
	createPortalPage(t, env, v1alpha1.TargetReference{
		APIVersion: "v1", Kind: v1alpha1.TargetKindNode, Name: "ip-10-0-1-17",
	})
}

// deployPortalTarget commits a manifest and creates the Deployment in its namespace, with the source-path
// annotation the repository ladder reads.
func deployPortalTarget(t *testing.T, env *testenv.Env, namespace, name string) {
	t.Helper()
	if namespace != testenv.SandboxNamespace {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		if err := env.Client.Create(t.Context(), ns); err != nil {
			t.Fatalf("create namespace %s: %v", namespace, err)
		}
	}
	// The manifest lives in the Sandbox repository; the live Deployment may sit in another namespace.
	env.Sandbox.Commit(target{name: name}.sourcePath(), target{name: name}.manifest(), "Add "+name)
	var deployment appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(target{name: name}.manifest()), &deployment); err != nil {
		t.Fatalf("decode the %s manifest: %v", name, err)
	}
	deployment.Namespace = namespace
	if err := env.Client.Create(t.Context(), &deployment); err != nil {
		t.Fatalf("create Deployment %s/%s: %v", namespace, name, err)
	}
}

// proposed opens a Page for the Target, has the stand-in agent propose a scale, and waits for Proposed.
func proposed(t *testing.T, env *testenv.Env, namespace, name string, replicas int32) string {
	t.Helper()
	pageName, agent := startPortalPage(t, env, namespace, name)
	if result, err := agent.API.PostReport(t.Context(), scaleReport(replicas)); err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report for %s: %+v, %v, want 200", name, result, err)
	}
	env.WaitForPhase(t, pageName, v1alpha1.PhaseProposed)
	return pageName
}

// startPortalPage creates a Page for one Target and its stand-in agent.
func startPortalPage(t *testing.T, env *testenv.Env, namespace, name string) (string, *testenv.StandInAgent) {
	t.Helper()
	page := createPortalPage(t, env, v1alpha1.TargetReference{
		APIVersion: "apps/v1", Kind: v1alpha1.TargetKindDeployment, Namespace: namespace, Name: name,
	})
	return page.Name, env.StartAgent(t, page.Name)
}

// createPortalPage creates a Page whose Target is ref.
func createPortalPage(t *testing.T, env *testenv.Env, ref v1alpha1.TargetReference) *v1alpha1.Page {
	t.Helper()
	page := &v1alpha1.Page{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "portal-", Namespace: testenv.Namespace},
		Spec: v1alpha1.PageSpec{
			Target:      ref,
			Fingerprint: v1alpha1.Fingerprint(ref, "PortalE2E"),
			Trigger:     v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI},
		},
	}
	if err := env.Client.Create(t.Context(), page); err != nil {
		t.Fatalf("create Page for %+v: %v", ref, err)
	}
	return page
}

// repositoryRoot walks up from this file until it finds go.mod.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the test file")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", filepath.Dir(file))
		}
		dir = parent
	}
}
