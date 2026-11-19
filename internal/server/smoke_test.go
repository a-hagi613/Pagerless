//go:build smoke

// The dashboard smoke test runs the real dashboard and its Keycloak sign-in
// against the envtest API server, and leaves the browser work to
// dashboard/e2e/smoke.spec.ts. It is excluded from the normal suite, which must
// not depend on Docker, a browser, or a Keycloak container.
package server_test

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/dashboardauth"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

func TestDashboardSmoke(t *testing.T) {
	base := requiredEnv(t, "PAGERLESS_SMOKE_BASE_URL")
	issuer := requiredEnv(t, "PAGERLESS_SMOKE_ISSUER")
	username := requiredEnv(t, "PAGERLESS_SMOKE_APPROVER")
	secretFile := requiredEnv(t, "PAGERLESS_SMOKE_CLIENT_SECRET_FILE")
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse PAGERLESS_SMOKE_BASE_URL: %v", err)
	}

	auth, err := dashboardauth.New(dashboardauth.Config{
		Issuer:   issuer,
		ClientID: "pagerless-dashboard",
		ClientSecret: func() (string, error) {
			raw, err := os.ReadFile(secretFile)
			return strings.TrimSpace(string(raw)), err
		},
		RedirectURL:    base + "/auth/callback",
		ApproversGroup: "pagerless-approvers",
		SecureCookies:  false,
	})
	if err != nil {
		t.Fatalf("configure sign-in: %v", err)
	}

	env := testenv.Start(t,
		testenv.WithDashboardAuth(auth),
		testenv.WithDashboardAddress(parsed.Host),
	)
	deploy(t, env, target{name: checkout})

	// The browser signs in and starts the Page through the Start Page page.
	name := waitForPage(t, env)
	env.WaitForPhase(t, name, v1alpha1.PhaseInvestigating)
	agent := env.StartAgent(t, name)

	// Give the browser time to open the detail page and connect to the event
	// stream, then record steps one at a time so the page receives them live.
	time.Sleep(5 * time.Second)
	for i := 1; i <= 20; i++ {
		event := agentapi.StepEvent{
			Kind: v1alpha1.StepKindToolCall, Tool: "pods_log",
			Summary: fmt.Sprintf("smoke step %d", i),
		}
		if result, err := agent.API.PostEvent(t.Context(), event); err != nil || result.StatusCode != http.StatusAccepted {
			t.Fatalf("post step %d: %+v, %v, want 202", i, result, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	reportResult, err := agent.API.PostReport(t.Context(), noActionReport())
	if err != nil || reportResult.StatusCode != http.StatusOK {
		t.Fatalf("post the report: %+v, %v, want 200", reportResult, err)
	}
	env.WaitForPhase(t, name, v1alpha1.PhaseProposed)

	decided := waitForDecision(t, env, name)
	decision := decided.Status.Decision
	if decision.Approver != username {
		t.Fatalf("decision approver = %q, want the Keycloak user %q", decision.Approver, username)
	}
	if decision.Outcome != v1alpha1.DecisionApproved {
		t.Fatalf("decision outcome = %q, want Approved", decision.Outcome)
	}
	t.Logf("Page %s approved by the asserted Keycloak user %s", name, decision.Approver)
}

// waitForDecision polls slowly until the browser's decision is applied. The
// admin client's default rate limit makes the harness's 50ms poll too fast for
// a wait that can last minutes.
func waitForDecision(t *testing.T, env *testenv.Env, name string) *v1alpha1.Page {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		tr := env.Get(t, name)
		if tr.Status.Decision != nil {
			return tr
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("Page %s never recorded a decision", name)
	return nil
}

// requiredEnv reads a variable that the smoke runner sets.
func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is not set; run dashboard/e2e/run.sh", name)
	}
	return value
}

// waitForPage waits until the browser has started a Page. The browser has
// to start Chromium and complete a Keycloak sign-in first, so the bound is
// generous.
func waitForPage(t *testing.T, env *testenv.Env) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		var list v1alpha1.PageList
		if err := env.Client.List(t.Context(), &list, client.InNamespace(testenv.Namespace)); err != nil {
			t.Fatalf("list Pages: %v", err)
		}
		if len(list.Items) > 0 {
			return list.Items[0].Name
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the browser did not start a Page within five minutes")
	return ""
}
