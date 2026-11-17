package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	"github.com/a-hagi613/pagerless/internal/triage"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// DefaultVerificationPollInterval is how often an Executing Triage is checked for a sync, and a Verifying Triage
// is observed for restarts.
const DefaultVerificationPollInterval = 10 * time.Second

// VerificationReconciler decides whether a Workload recovered after its merged pull request (ADR-0006). It moves
// an Executing Triage whose pull request has merged to Verifying once Argo CD reports the application Synced and
// Healthy at the merge commit, observes the Workload for the window, and ends the Triage as Recovered or
// NotRecovered. A NotRecovered Triage starts a Follow-up Triage. After a terminal outcome the reconciler starts
// the Narrative Job, which never changes the outcome. Every decision is made by deterministic code, and the
// reconciler writes nothing but status and the jobs and Triage it starts (ADR-0001).
type VerificationReconciler struct {
	// Client creates Follow-up Triages and the Narrative Job.
	Client client.Client
	// Scheme sets the Narrative Job's owner reference.
	Scheme *runtime.Scheme
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
	// NarrativeJob configures the Job that writes the Narrative of a concluded Triage.
	NarrativeJob AgentJobConfig
}

// SetupWithManager registers the reconciler for Triages and the Jobs it watches.
func (r *VerificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Triage{}).
		// The Narrative Job's completion or failure is what drives the last status write of a Triage.
		Owns(&batchv1.Job{}).
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

// Reconcile advances one Triage that is Executing with a merged pull request, Verifying, or has reached a
// terminal Verification outcome and still needs its Narrative or Follow-up Triage.
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
	case v1alpha1.PhaseExecuting, v1alpha1.PhaseVerifying, v1alpha1.PhaseRecovered, v1alpha1.PhaseNotRecovered:
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
	case v1alpha1.PhaseRecovered, v1alpha1.PhaseNotRecovered:
		// The Follow-up Triage comes first: the Narrative is secondary and must never delay it.
		if tr.Status.Phase == v1alpha1.PhaseNotRecovered {
			if err := r.ensureFollowUp(ctx, &tr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, r.ensureNarrative(ctx, &tr)
	default:
		return ctrl.Result{}, nil
	}
}

// awaitSync starts Verification once the pull request has merged and Argo CD has synced the merge commit,
// or, for an Action that executed live, once its result is recorded and Argo CD reports the application Synced.
func (r *VerificationReconciler) awaitSync(ctx context.Context, tr *v1alpha1.Triage) (ctrl.Result, error) {
	execution := tr.Status.Execution
	if execution == nil {
		return ctrl.Result{}, nil
	}
	if !live(tr) && execution.MergeCommit == "" {
		// The execution reconciler records the merge commit, which triggers another reconcile.
		return ctrl.Result{}, nil
	}
	synced, err := r.synced(ctx, tr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check that Argo CD synced the change: %w", err)
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
		logf.FromContext(ctx).Info("verification started", "triage", key.Name,
			"mergeCommit", execution.MergeCommit, "live", live(tr))
	}
	return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
}

// synced checks the Argo CD status that applies to the Action: the merge commit
// for a pull request Action, and no revision for an Action that changed no commit.
func (r *VerificationReconciler) synced(
	ctx context.Context, tr *v1alpha1.Triage,
) (v1alpha1.VerificationCheck, error) {
	if live(tr) {
		return r.Verifier.LiveSynced(ctx)
	}
	return r.Verifier.Synced(ctx, tr.Status.Execution.MergeCommit)
}

// live reports whether the Triage's Action executes against the cluster rather
// than as a Sandbox pull request.
func live(tr *v1alpha1.Triage) bool {
	return tr.Status.Proposal != nil && tr.Status.Proposal.Action.ExecutesLive()
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

	synced, err := r.synced(ctx, tr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check that Argo CD still has the change synced and healthy: %w", err)
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
	// The Follow-up Triage comes first: the Narrative is secondary and must never delay it.
	if concluded.Status.Phase == v1alpha1.PhaseNotRecovered {
		if err := r.ensureFollowUp(ctx, concluded); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, r.ensureNarrative(ctx, concluded)
}

// ensureNarrative starts the Narrative Job of a Triage whose Verification has
// ended, unless it already records a Narrative. The Job is named deterministically,
// so a crash between recording the name and creating the Job repairs on the next
// reconcile. A Job that failed is left alone: it writes no status, so the
// Verification outcome and every other field stay as they were.
func (r *VerificationReconciler) ensureNarrative(ctx context.Context, tr *v1alpha1.Triage) error {
	if tr.Status.Narrative != "" {
		return nil
	}
	job := buildNarrativeJob(tr, r.NarrativeJob)
	if err := controllerutil.SetControllerReference(tr, job, r.Scheme); err != nil {
		return fmt.Errorf("own the Narrative Job: %w", err)
	}
	// The record of the Job's name authorizes its pod at the agent API, so it is
	// written before the Job can start.
	recorded, err := r.Status.Update(ctx, client.ObjectKeyFromObject(tr), func(latest *v1alpha1.Triage) error {
		if latest.Status.NarrativeJobName == "" {
			latest.Status.NarrativeJobName = job.Name
		}
		return nil
	})
	if err != nil {
		return err
	}
	if recorded.Status.Narrative != "" {
		return nil
	}
	if err := r.Client.Create(ctx, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create the Narrative Job: %w", err)
		}
		var existing batchv1.Job
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(job), &existing); err != nil {
			return fmt.Errorf("get the existing Narrative Job: %w", err)
		}
		if !metav1.IsControlledBy(&existing, tr) {
			return fmt.Errorf("narrative Job %s exists and is not owned by Triage %s", existing.Name, tr.Name)
		}
	}
	return nil
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
