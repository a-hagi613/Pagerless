package controller

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const (
	// DefaultRetention is how long a terminal Page is kept before it is deleted.
	DefaultRetention = 7 * 24 * time.Hour
	// DefaultRetentionPollInterval is how often a terminal Page is checked before its retention expires.
	DefaultRetentionPollInterval = time.Minute
)

// RetentionReconciler deletes a Page seven days after it reaches a terminal Phase. The history in SQLite is
// written separately and is never deleted with the resource, so deleting a Page keeps its permanent record.
type RetentionReconciler struct {
	// Client deletes the Page.
	Client client.Client
	// Reader reads the Page directly from the API server, never from a cache that may be stale.
	Reader client.Reader
	// Clock decides whether the retention period has elapsed. It is injectable so the boundary is testable.
	Clock clock.PassiveClock
	// Retention is how long a terminal Page is kept. It defaults to DefaultRetention.
	Retention time.Duration
	// PollInterval bounds how long the reconciler waits before it checks a Page again.
	// It defaults to DefaultRetentionPollInterval.
	PollInterval time.Duration
}

// SetupWithManager registers the reconciler for Pages.
func (r *RetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Page{}).
		Named("retention").
		// The test harness runs several managers in one process, which
		// controller-runtime's process-wide name check would refuse.
		WithOptions(controller.Options{SkipNameValidation: new(true)}).
		Complete(r)
}

// Reconcile deletes a terminal Page once its retention period has elapsed.
func (r *RetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tr v1alpha1.Page
	if err := r.Reader.Get(ctx, req.NamespacedName, &tr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !tr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if !tr.Status.Phase.IsTerminal() || tr.Status.TerminalAt == nil {
		return ctrl.Result{}, nil
	}
	now := r.Clock.Now()
	expires := tr.Status.TerminalAt.Add(r.retention())
	if now.Before(expires) {
		return ctrl.Result{RequeueAfter: min(expires.Sub(now), r.pollInterval())}, nil
	}
	// The UID precondition keeps a deleted-and-recreated Page with the same name from being deleted by mistake.
	err := r.Client.Delete(
		ctx, &tr,
		client.Preconditions{UID: &tr.UID},
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	)
	return ctrl.Result{}, client.IgnoreNotFound(err)
}

// retention returns the configured retention period.
func (r *RetentionReconciler) retention() time.Duration {
	if r.Retention == 0 {
		return DefaultRetention
	}
	return r.Retention
}

// pollInterval returns the configured poll interval.
func (r *RetentionReconciler) pollInterval() time.Duration {
	if r.PollInterval == 0 {
		return DefaultRetentionPollInterval
	}
	return r.PollInterval
}
