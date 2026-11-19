package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// applyDecision records a human decision that the operator expressed as
// annotations on the Page. It is the only path from a decision to Page status,
// so the status stays written by pagerless-server alone (ADR-0001).
//
// A decision binds to one Proposal hash, and it is ignored when it no longer
// matches the current Proposal, when the approval window has closed, or when it
// carries no declared identity. A Page that already holds a decision is no
// longer Proposed, so a second decision is ignored rather than replayed.
func (r *PageReconciler) applyDecision(ctx context.Context, p *v1alpha1.Page) (bool, error) {
	if p.Status.Phase != v1alpha1.PhaseProposed || p.Status.Proposal == nil {
		return false, nil
	}
	raw := p.Annotations[v1alpha1.AnnotationDecision]
	if raw == "" {
		return false, nil
	}
	log := logf.FromContext(ctx).WithValues("page", p.Name)
	outcome, err := parseDecisionAnnotation(raw)
	if err != nil {
		// A malformed annotation is the caller's mistake, not a transient failure:
		// report it once and do not retry the reconcile.
		log.Info("ignoring a malformed decision annotation", "error", err.Error())
		return false, nil
	}
	if hash := p.Annotations[v1alpha1.AnnotationDecisionProposal]; hash != p.Status.Proposal.Hash {
		log.Info("ignoring a decision that does not bind to the current Proposal",
			"decidedFor", hash, "current", p.Status.Proposal.Hash)
		return false, nil
	}
	if !r.Clock.Now().Before(p.Status.Proposal.ExpiresAt.Time) {
		log.Info("ignoring a decision that arrived after the approval window closed")
		return false, nil
	}
	decidedBy := p.Annotations[v1alpha1.AnnotationDecidedBy]
	if decidedBy == "" {
		log.Info("ignoring a decision that declares no identity; set " + v1alpha1.AnnotationDecidedBy)
		return false, nil
	}
	reason, err := r.Proposals.Stale(ctx, p)
	if err != nil {
		return false, fmt.Errorf("check the Proposal for staleness: %w", err)
	}
	if reason != "" {
		// The Page reconciler re-validates the Proposal under a new hash, which
		// this decision does not cover.
		log.Info("ignoring a decision on a Proposal that became stale", "reason", reason)
		return false, nil
	}

	applied := false
	_, err = r.Status.Update(ctx, client.ObjectKeyFromObject(p), func(latest *v1alpha1.Page) error {
		applied = false
		if !bindsDecision(latest, outcome) {
			return nil
		}
		latest.Status.Decision = &v1alpha1.Decision{
			Outcome:      outcome,
			ProposalHash: latest.Status.Proposal.Hash,
			DecidedBy:    decidedBy,
			Reason:       p.Annotations[v1alpha1.AnnotationDecisionReason],
			DecidedAt:    metav1.NewTime(r.Clock.Now()),
		}
		latest.Status.Phase = phaseAfter(latest.Status.Proposal.Action, outcome)
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionDecided,
			Status:             metav1.ConditionTrue,
			Reason:             decidedReason(outcome, latest.Status.Proposal.Action),
			Message:            fmt.Sprintf("%s by %s", outcome, decidedBy),
			ObservedGeneration: latest.Generation,
		})
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if applied {
		log.Info("applied a decision", "decision", outcome, "decidedBy", decidedBy)
	}
	return applied, nil
}

// parseDecisionAnnotation maps the annotation value to a DecisionOutcome.
func parseDecisionAnnotation(value string) (v1alpha1.DecisionOutcome, error) {
	switch value {
	case v1alpha1.DecisionAnnotationApprove:
		return v1alpha1.DecisionApproved, nil
	case v1alpha1.DecisionAnnotationReject:
		return v1alpha1.DecisionRejected, nil
	default:
		return "", fmt.Errorf("annotation %s must be %q or %q, not %q", v1alpha1.AnnotationDecision,
			v1alpha1.DecisionAnnotationApprove, v1alpha1.DecisionAnnotationReject, value)
	}
}

// bindsDecision reports whether the Page is still waiting for this decision.
func bindsDecision(p *v1alpha1.Page, outcome v1alpha1.DecisionOutcome) bool {
	return p.Status.Phase == v1alpha1.PhaseProposed && p.Status.Proposal != nil && p.Status.Decision == nil &&
		(outcome == v1alpha1.DecisionApproved || outcome == v1alpha1.DecisionRejected)
}

// phaseAfter returns the phase a decision moves a Proposed Page to. An Approval
// of a NoAction Proposal only acknowledges the Diagnosis, so nothing is
// published.
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

// decidedReason is the Decided condition reason for an applied decision.
func decidedReason(outcome v1alpha1.DecisionOutcome, action v1alpha1.Action) string {
	switch {
	case outcome == v1alpha1.DecisionRejected:
		return v1alpha1.ReasonRejected
	case action == v1alpha1.ActionNoAction:
		return v1alpha1.ReasonAcknowledged
	default:
		return v1alpha1.ReasonApproved
	}
}
