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
	"github.com/a-hagi613/pagerless/internal/page"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// DefaultVerificationPollInterval is how often a PullRequestOpen Page is checked for a merge, and a Verifying
// Page is observed for restarts.
const DefaultVerificationPollInterval = 10 * time.Second

// VerificationReconciler decides whether a Target recovered after its merged
// pull request (ADR-0006). It observes the Target for the window and ends the
// Page as Recovered or NotRecovered. After a terminal outcome the reconciler
// starts the Narrative Job, which never changes the outcome. Every decision is
// made by deterministic code, and the reconciler writes nothing but status and
// the Job it starts (ADR-0001).
type VerificationReconciler struct {
	// Client creates the Narrative Job.
	Client client.Client
	// Scheme sets the Narrative Job's owner reference.
	Scheme *runtime.Scheme
	// Status is the only writer of Page status, and reads Pages directly from the API server.
	Status *page.StatusStore
	// Verifier runs the checks.
	Verifier *verification.Verifier
	// Clock timestamps the observation window.
	Clock clock.PassiveClock
	// Window is how long the Target is observed. It defaults to verification.DefaultWindow.
	Window time.Duration
	// PollInterval is how often a Page is checked. It defaults to DefaultVerificationPollInterval.
	PollInterval time.Duration
	// NarrativeJob configures the Job that writes the Narrative of a concluded Page.
	NarrativeJob AgentJobConfig
}

// SetupWithManager registers the reconciler for Pages and the Jobs it watches.
func (r *VerificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Page{}).
		// The Narrative Job's completion or failure is what drives the last status write of a Page.
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

// Reconcile advances one Page that is PullRequestOpen, Verifying, or has
// reached a terminal Verification outcome and still needs its Narrative.
func (r *VerificationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcile(ctx, req)
	if err != nil && ctx.Err() != nil {
		// The server is stopping, so the failure is not worth reporting.
		return ctrl.Result{}, nil
	}
	return result, err
}

func (r *VerificationReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cached v1alpha1.Page
	if err := r.Client.Get(ctx, req.NamespacedName, &cached); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	switch cached.Status.Phase {
	case v1alpha1.PhasePullRequestOpen, v1alpha1.PhaseVerifying, v1alpha1.PhaseRecovered, v1alpha1.PhaseNotRecovered:
	default:
		return ctrl.Result{}, nil
	}
	// Decide on the Page as the API server holds it, never on a cached copy that may predate a transition.
	var p v1alpha1.Page
	if err := r.Status.Reader.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	switch p.Status.Phase {
	case v1alpha1.PhasePullRequestOpen:
		return r.awaitSync(ctx, &p)
	case v1alpha1.PhaseVerifying:
		return r.observe(ctx, &p)
	case v1alpha1.PhaseRecovered, v1alpha1.PhaseNotRecovered:
		return ctrl.Result{}, r.ensureNarrative(ctx, &p)
	default:
		return ctrl.Result{}, nil
	}
}

// awaitSync starts Verification once the pull request has merged and, when a
// GitOps controller is configured, has synced the merge commit.
func (r *VerificationReconciler) awaitSync(ctx context.Context, p *v1alpha1.Page) (ctrl.Result, error) {
	pullRequest := p.Status.PullRequest
	if pullRequest == nil || pullRequest.MergeCommit == "" {
		// The pull request reconciler records the merge commit, which triggers another reconcile.
		return ctrl.Result{}, nil
	}
	synced, err := r.Verifier.Synced(ctx, pullRequest.MergeCommit)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check that the GitOps controller synced the change: %w", err)
	}
	if !synced.Passed {
		return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
	}
	key := client.ObjectKeyFromObject(p)
	started, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhasePullRequestOpen {
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
		logf.FromContext(ctx).Info("verification started", "page", key.Name, "mergeCommit", pullRequest.MergeCommit)
	}
	return ctrl.Result{RequeueAfter: r.pollInterval()}, nil
}

// observe watches the Target for restarts throughout the observation window and decides only after it ends.
func (r *VerificationReconciler) observe(ctx context.Context, p *v1alpha1.Page) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(p)
	if p.Status.VerificationStartedAt == nil {
		// Not reachable through this reconciler, which sets the time with the phase. Start the window now rather
		// than decide without one.
		_, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Page) error {
			if latest.Status.Phase == v1alpha1.PhaseVerifying && latest.Status.VerificationStartedAt == nil {
				now := metav1.NewTime(r.Clock.Now())
				latest.Status.VerificationStartedAt = &now
			}
			return nil
		})
		return ctrl.Result{RequeueAfter: r.pollInterval()}, err
	}
	started := p.Status.VerificationStartedAt.Time
	restarts, err := r.Verifier.Restarts(ctx, p.Spec.Target, started)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the Target for restarts: %w", err)
	}
	if !restarts.Passed {
		// Preserve any observed failure before a pod can be deleted and its last termination state lost.
		if _, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Page) error {
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

	synced, err := r.Verifier.Synced(ctx, p.Status.PullRequest.MergeCommit)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check that the GitOps controller still has the change synced: %w", err)
	}
	rollout, err := r.Verifier.Rollout(ctx, p.Spec.Target)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the rollout: %w", err)
	}
	alert, err := r.Verifier.Alert(ctx, p.Spec.Trigger)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check the triggering alert: %w", err)
	}
	concluded, err := r.Status.Update(ctx, key, func(latest *v1alpha1.Page) error {
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
		reason := v1alpha1.ReasonNotRecovered
		if recovered {
			latest.Status.Phase = v1alpha1.PhaseRecovered
			reason = v1alpha1.ReasonRecovered
		}
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionVerified,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            fmt.Sprintf("Verification ended the Page as %s", latest.Status.Phase),
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	logf.FromContext(ctx).Info("verification ended", "page", key.Name, "phase", concluded.Status.Phase)
	return ctrl.Result{}, r.ensureNarrative(ctx, concluded)
}

// ensureNarrative starts the Narrative Job of a Page whose Verification has
// ended, unless it already records a Narrative. The Job is named deterministically,
// so a crash between recording the name and creating the Job repairs on the next
// reconcile. A Job that failed is left alone: it writes no status, so the
// Verification outcome and every other field stay as they were.
func (r *VerificationReconciler) ensureNarrative(ctx context.Context, p *v1alpha1.Page) error {
	if p.Status.Narrative != "" {
		return nil
	}
	job := buildNarrativeJob(p, r.NarrativeJob)
	if err := controllerutil.SetControllerReference(p, job, r.Scheme); err != nil {
		return fmt.Errorf("own the Narrative Job: %w", err)
	}
	// The record of the Job's name authorizes its pod at the agent API, so it is
	// written before the Job can start.
	recorded, err := r.Status.Update(ctx, client.ObjectKeyFromObject(p), func(latest *v1alpha1.Page) error {
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
		if !metav1.IsControlledBy(&existing, p) {
			return fmt.Errorf("narrative Job %s exists and is not owned by Page %s", existing.Name, p.Name)
		}
	}
	return nil
}

// setCheck records the result of a check, replacing an earlier result of the same check.
func setCheck(status *v1alpha1.PageStatus, check v1alpha1.VerificationCheck) {
	for i := range status.VerificationChecks {
		if status.VerificationChecks[i].Name == check.Name {
			status.VerificationChecks[i] = check
			return
		}
	}
	status.VerificationChecks = append(status.VerificationChecks, check)
}
