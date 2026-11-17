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
		testenv.WithApprovers(username),
	)
	deploy(t, env, workload{name: checkout})

	// The browser signs in and starts the Triage through the Start Triage page.
	name := waitForTriage(t, env)
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
	if result, err := agent.API.PostReport(t.Context(), noActionReport()); err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("post the report: %+v, %v, want 200", result, err)
	}
	env.WaitForPhase(t, name, v1alpha1.PhaseProposed)

	decided := env.WaitFor(t, name, "the dashboard records a decision", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Decision != nil
	})
	decision := decided.Status.Decision
	if decision.Approver != username {
		t.Fatalf("decision approver = %q, want the Keycloak user %q", decision.Approver, username)
	}
	if decision.Outcome != v1alpha1.DecisionApproved {
		t.Fatalf("decision outcome = %q, want Approved", decision.Outcome)
	}
	t.Logf("Triage %s approved by the asserted Keycloak user %s", name, decision.Approver)
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

// waitForTriage waits until the browser has started a Triage.
func waitForTriage(t *testing.T, env *testenv.Env) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var list v1alpha1.TriageList
		if err := env.Client.List(t.Context(), &list, client.InNamespace(testenv.Namespace)); err != nil {
			t.Fatalf("list Triages: %v", err)
		}
		if len(list.Items) > 0 {
			return list.Items[0].Name
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the browser did not start a Triage within five minutes")
	return ""
}
