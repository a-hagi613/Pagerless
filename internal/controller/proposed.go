package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/proposal"
)

// DefaultProposalPollInterval is how often a Proposed Triage is checked for a
// Stale Proposal when nothing else wakes the reconciler. The Workload and the
// Sandbox are outside the Triage's namespace, so no event announces a change.
const DefaultProposalPollInterval = 30 * time.Second

// watchProposal ends a Triage whose Proposal went unapproved for an hour, and
// re-validates a Stale Proposal under a new hash. It returns when to look again.
func (r *TriageReconciler) watchProposal(ctx context.Context, t *v1alpha1.Triage) (ctrl.Result, error) {
	current := t.Status.Proposal
	if current == nil {
		return ctrl.Result{}, nil
	}
	remaining := current.ExpiresAt.Sub(r.Clock.Now())
	if remaining <= 0 {
		return ctrl.Result{}, r.expire(ctx, client.ObjectKeyFromObject(t), current.Hash)
	}
	reason, err := r.Proposals.Stale(ctx, t)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the Proposal for staleness: %w", err)
	}
	if reason != "" {
		if err := r.revalidate(ctx, t, reason); err != nil {
			return ctrl.Result{}, err
		}
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = DefaultProposalPollInterval
	}
	return ctrl.Result{RequeueAfter: min(interval, remaining)}, nil
}

// expire moves the Triage to Expired, unless a decision has since replaced the Proposal that expired.
func (r *TriageReconciler) expire(ctx context.Context, key client.ObjectKey, hash string) error {
	_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
		p := latest.Status.Proposal
		if latest.Status.Phase != v1alpha1.PhaseProposed || p == nil || p.Hash != hash {
			return nil
		}
		if r.Clock.Now().Before(p.ExpiresAt.Time) {
			return nil
		}
		latest.Status.Phase = v1alpha1.PhaseExpired
		return nil
	})
	return err
}

// revalidate replaces a Stale Proposal with the same Action validated against
// the present base, under a new hash. If Validation now refuses it, the Triage
// ends as Invalid, because no Proposal can be approved.
func (r *TriageReconciler) revalidate(ctx context.Context, t *v1alpha1.Triage, reason string) error {
	old := t.Status.Proposal
	rebuilt, errs, err := r.Proposals.Revalidate(ctx, t)
	if err != nil {
		return fmt.Errorf("re-validate the Stale Proposal: %w", err)
	}
	_, err = r.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Triage) error {
		p := latest.Status.Proposal
		if latest.Status.Phase != v1alpha1.PhaseProposed || p == nil || p.Hash != old.Hash {
			return nil
		}
		if len(errs) > 0 {
			latest.Status.Phase = v1alpha1.PhaseInvalid
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:   v1alpha1.ConditionInvalid,
				Status: metav1.ConditionTrue,
				Reason: v1alpha1.ReasonStaleProposalInvalid,
				Message: fmt.Sprintf("the Proposal became stale (%s) and failed Validation again: %s",
					reason, proposal.Summary(errs)),
				ObservedGeneration: latest.Generation,
			})
			return nil
		}
		latest.Status.Proposal = rebuilt
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:   v1alpha1.ConditionProposalRevalidated,
			Status: metav1.ConditionTrue,
			Reason: reason,
			Message: fmt.Sprintf("the Proposal became stale and was re-validated; its hash changed from %s to %s",
				old.Hash, rebuilt.Hash),
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	if err == nil {
		logf.FromContext(ctx).Info("re-validated a Stale Proposal", "triage", t.Name, "reason", reason)
	}
	return err
}
