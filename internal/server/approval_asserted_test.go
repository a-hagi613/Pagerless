package server_test

import (
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// The users that the Keycloak membership stand-in reports, and one who is not in the group.
const (
	dashboardApprover = "alice"
	nonApprover       = "mallory"
)

// serverIdentity returns a client that the API server authenticates as
// pagerless-server's own service account, which is the dashboard's write identity.
func serverIdentity(t *testing.T, env *testenv.Env) client.Client {
	t.Helper()
	return env.ClientAs(t, forgedUser)
}

func TestAssertedApproverIsAcceptedFromPagerlessServerForAGroupMember(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithApprovers(dashboardApprover))
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)

	approval := decision(proposed, v1alpha1.DecisionApproved, "the memory limit is right")
	approval.Spec.Approver = dashboardApprover
	if err := serverIdentity(t, env).Create(t.Context(), approval); err != nil {
		t.Fatalf("pagerless-server asserted %s and the approval was refused: %v", dashboardApprover, err)
	}
	tr := env.WaitForApproved(t, proposed.Name)
	got := tr.Status.Decision
	if got == nil || got.Outcome != v1alpha1.DecisionApproved || got.Approver != dashboardApprover {
		t.Errorf("decision = %+v, want an Approval by the asserted user %s", got, dashboardApprover)
	}
	var stored v1alpha1.TriageApproval
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(approval), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Approver != dashboardApprover {
		t.Errorf("stored approver = %q, want the asserted %q", stored.Spec.Approver, dashboardApprover)
	}
}

func TestAssertedApproverIsRefusedForANonGroupMember(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithApprovers(dashboardApprover))
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)

	approval := decision(proposed, v1alpha1.DecisionApproved, "")
	approval.Spec.Approver = nonApprover
	err := serverIdentity(t, env).Create(t.Context(), approval)
	requireRefusal(t, err, "not a member")
	requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
}

func TestPagerlessServerMayNotAssertItself(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)

	approval := decision(proposed, v1alpha1.DecisionApproved, "")
	approval.Spec.Approver = forgedUser
	err := serverIdentity(t, env).Create(t.Context(), approval)
	requireRefusal(t, err, "not itself")
	requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
}

// TestANonServerRequesterCannotAssertAnotherApprover removes the stamping
// webhook, so the validating webhook must refuse the assertion itself rather
// than rely on the overwrite (ADR-0020).
func TestANonServerRequesterCannotAssertAnotherApprover(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithApprovers(dashboardApprover))
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	deleteMutatingWebhook(t, env)

	asserted := decision(proposed, v1alpha1.DecisionApproved, "")
	asserted.Spec.Approver = dashboardApprover
	err := alice.Create(t.Context(), asserted)
	requireRefusal(t, err, "only pagerless-server may assert an approver")

	unstamped := decision(proposed, v1alpha1.DecisionApproved, "")
	requireRefusal(t, alice.Create(t.Context(), unstamped), "empty")

	requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
}

// TestStampedIdentityIsUnchangedForAClobberedAssertion keeps the Phase 1
// behaviour: a plain user who supplies another name is recorded as itself.
func TestStampedIdentityIsUnchangedForAClobberedAssertion(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithApprovers(dashboardApprover))
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	alice := approverClient(t, env, aliceUser)

	approval := decision(proposed, v1alpha1.DecisionApproved, "")
	approval.Spec.Approver = dashboardApprover // the stamping webhook overwrites this
	if err := alice.Create(t.Context(), approval); err != nil {
		t.Fatalf("a plain approver was refused: %v", err)
	}
	tr := env.WaitForApproved(t, proposed.Name)
	if got := tr.Status.Decision; got == nil || got.Approver != aliceUser {
		t.Errorf("decision = %+v, want the requester %s recorded", got, aliceUser)
	}
}

// deleteMutatingWebhook removes the TriageApproval stamping webhook, as a
// misconfigured or bypassed installation would.
func deleteMutatingWebhook(t *testing.T, env *testenv.Env) {
	t.Helper()
	config := &admissionregistrationv1.MutatingWebhookConfiguration{}
	config.Name = webhookConfigName
	if err := env.Client.Delete(t.Context(), config); err != nil {
		t.Fatalf("delete the mutating webhook configuration: %v", err)
	}
}

const (
	// webhookConfigName is the name of both webhook configurations that the harness installs.
	webhookConfigName = "pagerless-server"
	// admissionDecisionSource is the API's annotation name, repeated here so the
	// test fails if the API changes it without changing the audit contract.
	admissionDecisionSource  = "pagerless.dev/decision-source"
	admissionApproverSubject = "pagerless.dev/approver-subject"
)
