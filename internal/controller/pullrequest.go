package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/page"
	"github.com/a-hagi613/pagerless/internal/publish"
)

// DefaultPullRequestPollInterval is how often an open pull request is checked for a merge.
const DefaultPullRequestPollInterval = 10 * time.Second

// maxConditionMessage bounds a failure message quoted in a condition.
const maxConditionMessage = 500

// Publisher publishes an approved Proposal as a pull request and reports the pull request's state.
type Publisher interface {
	// Publish commits the Proposal to a branch, opens or finds its pull request, and reports the state.
	Publish(ctx context.Context, p *v1alpha1.Page) (publish.Result, error)
}

// PullRequestReconciler publishes an Approved Proposal as a pull request, moves
// the Page to PullRequestOpen, and records the pull request and, once it merges,
// the merge commit. It writes nothing but status (ADR-0001).
//
// Verification takes over once the merge commit is recorded.
type PullRequestReconciler struct {
	// Client reads Pages.
	Client client.Client
	// Status is the only writer of Page status.
	Status *page.StatusStore
	// Publisher carries out the Proposals.
	Publisher Publisher
	// Clock stamps the pull request's times.
	Clock clock.PassiveClock
	// PollInterval is how often an open pull request is checked for a merge. It
	// defaults to DefaultPullRequestPollInterval.
	PollInterval time.Duration
}

// SetupWithManager registers the reconciler for Pages.
func (r *PullRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Page{}).
		Named("pullrequest").
		// The test harness runs several managers in one process, which
		// controller-runtime's process-wide name check would refuse.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		Complete(r)
}

// Reconcile advances one Page that is Approved or PullRequestOpen.
func (r *PullRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcile(ctx, req)
	if err != nil && ctx.Err() != nil {
		// The server is stopping, so the failure is not worth reporting.
		return ctrl.Result{}, nil
	}
	return result, err
}

func (r *PullRequestReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Page
	if err := r.Client.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	current := p.Status.Proposal
	if !p.DeletionTimestamp.IsZero() || current == nil || !current.Action.DeliveredByPullRequest() {
		return ctrl.Result{}, nil
	}
	switch p.Status.Phase {
	case v1alpha1.PhaseApproved:
		// PullRequestOpen first, so that a crash between here and the
		// publication leaves a Page that is visibly mid-delivery and is retried,
		// and so that the publication is idempotent.
		if _, err := r.Status.Update(ctx, req.NamespacedName, func(latest *v1alpha1.Page) error {
			if latest.Status.Phase == v1alpha1.PhaseApproved {
				latest.Status.Phase = v1alpha1.PhasePullRequestOpen
			}
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
		return r.publish(ctx, req.NamespacedName)
	case v1alpha1.PhasePullRequestOpen:
		return r.publish(ctx, req.NamespacedName)
	default:
		return ctrl.Result{}, nil
	}
}

// publish publishes the Proposal and records the result. It reads the Page
// directly so that it acts on the phase it has just written.
func (r *PullRequestReconciler) publish(ctx context.Context, key client.ObjectKey) (ctrl.Result, error) {
	var p v1alpha1.Page
	if err := r.Status.Reader.Get(ctx, key, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if p.Status.Phase != v1alpha1.PhasePullRequestOpen || p.Status.Proposal == nil {
		return ctrl.Result{}, nil
	}
	if p.Status.PullRequest != nil && p.Status.PullRequest.MergeCommit != "" {
		return ctrl.Result{}, nil
	}
	winner, _, err := page.CanonicalActive(ctx, r.Status.Reader, p.Namespace, p.Spec.Fingerprint)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the canonical Active Page before publication: %w", err)
	}
	if winner == nil || winner.UID != p.UID {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx).WithValues("page", key.Name)

	result, err := r.Publisher.Publish(ctx, &p)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("publish the Proposal: %w", err)
	}
	if result.Closed {
		log.Info("the pull request closed without merging", "pullRequest", result.PullRequestURL)
		return ctrl.Result{}, r.fail(ctx, key, v1alpha1.ReasonPullRequestClosed,
			"the pull request "+result.PullRequestURL+" was closed without merging")
	}
	if err := r.record(ctx, key, result); err != nil {
		return ctrl.Result{}, err
	}
	if result.Merged {
		log.Info("the pull request merged", "pullRequest", result.PullRequestURL, "mergeCommit", result.MergeCommit)
		return ctrl.Result{}, nil
	}
	interval := r.PollInterval
	if interval == 0 {
		interval = DefaultPullRequestPollInterval
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// record writes the pull request, and the merge commit once merged.
func (r *PullRequestReconciler) record(ctx context.Context, key client.ObjectKey, result publish.Result) error {
	_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhasePullRequestOpen {
			return nil
		}
		now := metav1.NewTime(r.Clock.Now())
		if latest.Status.PullRequest == nil {
			latest.Status.PullRequest = &v1alpha1.PullRequest{
				Provider: "GitHub",
				State:    v1alpha1.PullRequestOpen,
				OpenedAt: &now,
			}
		}
		pullRequest := latest.Status.PullRequest
		pullRequest.URL = result.PullRequestURL
		pullRequest.Branch = result.Branch
		pullRequest.Number = int32(result.PullRequestNumber)
		if result.Merged {
			pullRequest.State = v1alpha1.PullRequestMerged
			pullRequest.MergeCommit = result.MergeCommit
			pullRequest.MergedAt = &now
		}
		reason := v1alpha1.ReasonPullRequestOpen
		if result.Merged {
			reason = v1alpha1.ReasonMerged
		}
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionDelivered,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            result.PullRequestURL,
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	return err
}

// fail ends a PullRequestOpen Page as Failed, with the Proposal and the pull
// request preserved.
func (r *PullRequestReconciler) fail(ctx context.Context, key client.ObjectKey, reason, message string) error {
	if len(message) > maxConditionMessage {
		message = message[:maxConditionMessage] + "..."
	}
	_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhasePullRequestOpen {
			return nil
		}
		latest.Status.Phase = v1alpha1.PhaseFailed
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionFailed,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	return err
}
