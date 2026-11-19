// Package controller reconciles Pages: it starts the agent Job for a new Page,
// records a human decision, publishes an approved Proposal as a pull request,
// and verifies the outcome. Every status write goes through page.StatusStore
// (ADR-0001).
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/contextbundle"
	"github.com/a-hagi613/pagerless/internal/page"
	"github.com/a-hagi613/pagerless/internal/proposal"
)

// PageReconciler drives a Page through Pending and Investigating, applies a
// decision recorded as annotations, supersedes a duplicate, and ends the Page as
// Failed when its agent Job ends without a report.
type PageReconciler struct {
	// Client reads and writes Jobs and reads Pages.
	Client client.Client
	// Scheme is the scheme that knows the Page and Job types.
	Scheme *runtime.Scheme
	// Status is the only writer of Page status.
	Status *page.StatusStore
	// Job configures the agent Job.
	Job AgentJobConfig
	// Proposals validates Proposals again when they become Stale Proposals.
	Proposals *proposal.Pipeline
	// Clock decides when a Proposal has expired and stamps a decision.
	Clock clock.PassiveClock
	// PollInterval is how often a Proposed Page is checked for staleness. It defaults to DefaultProposalPollInterval.
	PollInterval time.Duration
	// Evidence assembles the Page's Context Bundle before its agent Job starts.
	// It is optional; when it is nil the Page starts with no stored bundle.
	Evidence EvidenceCollector
	// Runtime refreshes the model endpoint from the credentials Secret, so a
	// settings write takes effect on the next Page. It is optional.
	Runtime RuntimeSource
}

// EvidenceCollector assembles and stores the Context Bundle of a Page. The
// Context Bundle is gathered once, before the agent's first model call, and a
// Connector failure degrades it rather than failing the Page.
type EvidenceCollector interface {
	Assemble(ctx context.Context, request contextbundle.Request) (*contextbundle.Bundle, error)
}

// SetupWithManager registers the reconciler for Pages and the Jobs it owns.
func (r *PageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Page{}).
		Owns(&batchv1.Job{}).
		Named("page").
		// The test harness runs several managers in one process, which
		// controller-runtime's process-wide name check would refuse.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		Complete(r)
}

// Reconcile advances one Page.
func (r *PageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Page
	if err := r.Status.Reader.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if p.Status.Phase == v1alpha1.PhaseSuperseded {
		return ctrl.Result{}, r.deleteSupersededJob(ctx, &p)
	}
	if p.Status.Phase.IsActive() {
		canonical, err := r.resolveDuplicates(ctx, &p)
		if err != nil || !canonical {
			return ctrl.Result{}, err
		}
	}

	switch p.Status.Phase {
	case "":
		return ctrl.Result{}, r.initialize(ctx, &p)
	case v1alpha1.PhasePending:
		return ctrl.Result{}, r.startInvestigation(ctx, &p)
	case v1alpha1.PhaseInvestigating:
		return ctrl.Result{}, r.watchAgentJob(ctx, &p)
	case v1alpha1.PhaseProposed:
		applied, err := r.applyDecision(ctx, &p)
		if err != nil || applied {
			return ctrl.Result{}, err
		}
		result, err := r.watchProposal(ctx, &p)
		if err != nil && ctx.Err() != nil {
			// The server is stopping, so the failure is not worth reporting.
			return ctrl.Result{}, nil
		}
		return result, err
	default:
		return ctrl.Result{}, nil
	}
}

