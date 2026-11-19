package watcher

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// StatusReconciler re-derives one kind's live signals and feeds them to the
// pipeline. It runs for every kind a Page can target, because Events alone are
// lossy: they expire after an hour and say nothing about whether the failure is
// still true.
type StatusReconciler struct {
	// Client reads the observed object and the Jobs a CronJob owns.
	Client client.Client
	// Resolver canonicalizes the observed object to its Subject.
	Resolver *Resolver
	// Pipeline decides what the signals open or suppress.
	Pipeline *Pipeline
	// Kind is the kind this reconciler watches.
	Kind v1alpha1.TargetKind
	// Observe tunes the thresholds.
	Observe ObserveConfig
	// RequeueAfter is how long a healthy object waits before it is re-derived. It
	// bounds how long a cleared failure can outlive its status.
	RequeueAfter time.Duration
}

// setup registers the reconciler for its kind.
func (r *StatusReconciler) setup(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		Named("status-" + string(r.Kind)).
		WithOptions(controller.Options{SkipNameValidation: new(true)})
	object, err := newObject(string(r.Kind))
	if err != nil {
		return err
	}
	return builder.For(object).Complete(r)
}

// Reconcile re-derives the signals of one object.
func (r *StatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	object, err := newObject(string(r.Kind))
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Client.Get(ctx, req.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	reference, signals, availability, err := r.signals(ctx, object)
	if err != nil {
		if ignorable(err) {
			return r.requeue(), nil
		}
		return ctrl.Result{}, err
	}
	subject, err := r.Resolver.Resolve(ctx, reference)
	if err != nil {
		if ignorable(err) {
			return r.requeue(), nil
		}
		return ctrl.Result{}, err
	}
	if err := r.Pipeline.HandleStatus(ctx, subject, availability, signals); err != nil {
		logf.FromContext(ctx).Error(err, "observe status", "kind", r.Kind, "object", req.NamespacedName)
	}
	return r.requeue(), nil
}

// requeue asks for another pass after the configured interval.
func (r *StatusReconciler) requeue() ctrl.Result {
	if r.RequeueAfter <= 0 {
		return ctrl.Result{}
	}
	return ctrl.Result{RequeueAfter: r.RequeueAfter}
}

// signals derives the live signals, the availability, and the reference of one
// object. It is a type switch because the durable fields differ per kind.
func (r *StatusReconciler) signals(ctx context.Context, object client.Object) (Reference, []Signal,
	Availability, error,
) {
	now := r.Pipeline.now()
	switch typed := object.(type) {
	case *corev1.Pod:
		return podReference(typed), PodSignals(typed, now, r.Observe), PodAvailability(typed), nil
	case *appsv1.Deployment:
		return referenceOfObject(typed, v1alpha1.TargetKindDeployment), DeploymentSignals(typed, now),
			DeploymentAvailability(typed), nil
	case *appsv1.StatefulSet:
		return referenceOfObject(typed, v1alpha1.TargetKindStatefulSet), StatefulSetSignals(typed, now),
			StatefulSetAvailability(typed), nil
	case *appsv1.DaemonSet:
		return referenceOfObject(typed, v1alpha1.TargetKindDaemonSet), DaemonSetSignals(typed, now),
			DaemonSetAvailability(typed), nil
	case *batchv1.Job:
		return referenceOfObject(typed, v1alpha1.TargetKindJob), JobSignals(typed, now),
			JobAvailability(typed), nil
	case *batchv1.CronJob:
		jobs, err := r.ownedJobs(ctx, typed)
		if err != nil {
			return Reference{}, nil, Availability{}, err
		}
		return referenceOfObject(typed, v1alpha1.TargetKindCronJob),
			CronJobSignals(typed, jobs, now, r.Observe), Availability{Desired: 1, Ready: 1}, nil
	case *corev1.Node:
		return Reference{
			APIVersion: apiVersionCore, Kind: string(v1alpha1.TargetKindNode), Name: typed.Name,
			UID: string(typed.UID),
		}, NodeSignals(typed, now), NodeAvailability(typed), nil
	case *corev1.PersistentVolumeClaim:
		return referenceOfObject(typed, v1alpha1.TargetKindPersistentVolumeClaim),
			PersistentVolumeClaimSignals(typed, now), PersistentVolumeClaimAvailability(typed), nil
	default:
		return Reference{}, nil, Availability{}, fmt.Errorf("no observer for %T", object)
	}
}

// ownedJobs returns the Jobs a CronJob controls.
func (r *StatusReconciler) ownedJobs(ctx context.Context, cronJob *batchv1.CronJob) ([]batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.Client.List(ctx, &jobs, client.InNamespace(cronJob.Namespace)); err != nil {
		return nil, fmt.Errorf("list Jobs in %s: %w", cronJob.Namespace, err)
	}
	var owned []batchv1.Job
	for i := range jobs.Items {
		owner := metav1.GetControllerOf(&jobs.Items[i])
		if owner == nil || owner.UID != cronJob.UID {
			continue
		}
		owned = append(owned, jobs.Items[i])
	}
	return owned, nil
}

// referenceOfObject projects a typed object onto a Reference.
func referenceOfObject(object metav1.Object, kind v1alpha1.TargetKind) Reference {
	return Reference{
		APIVersion: APIVersionFor(string(kind)),
		Kind:       string(kind),
		Namespace:  object.GetNamespace(),
		Name:       object.GetName(),
		UID:        string(object.GetUID()),
	}
}

// observableKinds are the kinds whose durable status the watcher reconciles.
var observableKinds = []v1alpha1.TargetKind{
	v1alpha1.TargetKindPod,
	v1alpha1.TargetKindDeployment,
	v1alpha1.TargetKindStatefulSet,
	v1alpha1.TargetKindDaemonSet,
	v1alpha1.TargetKindJob,
	v1alpha1.TargetKindCronJob,
	v1alpha1.TargetKindNode,
	v1alpha1.TargetKindPersistentVolumeClaim,
}
