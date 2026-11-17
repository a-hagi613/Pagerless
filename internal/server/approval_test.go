package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	webhookadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/admission"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	aliceUser = "alice@example.com"
	bobUser   = "bob@example.com"
	// forgedUser is pagerless-server's own identity, which no approver may claim.
	forgedUser    = "system:serviceaccount:pagerless:pagerless-server"
	serverName    = "pagerless-server"
	absentHash    = "0000000000000000000000000000000000000000000000000000000000000000"
	approvalsKind = "triageapprovals"
)

// writeVerbs are the verbs that change a resource. An approver may use only the first, on TriageApprovals.
var writeVerbs = []string{"create", updateVerb, "patch", "delete"}

// decision returns a TriageApproval for the Triage's current Proposal.
func decision(tr *v1alpha1.Triage, outcome v1alpha1.DecisionOutcome, reason string) *v1alpha1.TriageApproval {
	hash := absentHash
	if tr.Status.Proposal != nil {
		hash = tr.Status.Proposal.Hash
	}
	return &v1alpha1.TriageApproval{
		Name: v1alpha1.TriageApprovalName(tr, hash), Namespace: testenv.Namespace,
		Spec: v1alpha1.TriageApprovalSpec{
			Triage:       tr.Name,
			ProposalHash: hash,
			Decision:     outcome,
			Reason:       reason,
		},
	}
}

// approverClient returns a client for a user who holds the approver Role.
func approverClient(t *testing.T, env *testenv.Env, user string) client.Client {
	t.Helper()
	env.GrantApprover(t, user)
	return env.ClientAs(t, user)
}

func requireRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the TriageApproval was admitted, want a refusal mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to mention %q", err, want)
	}
}

// requireUndecided checks that a refused decision changed nothing: the Triage
// keeps its phase and holds no decision, and no TriageApproval was stored.
func requireUndecided(t *testing.T, env *testenv.Env, name string, phase v1alpha1.Phase) {
	t.Helper()
	tr := env.Get(t, name)
	if tr.Status.Phase != phase || tr.Status.Decision != nil {
		t.Errorf("after a refused decision: phase = %s, decision = %+v, want %s and none",
			tr.Status.Phase, tr.Status.Decision, phase)
	}
	var stored v1alpha1.TriageApprovalList
	if err := env.Client.List(t.Context(), &stored, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatal(err)
	}
	for _, a := range stored.Items {
		if a.Spec.Triage == name {
			t.Errorf("a refused TriageApproval was stored: %+v", a.Spec)
		}
	}
}

func TestApprovalIsStampedWithTheRequesterAndMovesTheTriageToApproved(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	approval := decision(proposed, v1alpha1.DecisionApproved, "memory limit looks right")
	approval.Spec.Approver = forgedUser // a client-supplied identity must be overwritten
	if err := alice.Create(t.Context(), approval); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approval.Spec.Approver != aliceUser {
		t.Errorf("approver in the created TriageApproval = %q, want %q", approval.Spec.Approver, aliceUser)
	}
	var stored v1alpha1.TriageApproval
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(approval), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Approver != aliceUser {
		t.Errorf("stored approver = %q, want %q", stored.Spec.Approver, aliceUser)
	}

	tr := env.WaitForApproved(t, proposed.Name)
	got := tr.Status.Decision
	if got == nil || got.Outcome != v1alpha1.DecisionApproved || got.Approver != aliceUser ||
		got.Reason != "memory limit looks right" || !got.DecidedAt.Time.Equal(testenv.StartTime) {
		t.Errorf("decision = %+v, want Approved by %s at %v with the reason", got, aliceUser, testenv.StartTime)
	}
	if tr.Status.Proposal.Hash != proposed.Status.Proposal.Hash {
		t.Errorf("the Approval changed the Proposal's hash")
	}
}

// TestApprovalWorksFromAPlainManifest is what `kubectl create -f` or
// `kubectl apply -f` of a hand-written TriageApproval does: it sends the
// manifest as written, without the plugin.
func TestApprovalWorksFromAPlainManifest(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	manifest := fmt.Sprintf(`apiVersion: pagerless.dev/v1alpha1
kind: TriageApproval
metadata:
  name: %s
  namespace: %s
spec:
  triage: %s
  proposalHash: %s
  decision: Approved
  reason: applied with kubectl
`, v1alpha1.TriageApprovalName(proposed, proposed.Status.Proposal.Hash),
		testenv.Namespace, proposed.Name, proposed.Status.Proposal.Hash)
	var obj unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(manifest), &obj.Object); err != nil {
		t.Fatal(err)
	}
	if err := alice.Create(t.Context(), &obj); err != nil {
		t.Fatalf("create the manifest: %v", err)
	}
	tr := env.WaitForApproved(t, proposed.Name)
	if got := tr.Status.Decision; got == nil || got.Approver != aliceUser || got.Reason != "applied with kubectl" {
		t.Errorf("decision = %+v, want an Approval by %s from the manifest", got, aliceUser)
	}
}

