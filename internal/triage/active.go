package triage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// ErrConflictingApprovals means older data already contains multiple approved Active Triages for a Workload.
// Choosing one would abandon approved or external work, so duplicate resolution must fail closed instead.
var ErrConflictingApprovals = errors.New("multiple approved Active Triages require operator review")

// HasApprovedWork reports whether failing the Triage could abandon a human-approved change or external work.
func HasApprovedWork(tr *v1alpha1.Triage) bool {
	return tr.Status.Phase == v1alpha1.PhaseApproved || tr.Status.Phase == v1alpha1.PhaseExecuting ||
		tr.Status.Phase == v1alpha1.PhaseVerifying || tr.Status.Execution != nil ||
		(tr.Status.Decision != nil && tr.Status.Decision.Outcome == v1alpha1.DecisionApproved)
}

// CanonicalActive reads the API directly through reader and selects one Active Triage for the Workload.
// Existing approved work wins over an unapproved duplicate; otherwise the oldest creation time wins, with
// name and UID as stable tie-breakers. The other unapproved Active Triages are returned as duplicates.
func CanonicalActive(
	ctx context.Context, reader client.Reader, namespace string, workload v1alpha1.WorkloadReference,
) (*v1alpha1.Triage, []v1alpha1.Triage, error) {
	var all v1alpha1.TriageList
	if err := reader.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("read Active Triages: %w", err)
	}
	var active []v1alpha1.Triage
	for _, tr := range all.Items {
		if tr.Spec.Workload == workload && tr.Status.Phase.IsActive() && tr.DeletionTimestamp.IsZero() {
			active = append(active, tr)
		}
	}
	if len(active) == 0 {
		return nil, nil, nil
	}
	if len(active) == 1 {
		return &active[0], nil, nil
	}
	slices.SortFunc(active, func(a, b v1alpha1.Triage) int {
		if order := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); order != 0 {
			return order
		}
		if order := strings.Compare(a.Name, b.Name); order != 0 {
			return order
		}
		return strings.Compare(string(a.UID), string(b.UID))
	})
	var decisions v1alpha1.TriageApprovalList
	for i := range active {
		if active[i].Status.Proposal != nil && !HasApprovedWork(&active[i]) {
			if err := reader.List(ctx, &decisions, client.InNamespace(namespace)); err != nil {
				return nil, nil, fmt.Errorf("read admitted Approvals before duplicate resolution: %w", err)
			}
			break
		}
	}
	winner, approved := 0, 0
	for i := range active {
		if HasApprovedWork(&active[i]) || protectedApproval(&active[i], decisions.Items) {
			winner, approved = i, approved+1
		}
	}
	if approved > 1 {
		return nil, nil, fmt.Errorf("%w for %s %s/%s",
			ErrConflictingApprovals, workload.Kind, workload.Namespace, workload.Name)
	}
	duplicates := make([]v1alpha1.Triage, 0, len(active)-1)
	for i := range active {
		if i != winner {
			duplicates = append(duplicates, active[i])
		}
	}
	return &active[winner], duplicates, nil
}

// protectedApproval observes admitted Approvals before their reconciler ran, including legacy resource names.
// The exact Proposal hash already binds the Triage UID, so a recreated Triage cannot inherit an old Approval.
func protectedApproval(tr *v1alpha1.Triage, decisions []v1alpha1.TriageApproval) bool {
	if tr.Status.Proposal == nil {
		return false
	}
	for _, approval := range decisions {
		if approval.Spec.Approver != "" && approval.Spec.Decision == v1alpha1.DecisionApproved &&
			approval.Spec.Triage == tr.Name && approval.Spec.ProposalHash == tr.Status.Proposal.Hash {
			return true
		}
	}
	return false
}
