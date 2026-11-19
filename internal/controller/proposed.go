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

// DefaultProposalPollInterval is how often a Proposed Page is checked for a
// Stale Proposal when nothing else wakes the reconciler. The Target and the
// Target Repository are outside the Page's namespace, so no event announces a
// change.
const DefaultProposalPollInterval = 30 * time.Second

// watchProposal ends a Page whose Proposal went unapproved for an hour, and
// re-validates a Stale Proposal under a new hash. It returns when to look again.
func (r *PageReconciler) watchProposal(ctx context.Context, p *v1alpha1.Page) (ctrl.Result, error) {
	current := p.Status.Proposal
	if current == nil {
		return ctrl.Result{}, nil
	}
	remaining := current.ExpiresAt.Sub(r.Clock.Now())
	if remaining <= 0 {
		return ctrl.Result{}, r.expire(ctx, client.ObjectKeyFromObject(p), current.Hash)
	}
	reason, err := r.Proposals.Stale(ctx, p)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the Proposal for staleness: %w", err)
	}
	if reason != "" {
		if err := r.revalidate(ctx, p, reason); err != nil {
			return ctrl.Result{}, err
		}
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = DefaultProposalPollInterval
	}
	return ctrl.Result{RequeueAfter: min(interval, remaining)}, nil
}

// expire moves the Page to Expired, unless a decision has since replaced the
// Proposal that expired.
func (r *PageReconciler) expire(ctx context.Context, key client.ObjectKey, hash string) error {
	_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Page) error {
		p := latest.Status.Proposal
		if latest.Status.Phase != v1alpha1.PhaseProposed || p == nil || p.Hash != hash {
			return nil
		}
		if r.Clock.Now().Before(p.ExpiresAt.Time) {
			return nil
		}
		latest.Status.Phase = v1alpha1.PhaseExpired
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionDecided,
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.ReasonExpired,
			Message:            "no decision arrived inside the approval window",
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	return err
}

// revalidate replaces a Stale Proposal with the same Action validated against
// the present base, under a new hash. If Validation now refuses it, the Page
// ends as Invalid, because no Proposal can be approved.
func (r *PageReconciler) revalidate(ctx context.Context, p *v1alpha1.Page, reason string) error {
	old := p.Status.Proposal
	rebuilt, errs, err := r.Proposals.Revalidate(ctx, p)
	if err != nil {
		return fmt.Errorf("re-validate the Stale Proposal: %w", err)
	}
	_, err = r.Status.Update(ctx, client.ObjectKeyFromObject(p), func(latest *v1alpha1.Page) error {
		current := latest.Status.Proposal
		if latest.Status.Phase != v1alpha1.PhaseProposed || current == nil || current.Hash != old.Hash {
			return nil
		}
		if len(errs) > 0 {
			latest.Status.Phase = v1alpha1.PhaseInvalid
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:   v1alpha1.ConditionFailed,
				Status: metav1.ConditionTrue,
				Reason: v1alpha1.ReasonValidationFailedTwice,
				Message: fmt.Sprintf("the Proposal became stale (%s) and failed Validation again: %s",
					reason, proposal.Summary(errs)),
				ObservedGeneration: latest.Generation,
			})
			return nil
		}
		latest.Status.Proposal = rebuilt
		return nil
	})
	if err == nil {
		logf.FromContext(ctx).Info("re-validated a Stale Proposal", "page", p.Name, "reason", reason)
	}
	return err
}