func TestRejectionIsFinalAndKeepsItsReason(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	rejection := decision(proposed, v1alpha1.DecisionRejected, "the limit should be 1Gi")
	if err := alice.Create(t.Context(), rejection); err != nil {
		t.Fatalf("reject: %v", err)
	}
	tr := env.WaitForPhase(t, proposed.Name, v1alpha1.PhaseRejected)
	got := tr.Status.Decision
	if got == nil || got.Outcome != v1alpha1.DecisionRejected || got.Approver != aliceUser ||
		got.Reason != "the limit should be 1Gi" {
		t.Errorf("decision = %+v, want a Rejection by %s with its reason", got, aliceUser)
	}

	t.Run("a Rejection cannot be reversed by an Approval", func(t *testing.T) {
		err := alice.Create(t.Context(), decision(tr, v1alpha1.DecisionApproved, ""))
		requireRefusal(t, err, "Rejected")
	})
	t.Run("the Workload can be triaged again", func(t *testing.T) {
		env.CreateTriage(t, testenv.NewTriage(checkout))
	})
}

func TestAcknowledgingANoActionProposalMovesTheTriageToDiagnosed(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	agent := env.StartAgent(t, created.Name)
	result, err := agent.API.PostReport(t.Context(), noActionReport())
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v", result, err)
	}
	proposed := env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	alice := approverClient(t, env, aliceUser)

	if err := alice.Create(t.Context(), decision(proposed, v1alpha1.DecisionApproved, "")); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseDiagnosed)
	if got := tr.Status.Decision; got == nil || got.Outcome != v1alpha1.DecisionApproved || got.Approver != aliceUser {
		t.Errorf("decision = %+v, want an acknowledgment by %s", got, aliceUser)
	}
}

func TestDecisionsThatDoNotBindToTheCurrentProposalAreRefused(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	for _, name := range []string{"mismatch", "elsewhere", "elsewhere-other", "stale", "investigating"} {
		deploy(t, env, workload{name: name})
	}
	alice := approverClient(t, env, aliceUser)

	t.Run("a hash that is not the Proposal's", func(t *testing.T) {
		t.Parallel()
		proposed := proposedTriage(t, env, "mismatch")
		bad := decision(proposed, v1alpha1.DecisionApproved, "")
		bad.Spec.ProposalHash = absentHash
		bad.Name = v1alpha1.TriageApprovalName(proposed, absentHash)
		requireRefusal(t, alice.Create(t.Context(), bad), "does not match")
		requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
	})

	t.Run("the hash of another Triage's Proposal", func(t *testing.T) {
		t.Parallel()
		mine := proposedTriage(t, env, "elsewhere")
		other := proposedTriage(t, env, "elsewhere-other")
		bad := decision(mine, v1alpha1.DecisionApproved, "")
		bad.Spec.ProposalHash = other.Status.Proposal.Hash
		bad.Name = v1alpha1.TriageApprovalName(mine, bad.Spec.ProposalHash)
		requireRefusal(t, alice.Create(t.Context(), bad), "does not match")
		requireUndecided(t, env, mine.Name, v1alpha1.PhaseProposed)
	})

	t.Run("a Triage that is not Proposed", func(t *testing.T) {
		t.Parallel()
		created := env.CreateTriage(t, testenv.NewTriage("investigating"))
		env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)
		err := alice.Create(t.Context(), decision(created, v1alpha1.DecisionApproved, ""))
		requireRefusal(t, err, "Investigating")
		requireUndecided(t, env, created.Name, v1alpha1.PhaseInvestigating)
	})

	t.Run("a Triage that does not exist", func(t *testing.T) {
		t.Parallel()
		ghost := &v1alpha1.Triage{Name: "ghost"}
		requireRefusal(t, alice.Create(t.Context(), decision(ghost, v1alpha1.DecisionApproved, "")), "not found")
	})

	t.Run("a Stale Proposal", func(t *testing.T) {
		t.Parallel()
		proposed := proposedTriage(t, env, "stale")
		d := liveDeployment(t, env, "stale")
		d.Spec.Replicas = new(int32(2))
		if err := env.Client.Update(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, alice.Create(t.Context(), decision(proposed, v1alpha1.DecisionApproved, "")), "stale")
		requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
	})
}

