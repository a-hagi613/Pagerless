package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/triage"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// DefaultVerificationPollInterval is how often an Executing Triage is checked for a sync, and a Verifying Triage
// is observed for restarts.
const DefaultVerificationPollInterval = 10 * time.Second

// VerificationReconciler decides whether a Workload recovered after its merged pull request (ADR-0006). It moves
// an Executing Triage whose pull request has merged to Verifying once Argo CD reports the application Synced and
// Healthy at the merge commit, observes the Workload for the window, and ends the Triage as Recovered or
// NotRecovered. A NotRecovered Triage starts a Follow-up Triage. Every decision is made by deterministic code, and
// the reconciler writes nothing but status and the Follow-up Triage (ADR-0001).
type VerificationReconciler struct {
	// Client creates Follow-up Triages.
	Client client.Client
	// Status is the only writer of Triage status, and reads Triages directly from the API server.
	Status *triage.StatusStore
	// Verifier runs the checks.
	Verifier *verification.Verifier
	// Clock timestamps the observation window.
	Clock clock.PassiveClock
	// Window is how long the Workload is observed. It defaults to verification.DefaultWindow.
	Window time.Duration
	// PollInterval is how often a Triage is checked. It defaults to DefaultVerificationPollInterval.
	PollInterval time.Duration
}

// SetupWithManager registers the reconciler for Triages.
func (r *VerificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Triage{}).
		Named("verification").
		// The test harness runs several managers in one process, which
		// controller-runtime's process-wide name check would refuse.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		Complete(r)
}

func (r *VerificationReconciler) window() time.Duration {
	if r.Window == 0 {
		return verification.DefaultWindow
	}
	return r.Window
}

func (r *VerificationReconciler) pollInterval() time.Duration {
	if r.PollInterval == 0 {
		return DefaultVerificationPollInterval
	}
	return r.PollInterval
}

// Reconcile advances one Triage that is Executing with a merged pull request, Verifying, or NotRecovered.
func (r *VerificationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcile(ctx, req)
	if err != nil && ctx.Err() != nil {
		// The server is stopping, so the failure is not worth reporting.
		return ctrl.Result{}, nil
	}
	return result, err
}

func (r *VerificationReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cached v1alpha1.Triage
	if err := r.Client.Get(ctx, req.NamespacedName, &cached); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	switch cached.Status.Phase {
	case v1alpha1.PhaseExecuting, v1alpha1.PhaseVerifying, v1alpha1.PhaseNotRecovered:
	default:
		return ctrl.Result{}, nil
	}
	// Decide on the Triage as the API server holds it, never on a cached copy that may predate a transition.
	var tr v1alpha1.Triage
	if err := r.Status.Reader.Get(ctx, req.NamespacedName, &tr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	switch tr.Status.Phase {
	case v1alpha1.PhaseExecuting:
		return r.awaitSync(ctx, &tr)
	case v1alpha1.PhaseVerifying:
		return r.observe(ctx, &tr)
	case v1alpha1.PhaseNotRecovered:
		return ctrl.Result{}, r.ensureFollowUp(ctx, &tr)
	default:
		return ctrl.Result{}, nil
	}
}

// awaitSync starts Verification once the pull request has merged and Argo CD has synced the merge commit.
func (r *VerificationReconciler) awaitSync(ctx context.Context, tr *v1alpha1.Triage) (ctrl.Result, error) {
	if tr.Status.Execution == nil || tr.Status.Execution.MergeCommit == "" {
		// The execution reconciler records the merge commit, which triggers another reconcile.
		return ctrl.Result{}, nil
	}
	synced, err := r.Verifier.Synced(ctx, tr.Status.Execution.MergeCommit)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check that Argo CD synced the merge commit: %w", err)
	}
	if !synced.Passed {
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}
	key := client.ObjectKeyFromObject(tr)
	started, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
		if latest.Status.Phase != v1alpha1.PhaseExecuting {
			return nil
		}
		now := metav1.NewTime(r.Clock.Now())
		latest.Status.Phase = v1alpha1.PhaseVerifying
		latest.Status.VerificationStartedAt = &now
		setCheck(&latest.Status, synced)
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if started.Status.Phase == v1alpha1.PhaseVerifying {
		logf.FromContext(ctx).Info("verification started", "triage", key.Name, "mergeCommit", tr.Status.Execution.MergeCommit)
	}
	return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
}

