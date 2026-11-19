package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// TestAPortalDecisionAppliesToAnExternallyProposedPage covers the kind-level parity check's shape: an
// operator (or a script) patches a Proposal into status, then the Portal records the decision. The
// controller must reconcile that annotation set exactly as it does a Proposal the pipeline produced.
func TestAPortalDecisionAppliesToAnExternallyProposedPage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)

	hash := strings.Repeat("a1", 32)
	latest := env.Get(t, created.Name)
	latest.Status.Phase = v1alpha1.PhaseProposed
	latest.Status.Proposal = &v1alpha1.Proposal{
		Action:     v1alpha1.ActionScale,
		Parameters: &v1alpha1.ActionParameters{Scale: &v1alpha1.ScaleParameters{Replicas: 2}},
		Hash:       hash,
		Attempt:    1,
		// The server checks expiry against its own clock, so the window is opened on that clock
		// rather than on the runner's wall clock.
		ExpiresAt: metav1.NewTime(env.Clock.Now().Add(time.Hour)),
	}
	if err := env.Client.Status().Update(t.Context(), latest); err != nil {
		t.Fatalf("propose the Page: %v", err)
	}

	response, body := dashboardRequest(t, env, env.Dashboard, http.MethodPost,
		"/api/pages/"+created.Name+"/decision",
		map[string]string{"decision": "approve", "proposalHash": hash, "reason": "parity check"})
	requireStatus(t, response, body, http.StatusCreated)

	var decision map[string]string
	if err := json.Unmarshal(body, &decision); err != nil {
		t.Fatalf("decode the decision answer: %v: %s", err, body)
	}
	if decision["decidedBy"] != testenv.DashboardUser {
		t.Errorf("answer = %v, want the signed-in user", decision)
	}

	decided := env.WaitFor(t, created.Name, "the decision is reconciled into status", func(tr *v1alpha1.Page) bool {
		return tr.Status.Decision != nil
	})
	if decided.Status.Decision.ProposalHash != hash || decided.Status.Decision.DecidedBy != testenv.DashboardUser {
		t.Errorf("decision = %+v, want the displayed hash and the signed-in user", decided.Status.Decision)
	}
}