func TestApprovalOfAnExpiredProposalIsRefusedBeforeTheTriageIsMarkedExpired(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	// Drive admission's clock independently so the stored Triage is still Proposed.
	// Advancing the shared clock would race the controller's first Proposed reconciliation.
	expiredClock := clocktesting.NewFakeClock(proposed.Status.Proposal.ExpiresAt.Time)
	validator := admission.ApprovalValidator{
		Reader: env.Client, Namespace: testenv.Namespace, Clock: expiredClock,
		Proposals: proposal.NewPipeline(expiredClock, nil),
	}
	approval := decision(proposed, v1alpha1.DecisionApproved, "")
	approval.Spec.Approver = aliceUser
	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	response := validator.Handle(t.Context(), webhookadmission.Request{
		Operation: admissionv1.Create, Namespace: testenv.Namespace,
		Object: runtime.RawExtension{Raw: raw}, UserInfo: authenticationv1.UserInfo{Username: aliceUser},
	})
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "expired") {
		t.Fatalf("expired admission response = %+v, want an expiry refusal", response)
	}
	requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)

	// The actual API path must also refuse the decision, regardless of whether
	// the independent expiry controller has already advanced the phase.
	env.Clock.Step(time.Hour)
	err = alice.Create(t.Context(), decision(proposed, v1alpha1.DecisionApproved, ""))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "expired") {
		t.Fatalf("expired API decision = %v, want an expiry refusal", err)
	}
	after := env.Get(t, proposed.Name)
	if after.Status.Decision != nil ||
		(after.Status.Phase != v1alpha1.PhaseProposed && after.Status.Phase != v1alpha1.PhaseExpired) {
		t.Fatalf("after expiry refusal: phase = %s, decision = %+v", after.Status.Phase, after.Status.Decision)
	}
	var stored v1alpha1.TriageApprovalList
	if err := env.Client.List(t.Context(), &stored, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(stored.Items) != 0 {
		t.Fatalf("expired decisions stored = %d, want none", len(stored.Items))
	}
}

func TestApprovalOfARevalidatedProposalNeedsTheNewHash(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithProposalPollInterval(proposalPoll))
	deploy(t, env, workload{name: checkout})
	before := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	d := liveDeployment(t, env, checkout)
	d.Spec.Replicas = new(int32(2))
	if err := env.Client.Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	after := revalidated(t, env, before.Name, before.Status.Proposal.Hash)

	stale := decision(before, v1alpha1.DecisionApproved, "")
	requireRefusal(t, alice.Create(t.Context(), stale), "does not match")
	if err := alice.Create(t.Context(), decision(after, v1alpha1.DecisionApproved, "")); err != nil {
		t.Fatalf("approve the re-validated Proposal: %v", err)
	}
	env.WaitForApproved(t, before.Name)
}

func TestADecisionIsAppliedOnceAndCannotBeReplayed(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)
	bob := approverClient(t, env, bobUser)

	if err := alice.Create(t.Context(), decision(proposed, v1alpha1.DecisionApproved, "")); err != nil {
		t.Fatal(err)
	}
	approved := env.WaitForApproved(t, proposed.Name)

	t.Run("the same Approval again", func(t *testing.T) {
		again := decision(proposed, v1alpha1.DecisionApproved, "")
		if err := bob.Create(t.Context(), again); !apierrors.IsAlreadyExists(err) {
			t.Errorf("repeated Approval returned %v, want AlreadyExists", err)
		}
	})
	t.Run("a Rejection after an Approval", func(t *testing.T) {
		reversal := decision(proposed, v1alpha1.DecisionRejected, "changed my mind")
		if err := bob.Create(t.Context(), reversal); !apierrors.IsAlreadyExists(err) {
			t.Errorf("opposing decision returned %v, want AlreadyExists", err)
		}
	})
	got := env.Get(t, proposed.Name).Status
	// The server hands an Approved Triage to the Executor, so it may already be Executing.
	decided := got.Phase == v1alpha1.PhaseApproved || got.Phase == v1alpha1.PhaseExecuting
	if !decided || got.Decision == nil || *got.Decision != *approved.Status.Decision {
		t.Errorf("status after replays = %+v, want the first decision unchanged", got)
	}
}

