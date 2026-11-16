// Package controller reconciles Triages: it starts the agent Job for a new
// Triage and ends the Triage as Failed when the Job ends without a report.
package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/triage"
)

// TriageReconciler drives a Triage through Pending and Investigating, and
// ends it as Failed when its agent Job ends without a report.
type TriageReconciler struct {
	// Client reads and writes Jobs and reads Triages.
	Client client.Client
	// Scheme is the scheme that knows the Triage and Job types.
	Scheme *runtime.Scheme
	// Status is the only writer of Triage status.
	Status *triage.StatusStore
	// Job configures the agent Job.
	Job AgentJobConfig
}

// SetupWithManager registers the reconciler for Triages and the Jobs it owns.
func (r *TriageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Triage{}).
		Owns(&batchv1.Job{}).
		Named("triage").
		// The test harness runs several managers in one process, which
		// controller-runtime's process-wide name check would refuse.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		Complete(r)
}

// Reconcile advances one Triage.
func (r *TriageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t v1alpha1.Triage
	if err := r.Client.Get(ctx, req.NamespacedName, &t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !t.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	switch t.Status.Phase {
	case "":
		if err := r.initialize(ctx, &t); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	case v1alpha1.PhasePending:
		return ctrl.Result{}, r.startInvestigation(ctx, &t)
	case v1alpha1.PhaseInvestigating:
		return ctrl.Result{}, r.watchAgentJob(ctx, &t)
	default:
		return ctrl.Result{}, nil
	}
}

// initialize puts a new Triage in the Pending phase.
func (r *TriageReconciler) initialize(ctx context.Context, t *v1alpha1.Triage) error {
	_, err := r.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Triage) error {
		if latest.Status.Phase != "" {
			return nil
		}
		latest.Status.Phase = v1alpha1.PhasePending
		// A repeat Trigger may have joined the Triage before it was initialized,
		// so the creating Trigger is added to the count rather than set.
		latest.Status.TriggerCount++
		return nil
	})
	return err
}

// startInvestigation creates the agent Job and moves the Triage to
// Investigating. Creating the Job first and tolerating an existing one makes
// the step safe to repeat after a crash.
func (r *TriageReconciler) startInvestigation(ctx context.Context, t *v1alpha1.Triage) error {
	job := buildAgentJob(t, r.Job)
	if err := controllerutil.SetControllerReference(t, job, r.Scheme); err != nil {
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
		if !metav1.IsControlledBy(&existing, t) {
			return fmt.Errorf("agent Job %s exists and is not owned by Triage %s", existing.Name, t.Name)
		}
	}
	_, err := r.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Triage) error {
		if latest.Status.Phase != v1alpha1.PhasePending {
			return nil
		}
		latest.Status.Phase = v1alpha1.PhaseInvestigating
		latest.Status.JobName = job.Name
		return nil
	})
	return err
}

// watchAgentJob ends the Triage as Failed when its Job has ended or vanished
// without a report. It reads the Job directly, because a cached read could
// miss a Job that was created a moment ago. A report that arrives first has
// already moved the Triage out of Investigating, and the guard in fail leaves
// it alone.
func (r *TriageReconciler) watchAgentJob(ctx context.Context, t *v1alpha1.Triage) error {
	var job batchv1.Job
	err := r.Status.Reader.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Status.JobName}, &job)
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
		outcome = outcomeOf(&job)
	}
	if !outcome.finished {
		return nil
	}
	return fail(ctx, r.Status, client.ObjectKeyFromObject(t), outcome.reason, outcome.message)
}

// fail moves an Investigating Triage to Failed with a Failed condition,
// preserving its Findings. It does nothing to a Triage in any other phase.
func fail(ctx context.Context, store *triage.StatusStore, key client.ObjectKey, reason, message string) error {
	_, err := store.Update(ctx, key, func(latest *v1alpha1.Triage) error {
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
