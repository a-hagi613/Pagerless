package watcher

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// The API versions of the kinds a Page can target. They are constants because
// every Fingerprint hashes them, so a typo would be an identity change.
const (
	apiVersionCore  = "v1"
	apiVersionApps  = "apps/v1"
	apiVersionBatch = "batch/v1"
)

// apiVersions maps a kind to the API version a Page records for it.
var apiVersions = map[string]string{
	string(v1alpha1.TargetKindPod):                   apiVersionCore,
	string(v1alpha1.TargetKindDeployment):            apiVersionApps,
	string(v1alpha1.TargetKindStatefulSet):           apiVersionApps,
	string(v1alpha1.TargetKindDaemonSet):             apiVersionApps,
	string(v1alpha1.TargetKindJob):                   apiVersionBatch,
	string(v1alpha1.TargetKindCronJob):               apiVersionBatch,
	string(v1alpha1.TargetKindPersistentVolumeClaim): apiVersionCore,
	string(v1alpha1.TargetKindNode):                  apiVersionCore,
}

// APIVersionFor returns the API version a kind is recorded under.
func APIVersionFor(kind string) string {
	if version, ok := apiVersions[kind]; ok {
		return version
	}
	return ""
}

// ClusterLookup reads the objects canonicalization needs from the informer cache.
type ClusterLookup struct {
	// Reader is the manager's cached reader.
	Reader client.Reader
}

// Get fetches one object and projects it onto Object.
func (l *ClusterLookup) Get(ctx context.Context, reference Reference) (*Object, error) {
	object, err := newObject(reference.Kind)
	if err != nil {
		return nil, err
	}
	key := client.ObjectKey{Namespace: reference.Namespace, Name: reference.Name}
	if err := l.Reader.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrObjectNotFound
		}
		return nil, fmt.Errorf("get %s: %w", reference, err)
	}
	projected := &Object{
		Reference: Reference{
			APIVersion: APIVersionFor(reference.Kind),
			Kind:       reference.Kind,
			Namespace:  object.GetNamespace(),
			Name:       object.GetName(),
			UID:        string(object.GetUID()),
		},
		Annotations: object.GetAnnotations(),
		Labels:      object.GetLabels(),
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}
		projected.ControllerRefs++
		if projected.ControllerRef == nil {
			projected.ControllerRef = &Reference{
				APIVersion: owner.APIVersion,
				Kind:       owner.Kind,
				Namespace:  object.GetNamespace(),
				Name:       owner.Name,
				UID:        string(owner.UID),
			}
		}
	}
	return projected, nil
}

// Availability reports the replica health of a resolved Subject, so an Event
// Signal is judged by the same availability modifier a status Signal is.
func (l *ClusterLookup) Availability(ctx context.Context, subject Subject) (Availability, error) {
	switch v1alpha1.TargetKind(subject.Kind) {
	case v1alpha1.TargetKindPod:
		var pod corev1.Pod
		if err := l.get(ctx, subject, &pod); err != nil {
			return Availability{}, err
		}
		return PodAvailability(&pod), nil
	case v1alpha1.TargetKindDeployment:
		var deployment appsv1.Deployment
		if err := l.get(ctx, subject, &deployment); err != nil {
			return Availability{}, err
		}
		return DeploymentAvailability(&deployment), nil
	case v1alpha1.TargetKindStatefulSet:
		var statefulSet appsv1.StatefulSet
		if err := l.get(ctx, subject, &statefulSet); err != nil {
			return Availability{}, err
		}
		return StatefulSetAvailability(&statefulSet), nil
	case v1alpha1.TargetKindDaemonSet:
		var daemonSet appsv1.DaemonSet
		if err := l.get(ctx, subject, &daemonSet); err != nil {
			return Availability{}, err
		}
		return DaemonSetAvailability(&daemonSet), nil
	case v1alpha1.TargetKindJob:
		var job batchv1.Job
		if err := l.get(ctx, subject, &job); err != nil {
			return Availability{}, err
		}
		return JobAvailability(&job), nil
	case v1alpha1.TargetKindCronJob:
		var cronJob batchv1.CronJob
		if err := l.get(ctx, subject, &cronJob); err != nil {
			return Availability{}, err
		}
		return Availability{Desired: 1, Ready: 1}, nil
	case v1alpha1.TargetKindNode:
		var node corev1.Node
		if err := l.get(ctx, subject, &node); err != nil {
			return Availability{}, err
		}
		return NodeAvailability(&node), nil
	case v1alpha1.TargetKindPersistentVolumeClaim:
		var claim corev1.PersistentVolumeClaim
		if err := l.get(ctx, subject, &claim); err != nil {
			return Availability{}, err
		}
		return PersistentVolumeClaimAvailability(&claim), nil
	default:
		return Availability{}, fmt.Errorf("no availability for kind %q: %w", subject.Kind, ErrNoSubject)
	}
}

// get reads a Subject object into typed.
func (l *ClusterLookup) get(ctx context.Context, subject Subject, typed client.Object) error {
	key := client.ObjectKey{Namespace: subject.Namespace, Name: subject.Name}
	if err := l.Reader.Get(ctx, key, typed); err != nil {
		return fmt.Errorf("get %s: %w", subject.Name, err)
	}
	return nil
}

// Consumers lists the Pods that mount one claim in one namespace.
func (l *ClusterLookup) Consumers(ctx context.Context, namespace, claim string) ([]Reference, error) {
	var pods corev1.PodList
	if err := l.Reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Pods in %s: %w", namespace, err)
	}
	var consumers []Reference
	for _, pod := range pods.Items {
		if !mountsClaim(pod.Spec, claim) {
			continue
		}
		consumers = append(consumers, Reference{
			APIVersion: apiVersionCore,
			Kind:       string(v1alpha1.TargetKindPod),
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			UID:        string(pod.UID),
		})
	}
	return consumers, nil
}

// mountsClaim reports whether a Pod spec mounts the named claim.
func mountsClaim(spec corev1.PodSpec, claim string) bool {
	for _, volume := range spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

// newObject returns an empty typed object for a kind the watcher reads.
func newObject(kind string) (client.Object, error) {
	switch kind {
	case string(v1alpha1.TargetKindPod):
		return &corev1.Pod{}, nil
	case string(v1alpha1.TargetKindPersistentVolumeClaim):
		return &corev1.PersistentVolumeClaim{}, nil
	case string(v1alpha1.TargetKindNode):
		return &corev1.Node{}, nil
	case string(v1alpha1.TargetKindDeployment):
		return &appsv1.Deployment{}, nil
	case string(v1alpha1.TargetKindStatefulSet):
		return &appsv1.StatefulSet{}, nil
	case string(v1alpha1.TargetKindDaemonSet):
		return &appsv1.DaemonSet{}, nil
	case "ReplicaSet":
		return &appsv1.ReplicaSet{}, nil
	case string(v1alpha1.TargetKindJob):
		return &batchv1.Job{}, nil
	case string(v1alpha1.TargetKindCronJob):
		return &batchv1.CronJob{}, nil
	default:
		return nil, fmt.Errorf("the watcher does not read kind %q: %w", kind, ErrNoSubject)
	}
}