func TestSimultaneousDecisionsApplyExactlyOne(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	outcomes := []v1alpha1.DecisionOutcome{v1alpha1.DecisionApproved, v1alpha1.DecisionRejected}
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			results <- alice.Create(context.WithoutCancel(t.Context()), decision(proposed, outcomes[i%2], "race"))
		})
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !apierrors.IsAlreadyExists(err) {
			t.Errorf("a competing decision returned %v, want AlreadyExists", err)
		}
	}
	if created != 1 {
		t.Fatalf("%d decisions were created for the same Proposal, want exactly one", created)
	}

	tr := env.WaitFor(t, proposed.Name, "a decision is applied", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Phase != v1alpha1.PhaseProposed
	})
	// The server hands an Approved Triage to the Executor, so an Approval may already show as Executing.
	matches := func(status v1alpha1.TriageStatus) bool {
		switch {
		case status.Decision == nil:
			return false
		case status.Decision.Outcome == v1alpha1.DecisionApproved:
			return status.Phase == v1alpha1.PhaseApproved || status.Phase == v1alpha1.PhaseExecuting
		default:
			return status.Phase == v1alpha1.PhaseRejected
		}
	}
	first := tr.Status.Decision
	if !matches(tr.Status) {
		t.Fatalf("phase = %s, decision = %+v, want a phase that matches the decision", tr.Status.Phase, first)
	}
	time.Sleep(500 * time.Millisecond)
	if again := env.Get(t, proposed.Name).Status; !matches(again) || *again.Decision != *first {
		t.Errorf("the decision changed from %+v (%s) to %+v (%s)", first, tr.Status.Phase, again.Decision, again.Phase)
	}
	var decisions v1alpha1.TriageApprovalList
	if err := env.Client.List(t.Context(), &decisions, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(decisions.Items) != 1 {
		t.Errorf("stored %d TriageApprovals for a single Proposal, want one", len(decisions.Items))
	}
}

func TestCustomOrGeneratedDecisionNamesCannotBypassTheProposalDecisionSlot(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	for _, generated := range []bool{false, true} {
		approval := decision(proposed, v1alpha1.DecisionApproved, "")
		approval.Name, approval.GenerateName = "custom-decision", ""
		if generated {
			approval.Name, approval.GenerateName = "", "custom-decision-"
		}
		requireRefusal(t, alice.Create(t.Context(), approval), "deterministic")
		requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
	}
}

func TestTriageApprovalIsImmutable(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)
	// An administrator may update anything RBAC allows, so only the CRD's rule can hold these back.
	approval := decision(proposed, v1alpha1.DecisionRejected, "no")
	if err := alice.Create(t.Context(), approval); err != nil {
		t.Fatal(err)
	}

	changes := map[string]func(*v1alpha1.TriageApproval){
		"decision": func(a *v1alpha1.TriageApproval) { a.Spec.Decision = v1alpha1.DecisionApproved },
		"hash":     func(a *v1alpha1.TriageApproval) { a.Spec.ProposalHash = absentHash },
		"triage":   func(a *v1alpha1.TriageApproval) { a.Spec.Triage = "another" },
		"reason":   func(a *v1alpha1.TriageApproval) { a.Spec.Reason = "yes" },
		"approver": func(a *v1alpha1.TriageApproval) { a.Spec.Approver = forgedUser },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			var latest v1alpha1.TriageApproval
			if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(approval), &latest); err != nil {
				t.Fatal(err)
			}
			change(&latest)
			requireRefusal(t, env.Client.Update(t.Context(), &latest), "spec is immutable")
		})
	}
}

func TestTriageApprovalSchemaRefusesMalformedDecisions(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	tests := map[string]func(*v1alpha1.TriageApproval){
		"unknown decision": func(a *v1alpha1.TriageApproval) { a.Spec.Decision = "Maybe" },
		"short hash":       func(a *v1alpha1.TriageApproval) { a.Spec.ProposalHash = "abc123" },
		"uppercase hash": func(a *v1alpha1.TriageApproval) {
			a.Spec.ProposalHash = strings.ToUpper(proposed.Status.Proposal.Hash)
		},
		"no Triage": func(a *v1alpha1.TriageApproval) { a.Spec.Triage = "" },
		"long reason": func(a *v1alpha1.TriageApproval) {
			a.Spec.Reason = strings.Repeat("x", v1alpha1.MaxReasonLength+1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			bad := decision(proposed, v1alpha1.DecisionApproved, "")
			mutate(bad)
			err := alice.Create(t.Context(), bad)
			if !apierrors.IsInvalid(err) {
				t.Errorf("error = %v, want an Invalid refusal", err)
			}
		})
	}
	requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
}