// initialize puts a new Page in the Pending phase and records that it is the
// canonical Active Page for its Fingerprint.
func (r *PageReconciler) initialize(ctx context.Context, p *v1alpha1.Page) error {
	_, err := r.Status.Update(ctx, client.ObjectKeyFromObject(p), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != "" {
			return nil
		}
		latest.Status.Phase = v1alpha1.PhasePending
		// A repeat Trigger may have joined the Page before it was initialized,
		// so the creating Trigger is added to the count rather than set.
		latest.Status.TriggerCount++
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ReasonCanonical,
			Message:            "this Page is the canonical Active Page for its Fingerprint",
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	return err
}

// startInvestigation creates the agent Job and moves the Page to
// Investigating. Creating the Job first and tolerating an existing one makes
// the step safe to repeat after a crash.
//
// A Page whose investigation is suppressed by a guardrail never starts one, and
// a queued Page waits until the watcher grants a slot; both stay visible with
// their reason on status.investigation.
func (r *PageReconciler) startInvestigation(ctx context.Context, p *v1alpha1.Page) error {
	if investigation := p.Status.Investigation; investigation != nil {
		switch investigation.State {
		case v1alpha1.InvestigationSuppressed, v1alpha1.InvestigationQueued:
			return nil
		}
	}
	// The Context Bundle is gathered before the agent Job exists, so the agent
	// starts from the same evidence the Page records. A failed assembly is a
	// degraded bundle, never a failed Page: the Page proceeds on native logs
	// alone and the operator sees the missing summary.
	evidence := p.Status.Evidence
	if r.Evidence != nil && evidence == nil {
		bundle, err := r.Evidence.Assemble(ctx, contextbundle.Request{
			Page: p.Name, PageUID: string(p.UID), Namespace: p.Namespace, Target: p.Spec.Target,
			Pod: p.Spec.Trigger.Pod,
		})
		if err != nil {
			logf.FromContext(ctx).Error(err, "assemble the Context Bundle", "page", p.Name)
		} else {
			evidence = bundle.Summary()
		}
	}
	job := AgentJob(p, r.Runtime.jobConfig(ctx, r.Job))
	if err := controllerutil.SetControllerReference(p, job, r.Scheme); err != nil {
		return fmt.Errorf("own agent Job: %w", err)
	}
	if err := r.Client.Create(ctx, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create agent Job: %w", err)
		}
		var existing batchv1.Job
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(job), &existing); err != nil {
			return fmt.Errorf("get existing agent Job: %w", err)
		}
		if !metav1.IsControlledBy(&existing, p) {
			return fmt.Errorf("agent Job %s exists and is not owned by Page %s", existing.Name, p.Name)
		}
	}
	updated, err := r.Status.Update(ctx, client.ObjectKeyFromObject(p), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhasePending {
			return nil
		}
		latest.Status.Phase = v1alpha1.PhaseInvestigating
		latest.Status.AgentJobName = job.Name
		latest.Status.Investigation = &v1alpha1.InvestigationStatus{
			State: v1alpha1.InvestigationRunning,
			Job:   job.Name,
		}
		if evidence != nil {
			latest.Status.Evidence = evidence
		}
		return nil
	})
	if err == nil && updated.Status.Phase == v1alpha1.PhaseSuperseded {
		return r.deleteSupersededJob(ctx, updated)
	}
	return err
}

// resolveDuplicates repairs the Active Page race before allowing more
// investigation work. The standalone chart installs no admission webhook, so the
// single-writer server selects the canonical Page and supersedes the rest
// deterministically. It can run for any Active Page, so a late tie cannot leave
// an earlier loser's agent running without another status update.
func (r *PageReconciler) resolveDuplicates(ctx context.Context, current *v1alpha1.Page) (bool, error) {
	winner, duplicates, err := page.CanonicalActive(ctx, r.Status.Reader, current.Namespace, current.Spec.Fingerprint)
	if err != nil || winner == nil {
		return false, err
	}
	for i := range duplicates {
		if err := r.supersede(ctx, &duplicates[i], winner); err != nil {
			return false, err
		}
	}
	return winner.UID == current.UID, nil
}

