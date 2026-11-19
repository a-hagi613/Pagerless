package server_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	aliceUser = "alice@example.com"
	bobUser   = "bob@example.com"
)

// approve records an Approval of the Page's current Proposal as the user.
func approve(t *testing.T, env *testenv.Env, page *v1alpha1.Page) {
	t.Helper()
	decide(t, env, page, v1alpha1.DecisionAnnotationApprove, aliceUser, "")
}

// dismiss records a Rejection of the Page's current Proposal as the user.
func dismiss(t *testing.T, env *testenv.Env, page *v1alpha1.Page, user, reason string) {
	t.Helper()
	decide(t, env, page, v1alpha1.DecisionAnnotationReject, user, reason)
}

// decide patches the decision annotations, as kubectl page or the Portal does.
func decide(t *testing.T, env *testenv.Env, page *v1alpha1.Page, outcome, user, reason string) {
	t.Helper()
	latest := env.Get(t, page.Name)
	patch := client.MergeFrom(latest.DeepCopy())
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[v1alpha1.AnnotationDecision] = outcome
	latest.Annotations[v1alpha1.AnnotationDecidedBy] = user
	latest.Annotations[v1alpha1.AnnotationDecisionProposal] = latest.Status.Proposal.Hash
	if reason != "" {
		latest.Annotations[v1alpha1.AnnotationDecisionReason] = reason
	}
	if err := env.Client.Patch(t.Context(), latest, patch); err != nil {
		t.Fatalf("patch the decision: %v", err)
	}
}

// propose runs a stand-in agent that reports a NoAction Proposal and waits
// until the Page is Proposed.
func propose(t *testing.T, env *testenv.Env, page *v1alpha1.Page) *v1alpha1.Page {
	t.Helper()
	agent := env.StartAgent(t, page.Name)
	result, err := agent.API.PostReport(t.Context(), noActionReport())
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v", result, err)
	}
	return env.WaitForPhase(t, page.Name, v1alpha1.PhaseProposed)
}

func TestAnApprovalAnnotationMovesThePageToApproved(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	page := propose(t, env, env.CreatePage(t, testenv.NewPage(checkout)))

	approve(t, env, page)
	approved := env.WaitForPhase(t, page.Name, v1alpha1.PhaseDiagnosed)
	decision := approved.Status.Decision
	if decision == nil || decision.Outcome != v1alpha1.DecisionApproved ||
		decision.DecidedBy != aliceUser || decision.ProposalHash != page.Status.Proposal.Hash {
		t.Fatalf("decision = %+v, want an Approval of %s by %s",
			decision, page.Status.Proposal.Hash, aliceUser)
	}
	condition := metaCondition(approved, v1alpha1.ConditionDecided)
	if condition == nil || condition.Reason != v1alpha1.ReasonAcknowledged {
		t.Fatalf("Decided condition = %+v, want an acknowledgement of the no-change Proposal", condition)
	}
}

func TestARejectionKeepsItsReasonAndEndsThePage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	page := propose(t, env, env.CreatePage(t, testenv.NewPage(checkout)))

	dismiss(t, env, page, bobUser, "1Gi is safer")
	rejected := env.WaitForPhase(t, page.Name, v1alpha1.PhaseRejected)
	decision := rejected.Status.Decision
	if decision == nil || decision.Outcome != v1alpha1.DecisionRejected ||
		decision.Reason != "1Gi is safer" || decision.DecidedBy != bobUser {
		t.Fatalf("decision = %+v, want a Rejection by %s with its reason", decision, bobUser)
	}
	if condition := metaCondition(rejected, v1alpha1.ConditionDecided); condition == nil ||
		condition.Reason != v1alpha1.ReasonRejected {
		t.Errorf("Decided condition = %+v, want Rejected", condition)
	}
}

func TestADecisionThatDoesNotBindToTheCurrentProposalIsIgnored(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	page := propose(t, env, env.CreatePage(t, testenv.NewPage(checkout)))

	latest := env.Get(t, page.Name)
	patch := client.MergeFrom(latest.DeepCopy())
	latest.Annotations = map[string]string{
		v1alpha1.AnnotationDecision:         v1alpha1.DecisionAnnotationApprove,
		v1alpha1.AnnotationDecisionProposal: strings.Repeat("0", 64),
		v1alpha1.AnnotationDecidedBy:        aliceUser,
	}
	if err := env.Client.Patch(t.Context(), latest, patch); err != nil {
		t.Fatal(err)
	}
	env.RequirePhaseStays(t, page.Name, v1alpha1.PhaseProposed)
	if got := env.Get(t, page.Name); got.Status.Decision != nil {
		t.Fatalf("decision = %+v, want none for a decision bound to another hash", got.Status.Decision)
	}
}

func TestADecisionWithoutADeclaredIdentityIsIgnored(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	page := propose(t, env, env.CreatePage(t, testenv.NewPage(checkout)))

	latest := env.Get(t, page.Name)
	patch := client.MergeFrom(latest.DeepCopy())
	latest.Annotations = map[string]string{
		v1alpha1.AnnotationDecision:         v1alpha1.DecisionAnnotationApprove,
		v1alpha1.AnnotationDecisionProposal: latest.Status.Proposal.Hash,
	}
	if err := env.Client.Patch(t.Context(), latest, patch); err != nil {
		t.Fatal(err)
	}
	env.RequirePhaseStays(t, page.Name, v1alpha1.PhaseProposed)
}

