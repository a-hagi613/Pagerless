package cli

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// loadProposedTriage returns the named Triage, which must be Proposed so that
// it has a Proposal for a decision to bind to.
func loadProposedTriage(ctx context.Context, cl client.Client, name string) (*v1alpha1.Triage, error) {
	var tr v1alpha1.Triage
	if err := cl.Get(ctx, client.ObjectKey{Namespace: TriageNamespace, Name: name}, &tr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("triage %q not found in namespace %q", name, TriageNamespace)
		}
		return nil, fmt.Errorf("get Triage %s: %w", name, err)
	}
	if tr.Status.Phase != v1alpha1.PhaseProposed || tr.Status.Proposal == nil {
		return nil, fmt.Errorf("triage %s is in phase %s; only a Proposed Triage accepts a decision",
			name, phaseOrUnset(tr.Status.Phase))
	}
	return &tr, nil
}

func phaseOrUnset(phase v1alpha1.Phase) string {
	if phase == "" {
		return "unset"
	}
	return string(phase)
}

// submitDecision creates the TriageApproval that decides the Triage's current
// Proposal. The approver is not set here: the server stamps it from the
// identity that the kubeconfig authenticates as.
func submitDecision(
	ctx context.Context, cl client.Client, tr *v1alpha1.Triage, outcome v1alpha1.DecisionOutcome, reason string,
) (*v1alpha1.TriageApproval, error) {
	verb := "approve"
	if outcome == v1alpha1.DecisionRejected {
		verb = "reject"
	}
	approval := &v1alpha1.TriageApproval{
		Name: v1alpha1.TriageApprovalName(tr, tr.Status.Proposal.Hash), Namespace: TriageNamespace,
		Spec: v1alpha1.TriageApprovalSpec{
			Triage:       tr.Name,
			ProposalHash: tr.Status.Proposal.Hash,
			Decision:     outcome,
			Reason:       reason,
		},
	}
	if err := cl.Create(ctx, approval); err != nil {
		return nil, fmt.Errorf("%s triage %s: %w", verb, tr.Name, err)
	}
	return approval, nil
}

// approvalRef returns the resource reference of a TriageApproval, as kubectl prints it.
func approvalRef(name string) string {
	return "triageapproval." + v1alpha1.GroupVersion.Group + "/" + name
}