// supersede ends a duplicate Page as Superseded. It fails closed when the
// duplicate carries approved work or the canonical choice moved under it.
func (r *PageReconciler) supersede(ctx context.Context, duplicate, winner *v1alpha1.Page) error {
	updated, err := r.Status.Update(ctx, client.ObjectKeyFromObject(duplicate), func(latest *v1alpha1.Page) error {
		if latest.UID != duplicate.UID || !latest.Status.Phase.IsActive() || page.HasApprovedWork(latest) {
			return nil
		}
		canonical, _, err := page.CanonicalActive(ctx, r.Status.Reader, latest.Namespace, latest.Spec.Fingerprint)
		if err != nil {
			return err
		}
		if canonical == nil || canonical.UID != winner.UID {
			return errors.New("the canonical Active Page changed; retry duplicate resolution")
		}
		latest.Status.Phase = v1alpha1.PhaseSuperseded
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAccepted,
			Status:             metav1.ConditionFalse,
			Reason:             v1alpha1.ReasonDuplicate,
			Message:            fmt.Sprintf("Page %q is canonical for this Fingerprint", winner.Name),
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	if err != nil {
		return err
	}
	if updated.Status.Phase == v1alpha1.PhaseSuperseded {
		return r.deleteSupersededJob(ctx, updated)
	}
	return nil
}

// deleteSupersededJob never deletes a name collision or a replaced Job:
// ownership and a UID precondition bind deletion to this superseded Page. An
// owned Job created after the supersession is cleaned on its Owns watch event.
func (r *PageReconciler) deleteSupersededJob(ctx context.Context, p *v1alpha1.Page) error {
	var job batchv1.Job
	key := client.ObjectKey{Namespace: p.Namespace, Name: AgentJobName(p.Name)}
	if err := r.Status.Reader.Get(ctx, key, &job); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(&job, p) {
		return nil
	}
	err := r.Client.Delete(ctx, &job, client.Preconditions{UID: &job.UID},
		client.PropagationPolicy(metav1.DeletePropagationBackground))
	return client.IgnoreNotFound(err)
}

// watchAgentJob ends the Page as Failed when its Job has ended or vanished
// without a report. It reads the Job directly, because a cached read could
// miss a Job that was created a moment ago. A report that arrives first has
// already moved the Page out of Investigating, and the guard in fail leaves
// it alone.
func (r *PageReconciler) watchAgentJob(ctx context.Context, p *v1alpha1.Page) error {
	var job batchv1.Job
	err := r.Status.Reader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Status.AgentJobName}, &job)
	var outcome jobOutcome
	switch {
	case apierrors.IsNotFound(err):
		outcome = jobOutcome{
			finished: true,
			reason:   v1alpha1.ReasonAgentJobMissing,
			message:  "the agent Job disappeared before the agent reported",
		}
	case err != nil:
		return fmt.Errorf("get agent Job: %w", err)
	default:
		pods, err := r.agentPods(ctx, &job)
		if err != nil {
			return err
		}
		outcome = outcomeOf(&job, pods)
	}
	if !outcome.finished {
		return nil
	}
	return fail(ctx, r.Status, client.ObjectKeyFromObject(p), outcome.reason, outcome.message)
}

// agentPods returns the pods the agent Job started. The Job controller sets the
// Job's completion condition only after its pod has terminated, so the exit code
// that explains a budget exhaustion is available here.
func (r *PageReconciler) agentPods(ctx context.Context, job *batchv1.Job) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.Status.Reader.List(ctx, &pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{PageLabel: job.Labels[PageLabel]}); err != nil {
		return nil, fmt.Errorf("list the pods of agent Job %s: %w", job.Name, err)
	}
	owned := make([]corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if metav1.IsControlledBy(&pods.Items[i], job) {
			owned = append(owned, pods.Items[i])
		}
	}
	return owned, nil
}

// fail moves an Investigating Page to Failed with a Failed condition,
// preserving its Findings. It does nothing to a Page in any other phase.
func fail(ctx context.Context, store *page.StatusStore, key client.ObjectKey, reason, message string) error {
	_, err := store.Update(ctx, key, func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhaseInvestigating {
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