func TestADecisionAppliedOnceCannotBeReplayed(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	page := propose(t, env, env.CreatePage(t, testenv.NewPage(checkout)))

	approve(t, env, page)
	env.WaitForPhase(t, page.Name, v1alpha1.PhaseDiagnosed)

	// Flipping the annotation cannot turn an acknowledged Diagnosis into a Rejection,
	// because the Page is no longer Proposed.
	dismiss(t, env, page, bobUser, "changed my mind")
	got := env.Get(t, page.Name)
	if got.Status.Decision == nil || got.Status.Decision.Outcome != v1alpha1.DecisionApproved {
		t.Fatalf("decision = %+v, want the first Approval preserved", got.Status.Decision)
	}
}

func TestAnExpiredProposalIgnoresALateDecision(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	page := propose(t, env, env.CreatePage(t, testenv.NewPage(checkout)))
	env.Clock.SetTime(page.Status.Proposal.ExpiresAt.Add(time.Minute))

	approve(t, env, page)
	expired := env.WaitForPhase(t, page.Name, v1alpha1.PhaseExpired)
	if expired.Status.Decision != nil {
		t.Fatalf("decision = %+v, want none after the approval window closed", expired.Status.Decision)
	}
	if condition := metaCondition(expired, v1alpha1.ConditionDecided); condition == nil ||
		condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonExpired {
		t.Errorf("Decided condition = %+v, want False/Expired", condition)
	}
}

func TestASecondActivePageForOneFingerprintIsSuperseded(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	first := env.CreatePage(t, testenv.NewPage(checkout))
	env.WaitForPhase(t, first.Name, v1alpha1.PhaseInvestigating)
	second := env.CreatePage(t, testenv.NewPage(checkout))
	// Creation times are second-resolution, so the tie is broken by name: either
	// Page may be canonical, and the other must be superseded deterministically.
	winner, loser := canonicalAndSuperseded(t, env, first.Name, second.Name)
	condition := metaCondition(loser, v1alpha1.ConditionAccepted)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonDuplicate {
		t.Fatalf("Accepted condition = %+v, want False/Duplicate", condition)
	}
	if !strings.Contains(condition.Message, winner.Name) {
		t.Errorf("duplicate message = %q, want it to name the canonical Page %q", condition.Message, winner.Name)
	}
	if accepted := metaCondition(winner, v1alpha1.ConditionAccepted); accepted == nil ||
		accepted.Status != metav1.ConditionTrue || accepted.Reason != v1alpha1.ReasonCanonical {
		t.Errorf("canonical Accepted condition = %+v, want True/Canonical", accepted)
	}
}

// canonicalAndSuperseded waits until exactly one of the two Pages is Superseded
// and returns the canonical Page and the superseded one.
func canonicalAndSuperseded(t *testing.T, env *testenv.Env, names ...string) (*v1alpha1.Page, *v1alpha1.Page) {
	t.Helper()
	var canonical, superseded *v1alpha1.Page
	eventually(t, "one Page is superseded while another stays canonical", func() bool {
		canonical, superseded = nil, nil
		for _, name := range names {
			page := env.Get(t, name)
			switch {
			case page.Status.Phase == v1alpha1.PhaseSuperseded:
				superseded = page
			case page.Status.Phase.IsActive():
				if condition := metaCondition(page, v1alpha1.ConditionAccepted); condition != nil &&
					condition.Status == metav1.ConditionTrue {
					canonical = page
				}
			}
		}
		return canonical != nil && superseded != nil
	})
	return canonical, superseded
}

func TestSupersededPageStartsNoAgentJob(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	first := env.CreatePage(t, testenv.NewPage(checkout))
	env.WaitForPhase(t, first.Name, v1alpha1.PhaseInvestigating)
	second := env.CreatePage(t, testenv.NewPage(checkout))
	_, loser := canonicalAndSuperseded(t, env, first.Name, second.Name)

	if _, err := env.GetJob(t.Context(), loser.Name+"-agent"); !apierrors.IsNotFound(err) {
		t.Fatalf("agent Job for the superseded Page: error = %v, want NotFound", err)
	}
}

func TestADifferentFingerprintIsNeverSuperseded(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	first := env.CreatePage(t, testenv.NewPage(checkout))
	second := testenv.NewPage(checkout)
	second.Spec.Fingerprint = v1alpha1.Fingerprint(second.Spec.Target, "AnotherFailure")
	env.CreatePage(t, second)
	env.WaitForPhase(t, first.Name, v1alpha1.PhaseInvestigating)
	env.WaitForPhase(t, second.Name, v1alpha1.PhaseInvestigating)
}

func TestThePlatformDoesNotRequireAWebhookOrAnIdentityProvider(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	page := propose(t, env, env.CreatePage(t, testenv.NewPage(checkout)))
	if page.Status.Decision != nil {
		t.Fatalf("decision = %+v, want none before a human decides", page.Status.Decision)
	}
	if checks := page.Status.VerificationChecks; len(checks) != 0 {
		t.Fatalf("verification checks = %+v, want none before a pull request merges", checks)
	}
}

// metaCondition returns the named condition of a Page.
func metaCondition(page *v1alpha1.Page, conditionType string) *metav1.Condition {
	return meta.FindStatusCondition(page.Status.Conditions, conditionType)
}

// requireUndecided asserts that the Page is still in the phase and holds no decision.
func requireUndecided(t *testing.T, env *testenv.Env, name string, phase v1alpha1.Phase) {
	t.Helper()
	page := env.Get(t, name)
	if page.Status.Phase != phase || page.Status.Decision != nil {
		t.Fatalf("status = %+v, want %s with no decision", page.Status, phase)
	}
}
