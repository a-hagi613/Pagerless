package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/triage"
)

// DefaultExecutionPollInterval is how often the Executor is asked whether an
// open pull request has merged.
const DefaultExecutionPollInterval = 10 * time.Second

// maxConditionMessage bounds a refusal quoted in a condition.
const maxConditionMessage = 500

// Executor is how pagerless-server asks pagerless-executor to carry out a
// Proposal (ADR-0014).
type Executor interface {
	// Execute executes the Triage's Proposal with the hash, or returns the
	// pull request that an earlier call opened for it.
	Execute(ctx context.Context, name, proposalHash string) (*executorapi.Response, error)
}

// ExecutionReconciler hands an Approved Triage to the Executor, moves it to
// Executing, and records the pull request and, once it merges, the merge
// commit. It writes nothing but status (ADR-0001) and holds no credentials
// beyond its own token: the Executor re-checks the Approval before it acts.
//
// Verification takes over once the merge commit is recorded.
type ExecutionReconciler struct {
	// Client reads Triages.
	Client client.Client
	// Status is the only writer of Triage status.
	Status *triage.StatusStore
	// Executor carries out the Proposals.
	Executor Executor
	// Clock timestamps the execution.
	Clock clock.PassiveClock
	// PollInterval is how often an open pull request is checked for a merge. It defaults to
	// DefaultExecutionPollInterval.
	PollInterval time.Duration
}

// SetupWithManager registers the reconciler for Triages.
func (r *ExecutionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Triage{}).
		Named("execution").
		// The test harness runs several managers in one process, which
		// controller-runtime's process-wide name check would refuse.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		Complete(r)
}

// Reconcile advances one Triage that is Approved or Executing.
func (r *ExecutionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcile(ctx, req)
	if err != nil && ctx.Err() != nil {
		// The server is stopping, so the failure is not worth reporting.
		return ctrl.Result{}, nil
	}
	return result, err
}

func (r *ExecutionReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tr v1alpha1.Triage
	if err := r.Client.Get(ctx, req.NamespacedName, &tr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	proposal := tr.Status.Proposal
	if !tr.DeletionTimestamp.IsZero() || proposal == nil ||
		(!proposal.Action.DeliveredByPullRequest() && !proposal.Action.ExecutesLive()) {
		return ctrl.Result{}, nil
	}
	switch tr.Status.Phase {
	case v1alpha1.PhaseApproved:
		// Executing first, so that a crash between here and the request leaves a Triage that is visibly mid-execution
		// and is retried, and so that the request is idempotent.
		if _, err := r.Status.Update(ctx, req.NamespacedName, func(latest *v1alpha1.Triage) error {
			if latest.Status.Phase == v1alpha1.PhaseApproved {
				latest.Status.Phase = v1alpha1.PhaseExecuting
			}
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
		return r.execute(ctx, req.NamespacedName)
	case v1alpha1.PhaseExecuting:
		return r.execute(ctx, req.NamespacedName)
	default:
		return ctrl.Result{}, nil
	}
}

// execute asks the Executor and records its answer. It reads the Triage
// directly so that it acts on the phase it has just written.
func (r *ExecutionReconciler) execute(ctx context.Context, key client.ObjectKey) (ctrl.Result, error) {
	var tr v1alpha1.Triage
	if err := r.Status.Reader.Get(ctx, key, &tr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if tr.Status.Phase != v1alpha1.PhaseExecuting || tr.Status.Proposal == nil {
		return ctrl.Result{}, nil
	}
	if tr.Status.Proposal.Action.DeliveredByPullRequest() {
		if tr.Status.Execution != nil && tr.Status.Execution.MergeCommit != "" {
			return ctrl.Result{}, nil
		}
	} else if tr.Status.Execution != nil && tr.Status.Execution.ExecutedAt != nil {
		// A live Action is complete as soon as its result is recorded; Verification takes over.
		return ctrl.Result{}, nil
	}
	winner, _, err := triage.CanonicalActive(ctx, r.Status.Reader, tr.Namespace, tr.Spec.Workload)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the canonical Active Triage before execution: %w", err)
	}
	if winner == nil || winner.UID != tr.UID {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx).WithValues("triage", key.Name)

	response, err := r.Executor.Execute(ctx, key.Name, tr.Status.Proposal.Hash)
	var refusal *executorapi.Error
	if errors.As(err, &refusal) && refusal.Refused() {
		log.Info("the Executor refused the Proposal", "reason", refusal.Reason, "message", refusal.Message)
		return ctrl.Result{}, r.fail(ctx, key, v1alpha1.ReasonExecutionRefused, refusal.Error())
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ask the Executor to execute: %w", err)
	}
	if response.State == executorapi.PullRequestClosed {
		log.Info("the pull request closed without merging", "pullRequest", response.PullRequestURL)
		return ctrl.Result{}, r.fail(ctx, key, v1alpha1.ReasonPullRequestClosed,
			"the pull request "+response.PullRequestURL+" was closed without merging")
	}
	if err := r.record(ctx, key, response); err != nil {
		return ctrl.Result{}, err
	}
	if response.Live {
		log.Info("executed the Proposal live", "restartedPods", response.RestartedPods)
		return ctrl.Result{}, nil
	}
	if response.State == executorapi.PullRequestMerged {
		log.Info("the pull request merged", "pullRequest", response.PullRequestURL, "mergeCommit", response.MergeCommit)
		return ctrl.Result{}, nil
	}
	interval := r.PollInterval
	if interval == 0 {
		interval = DefaultExecutionPollInterval
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// record writes the pull request, and the merge commit once merged.
func (r *ExecutionReconciler) record(ctx context.Context, key client.ObjectKey, response *executorapi.Response) error {
	_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
		if latest.Status.Phase != v1alpha1.PhaseExecuting {
			return nil
		}
		if latest.Status.Execution == nil {
			now := metav1.NewTime(r.Clock.Now())
			latest.Status.Execution = &v1alpha1.Execution{ExecutedAt: &now}
		}
		if response.Live {
			latest.Status.Execution.RestartedPods = slices.Clone(response.RestartedPods)
			return nil
		}
		latest.Status.Execution.PullRequestURL = response.PullRequestURL
		if response.State == executorapi.PullRequestMerged {
			latest.Status.Execution.MergeCommit = response.MergeCommit
		}
		return nil
	})
	return err
}

// fail ends an Executing Triage as Failed, with the Proposal and the pull
// request preserved.
func (r *ExecutionReconciler) fail(ctx context.Context, key client.ObjectKey, reason, message string) error {
	if len(message) > maxConditionMessage {
		message = message[:maxConditionMessage] + "..."
	}
	_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
		if latest.Status.Phase != v1alpha1.PhaseExecuting {
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