func TestApprovalOutsideTheServerNamespaceIsRefused(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)

	approval := decision(proposed, v1alpha1.DecisionApproved, "")
	approval.Namespace = metav1.NamespaceDefault
	requireRefusal(t, env.Client.Create(t.Context(), approval), testenv.Namespace)
	requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
}

func TestForgedApproverIsRefusedWhenTheStampingWebhookIsMissing(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	// Without the mutating webhook nothing overwrites a client-supplied identity,
	// so the validating webhook must refuse a value that is not the requester's.
	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{
		Name: server.DefaultServerServiceAccount,
	}
	if err := env.Client.Delete(t.Context(), mutating); err != nil {
		t.Fatalf("delete the mutating webhook configuration: %v", err)
	}
	forged := decision(proposed, v1alpha1.DecisionApproved, "")
	forged.Spec.Approver = bobUser
	requireRefusal(t, alice.Create(t.Context(), forged), "approver")
	unstamped := decision(proposed, v1alpha1.DecisionApproved, "")
	requireRefusal(t, alice.Create(t.Context(), unstamped), "approver")
	requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)

	honest := decision(proposed, v1alpha1.DecisionApproved, "")
	honest.Spec.Approver = aliceUser
	if err := alice.Create(t.Context(), honest); err != nil {
		t.Fatalf("a TriageApproval that carries the requester's own identity was refused: %v", err)
	}
}

func TestOnlyApproversMayCreateTriageApprovals(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)

	t.Run("a user without the Role is forbidden", func(t *testing.T) {
		stranger := env.ClientAs(t, "stranger@example.com")
		err := stranger.Create(t.Context(), decision(proposed, v1alpha1.DecisionApproved, ""))
		if !apierrors.IsForbidden(err) {
			t.Errorf("error = %v, want Forbidden", err)
		}
		requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
	})

	t.Run("an approver may decide but not write status or Triages", func(t *testing.T) {
		env.GrantApprover(t, aliceUser)
		if !env.Can(t, aliceUser, nil, "create", pagerlessGroup, approvalsKind, "") {
			t.Errorf("an approver may not create TriageApprovals")
		}
		for _, verb := range writeVerbs {
			if env.Can(t, aliceUser, nil, verb, pagerlessGroup, triagesResource, "status") {
				t.Errorf("an approver may %s triages/status", verb)
			}
			if env.Can(t, aliceUser, nil, verb, pagerlessGroup, triagesResource, "") {
				t.Errorf("an approver may %s triages", verb)
			}
		}
		for _, verb := range writeVerbs[1:] {
			if env.Can(t, aliceUser, nil, verb, pagerlessGroup, approvalsKind, "") {
				t.Errorf("an approver may %s TriageApprovals after creating them", verb)
			}
		}
	})
}

func TestTriageApprovalIsUsableFromKubectl(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)
	if err := alice.Create(t.Context(), decision(proposed, v1alpha1.DecisionApproved, "")); err != nil {
		t.Fatal(err)
	}

	resources, err := env.Clientset.Discovery().ServerResourcesForGroupVersion(v1alpha1.GroupVersion.String())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range resources.APIResources {
		if r.Name == approvalsKind {
			found = true
			if len(r.ShortNames) != 1 || r.ShortNames[0] != "tra" {
				t.Errorf("short names = %v, want [tra]", r.ShortNames)
			}
		}
	}
	if !found {
		t.Fatal("the triageapprovals resource is not served")
	}
	table := env.Table(t, approvalsKind)
	names := make([]string, 0, len(table.ColumnDefinitions))
	for _, c := range table.ColumnDefinitions {
		names = append(names, c.Name)
	}
	if want := "Name,Triage,Decision,Approver,Age"; strings.Join(names, ",") != want {
		t.Errorf("columns = %v, want %s", names, want)
	}
	if len(table.Rows) != 1 || table.Rows[0].Cells[1] != proposed.Name || table.Rows[0].Cells[2] != "Approved" ||
		table.Rows[0].Cells[3] != aliceUser {
		t.Errorf("rows = %+v, want one Approval of %s by %s", table.Rows, proposed.Name, aliceUser)
	}
}
