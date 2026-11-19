package subject

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// maxOwnerHops bounds the owner chain. The longest documented chain is
// Pod → ReplicaSet → Deployment, so four hops leave room for a future owner
// without allowing a cycle to loop forever.
const maxOwnerHops = 4

// fromPod resolves a pod label through the controller owner chain.
func (r *Resolver) fromPod(ctx context.Context, namespace, podName string) (Result, error) {
	if namespace == "" {
		return Result{}, newIgnore("namespace-missing")
	}
	if r.Reader == nil {
		return Result{}, newIgnore("pod-owner-unresolved")
	}
	var pod corev1.Pod
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return Result{}, newIgnore("pod-not-found")
		}
		return Result{}, fmt.Errorf("read Pod %s/%s: %w", namespace, podName, err)
	}
	resolved := Subject{
		APIVersion: "v1", Kind: v1alpha1.TargetKindPod, Namespace: namespace, Name: pod.Name, UID: string(pod.UID),
	}
	meta := &pod.ObjectMeta
	for range maxOwnerHops {
		reference := metav1.GetControllerOf(meta)
		if reference == nil {
			break
		}
		if reference.Kind == "ReplicaSet" {
			// A ReplicaSet is never a Page Target, so it is only an
			// intermediate hop on the way to its Deployment. An orphaned
			// ReplicaSet leaves the Pod as the subject.
			next, err := ownerMetadata(ctx, r.Reader, reference, namespace)
			if err != nil {
				if _, ignored := IgnoreReason(err); ignored {
					return Result{}, err
				}
				if apierrors.IsNotFound(err) {
					return Result{}, newIgnore("owner-not-found")
				}
				return Result{}, fmt.Errorf("read the owner of Pod %s/%s: %w", namespace, podName, err)
			}
			meta = next
			continue
		}
		kind, ok := ParseKind(reference.Kind)
		if !ok {
			return Result{}, newIgnore("unsupported-owner-kind")
		}
		resolved = Subject{
			APIVersion: reference.APIVersion, Kind: kind, Namespace: namespace,
			Name: reference.Name, UID: string(reference.UID),
		}
		if terminalKind(reference.Kind) {
			break
		}
		next, err := ownerMetadata(ctx, r.Reader, reference, namespace)
		if err != nil {
			if _, ignored := IgnoreReason(err); ignored {
				return Result{}, err
			}
			if apierrors.IsNotFound(err) {
				return Result{}, newIgnore("owner-not-found")
			}
			return Result{}, fmt.Errorf("read the owner of Pod %s/%s: %w", namespace, podName, err)
		}
		meta = next
	}
	return Result{
		Subject: resolved, Resolution: ResolutionPodOwner, ResolvedFrom: []string{LabelPod, LabelNamespace},
	}, nil
}

// ownerMetadata reads an intermediate owner so the walk can continue. Only a
// ReplicaSet and a Job are intermediate: every other supported kind terminates
// the chain.
func ownerMetadata(
	ctx context.Context, reader client.Reader, reference *metav1.OwnerReference, namespace string,
) (*metav1.ObjectMeta, error) {
	key := client.ObjectKey{Namespace: namespace, Name: reference.Name}
	switch reference.Kind {
	case "ReplicaSet":
		var replicaSet appsv1.ReplicaSet
		if err := reader.Get(ctx, key, &replicaSet); err != nil {
			return nil, err
		}
		return &replicaSet.ObjectMeta, nil
	case "Job":
		var job batchv1.Job
		if err := reader.Get(ctx, key, &job); err != nil {
			return nil, err
		}
		return &job.ObjectMeta, nil
	default:
		return nil, newIgnore("unsupported-owner-kind")
	}
}

// terminalKind reports whether the owner chain stops at this kind.
func terminalKind(kind string) bool {
	switch v1alpha1.TargetKind(kind) {
	case v1alpha1.TargetKindPod, v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet,
		v1alpha1.TargetKindDaemonSet, v1alpha1.TargetKindCronJob:
		return true
	default:
		return false
	}
}

// Checker looks a resolved subject up in the cluster. An alert may name an
// object that no longer exists; the checker reports that rather than letting
// the alert attach to a same-named object elsewhere.
type Checker struct {
	// Reader reads the Target.
	Reader client.Reader
}

// Lookup returns the subject with its live UID filled in. found is false when
// the object does not exist. The returned subject is otherwise unchanged, so a
// caller can still record the alert as naming a missing Target.
func (c *Checker) Lookup(ctx context.Context, resolved Subject) (Subject, bool, error) {
	if c.Reader == nil || resolved.IsZero() {
		return resolved, false, nil
	}
	key := client.ObjectKey{Namespace: resolved.Namespace, Name: resolved.Name}
	var object client.Object
	switch resolved.Kind {
	case v1alpha1.TargetKindDeployment:
		object = &appsv1.Deployment{}
	case v1alpha1.TargetKindStatefulSet:
		object = &appsv1.StatefulSet{}
	case v1alpha1.TargetKindDaemonSet:
		object = &appsv1.DaemonSet{}
	case v1alpha1.TargetKindJob:
		object = &batchv1.Job{}
	case v1alpha1.TargetKindCronJob:
		object = &batchv1.CronJob{}
	case v1alpha1.TargetKindPod:
		object = &corev1.Pod{}
	case v1alpha1.TargetKindPersistentVolumeClaim:
		object = &corev1.PersistentVolumeClaim{}
	case v1alpha1.TargetKindNode:
		object = &corev1.Node{}
		key = client.ObjectKey{Name: resolved.Name}
	default:
		return resolved, false, nil
	}
	if err := c.Reader.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return resolved, false, nil
		}
		return resolved, false, fmt.Errorf("look up %s %s/%s: %w",
			resolved.Kind, resolved.Namespace, resolved.Name, err)
	}
	resolved.UID = string(object.GetUID())
	if resolved.APIVersion == "" {
		resolved.APIVersion = APIVersion(resolved.Kind)
	}
	return resolved, true, nil
}
