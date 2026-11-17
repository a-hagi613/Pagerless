package controller

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/triage"
)

// ApprovalReconciler applies the decision of a TriageApproval to its Triage.
// It is the only path from a human decision to Triage status, so the status
// stays written by pagerless-server alone (ADR-0001).
//
// The admission webhook has already checked the decision, but the Proposal may
// have changed since, so the reconciler checks again and ignores a decision
// that no longer binds to the current Proposal. Whichever decision it applies
// first wins, because a decided Triage is no longer Proposed.
type ApprovalReconciler struct {
	// Client reads TriageApprovals.
	Client client.Client
	// Status is the only writer of Triage status, and reads Triages directly from the API server.
	Status *triage.StatusStore
	// Proposals reports whether a Proposal has become a Stale Proposal.
	Proposals *proposal.Pipeline
	// Clock timestamps decisions and decides whether a Proposal has expired.
	Clock clock.PassiveClock
}

// SetupWithManager registers the reconciler for TriageApprovals.
func (r *ApprovalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.TriageApproval{}).
		Named("triageapproval").
		// The test harness runs several managers in one process, which
		// controller-runtime's process-wide name check would refuse.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		Complete(r)
}

// Reconcile applies one TriageApproval.
func (r *ApprovalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var approval v1alpha1.TriageApproval
	if err := r.Client.Get(ctx, req.NamespacedName, &approval); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := logf.FromContext(ctx).WithValues("triage", approval.Spec.Triage, "approval", approval.Name)
	if approval.Spec.Approver == "" {
		// The webhooks never admit this, so it is not an approval to trust.
		log.Info("ignoring a TriageApproval that carries no approver")
		return ctrl.Result{}, nil
	}

	key := types.NamespacedName{Namespace: approval.Namespace, Name: approval.Spec.Triage}
	var tr v1alpha1.Triage
	if err := r.Status.Reader.Get(ctx, key, &tr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !binds(&tr, &approval) || !r.Clock.Now().Before(tr.Status.Proposal.ExpiresAt.Time) {
		return ctrl.Result{}, nil
	}
	reason, err := r.Proposals.Stale(ctx, &tr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the Proposal for staleness: %w", err)
	}
	if reason != "" {
		// The Triage reconciler re-validates the Proposal under a new hash, which this decision does not cover.
		log.Info("ignoring a decision on a Proposal that became stale", "reason", reason)
		return ctrl.Result{}, nil
	}

	applied := false
	_, err = r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
		applied = false
		if !binds(latest, &approval) || !r.Clock.Now().Before(latest.Status.Proposal.ExpiresAt.Time) {
			return nil
		}
		latest.Status.Decision = &v1alpha1.Decision{
			Outcome:   approval.Spec.Decision,
			Approver:  approval.Spec.Approver,
			Reason:    approval.Spec.Reason,
			DecidedAt: metav1.NewTime(r.Clock.Now()),
		}
		latest.Status.Phase = phaseAfter(latest.Status.Proposal.Action, approval.Spec.Decision)
		applied = true
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if applied {
		log.Info("applied a decision", "decision", approval.Spec.Decision, "approver", approval.Spec.Approver)
	}
	return ctrl.Result{}, nil
}

// binds reports whether the TriageApproval is a decision on the Triage's current Proposal.
func binds(tr *v1alpha1.Triage, approval *v1alpha1.TriageApproval) bool {
	current := tr.Status.Proposal
	return tr.Status.Phase == v1alpha1.PhaseProposed && current != nil && current.Hash == approval.Spec.ProposalHash
}

// phaseAfter returns the phase a decision moves a Proposed Triage to. An
// Approval of a NoAction Proposal only acknowledges the Diagnosis, so there is
// nothing to execute.
func phaseAfter(action v1alpha1.Action, outcome v1alpha1.DecisionOutcome) v1alpha1.Phase {
	switch {
	case outcome == v1alpha1.DecisionRejected:
		return v1alpha1.PhaseRejected
	case action == v1alpha1.ActionNoAction:
		return v1alpha1.PhaseDiagnosed
	default:
		return v1alpha1.PhaseApproved
	}
}