// observe watches the Workload for restarts throughout the observation window and decides only after it ends.
func (r *VerificationReconciler) observe(ctx context.Context, tr *v1alpha1.Triage) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(tr)
	if tr.Status.VerificationStartedAt == nil {
		// Not reachable through this reconciler, which sets the time with the phase. Start the window now rather
		// than decide without one.
		_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
			if latest.Status.Phase == v1alpha1.PhaseVerifying && latest.Status.VerificationStartedAt == nil {
				now := metav1.NewTime(r.Clock.Now())
				latest.Status.VerificationStartedAt = &now
			}
			return nil
		})
		return ctrl.Result{RequeueAfter: r.pollInterval()}, err
	}
	started := tr.Status.VerificationStartedAt.Time
	restarts, err := r.Verifier.Restarts(ctx, tr.Spec.Workload, started)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the Workload for restarts: %w", err)
	}
	if !restarts.Passed {
		// Preserve any observed failure before a pod can be deleted and its last termination state lost.
		if _, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
			if latest.Status.Phase == v1alpha1.PhaseVerifying {
				setCheck(&latest.Status, restarts)
			}
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	windowOver := !r.Clock.Now().Before(started.Add(r.window()))
	if !windowOver {
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}

	synced, err := r.Verifier.Synced(ctx, tr.Status.Execution.MergeCommit)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check that Argo CD still has the merge commit synced and healthy: %w", err)
	}
	rollout, err := r.Verifier.Rollout(ctx, tr.Spec.Workload)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the rollout: %w", err)
	}
	alert, err := r.Verifier.Alert(ctx, tr.Spec.Trigger)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the triggering alert: %w", err)
	}
	concluded, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
		if latest.Status.Phase != v1alpha1.PhaseVerifying {
			return nil
		}
		recovered := true
		setCheck(&latest.Status, synced)
		setCheck(&latest.Status, rollout)
		setCheck(&latest.Status, alert)
		restartFailureRecorded := false
		for _, c := range latest.Status.VerificationChecks {
			if c.Name == v1alpha1.CheckNoNewRestarts && !c.Passed {
				restartFailureRecorded = true
				break
			}
		}
		if !restartFailureRecorded {
			setCheck(&latest.Status, restarts)
		}
		for _, c := range latest.Status.VerificationChecks {
			recovered = recovered && c.Passed
		}
		latest.Status.Phase = v1alpha1.PhaseNotRecovered
		if recovered {
			latest.Status.Phase = v1alpha1.PhaseRecovered
		}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("verification ended", "triage", key.Name, "phase", concluded.Status.Phase)
	if concluded.Status.Phase == v1alpha1.PhaseNotRecovered {
		return ctrl.Result{}, r.ensureFollowUp(ctx, concluded)
	}
	return ctrl.Result{}, nil
}

// setCheck records the result of a check, replacing an earlier result of the same check.
func setCheck(status *v1alpha1.TriageStatus, check v1alpha1.VerificationCheck) {
	for i := range status.VerificationChecks {
		if status.VerificationChecks[i].Name == check.Name {
			status.VerificationChecks[i] = check
			return
		}
	}
	status.VerificationChecks = append(status.VerificationChecks, check)
}

// ensureFollowUp starts the Follow-up Triage of a NotRecovered Triage and records it, or does nothing if the
// Triage already records one. A crash between the two writes is repaired on the next reconcile, which finds the
// Follow-up Triage by the reference it holds.
func (r *VerificationReconciler) ensureFollowUp(ctx context.Context, tr *v1alpha1.Triage) error {
	if tr.Status.FollowUpTriage != "" ||
		meta.IsStatusConditionTrue(tr.Status.Conditions, v1alpha1.ConditionFollowUpSkipped) {
		return nil
	}
	key := client.ObjectKeyFromObject(tr)
	name, err := r.followUpName(ctx, tr)
	if err != nil {
		return err
	}
	if name == "" {
		followUp := &v1alpha1.Triage{
			GenerateName: "triage-", Namespace: tr.Namespace,
			Spec: v1alpha1.TriageSpec{
				Workload: tr.Spec.Workload,
				Trigger: v1alpha1.Trigger{
					Source: v1alpha1.TriggerSourceFollowUp,
					Pod:    tr.Spec.Trigger.Pod,
					// The alert that started the original Triage must also resolve after the follow-up's remediation.
					AlertFingerprint: tr.Spec.Trigger.AlertFingerprint,
				},
				FollowsTriage: tr.Name,
			},
		}
		if err := r.Client.Create(ctx, followUp); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// The duplicate refusal: a Trigger started another Triage for the Workload first, and Triggers attach to it.
				return r.skipFollowUp(ctx, key, err.Error())
			}
			return fmt.Errorf("create the Follow-up Triage: %w", err)
		}
		name = followUp.Name
		logf.FromContext(ctx).Info("started a Follow-up Triage", "triage", key.Name, "followUp", name)
	}
	_, err = r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
		latest.Status.FollowUpTriage = name
		return nil
	})
	return err
}

// followUpName returns the name of the Triage that already follows tr, or empty if none does.
func (r *VerificationReconciler) followUpName(ctx context.Context, tr *v1alpha1.Triage) (string, error) {
	var all v1alpha1.TriageList
	if err := r.Status.Reader.List(ctx, &all, client.InNamespace(tr.Namespace)); err != nil {
		return "", fmt.Errorf("list Triages: %w", err)
	}
	for i := range all.Items {
		if all.Items[i].Spec.FollowsTriage == tr.Name {
			return all.Items[i].Name, nil
		}
	}
	return "", nil
}

// skipFollowUp records that the Follow-up Triage could not be started.
func (r *VerificationReconciler) skipFollowUp(ctx context.Context, key client.ObjectKey, message string) error {
	if len(message) > maxConditionMessage {
		message = message[:maxConditionMessage] + "..."
	}
	_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Triage) error {
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionFollowUpSkipped,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ReasonActiveTriageExists,
			Message:            message,
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	return err
}
