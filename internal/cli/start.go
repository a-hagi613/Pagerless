package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// target kinds that the start command accepts.
const (
	targetPod         = "pod"
	targetDeployment  = "deployment"
	targetStatefulSet = "statefulset"
)

// startOptions holds what the start action needs from the command line.
type startOptions struct {
	conn *connection
	hint string
}

// run starts a Triage for the Workload that the argument names, or prints the
// name of the Active Triage that already covers it.
func (o *startOptions) run(cmd *cobra.Command, args []string) error {
	target, err := parseTarget(args[0])
	if err != nil {
		return err
	}
	cl, err := o.conn.connect()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	workload, pod, err := resolveTarget(ctx, cl.client, cl.namespace, target)
	if err != nil {
		return err
	}

	tr := newTriage(workload, pod, o.hint)
	out := &printer{w: cmd.OutOrStdout()}
	if err := cl.client.Create(ctx, tr); err != nil {
		name, ok := existingActiveTriage(ctx, cl.client, err, workload)
		if !ok {
			return fmt.Errorf("create Triage: %w", err)
		}
		out.printf("%s already active\n", triageRef(name))
		return out.err
	}
	out.printf("%s created\n", triageRef(tr.Name))
	return out.err
}

// target is the resource that a start request names.
type target struct {
	kind string
	name string
}

// parseTarget reads a target such as pod/checkout-7d9f-x2x4q or deploy/checkout.
func parseTarget(arg string) (target, error) {
	resource, name, ok := strings.Cut(arg, "/")
	if !ok || name == "" {
		return target{}, fmt.Errorf("invalid target %q: want pod/<name>, deploy/<name>, or sts/<name>", arg)
	}
	switch resource {
	case "pod":
		return target{kind: targetPod, name: name}, nil
	case "deploy", "deployment":
		return target{kind: targetDeployment, name: name}, nil
	case "sts", "statefulset":
		return target{kind: targetStatefulSet, name: name}, nil
	default:
		return target{}, fmt.Errorf("invalid target %q: unknown resource %q, want pod, deploy, or sts", arg, resource)
	}
}

// resolveTarget returns the Workload that a target belongs to. For a pod, it
// also returns the pod's name, which becomes the Trigger's pod.
func resolveTarget(
	ctx context.Context, cl client.Client, namespace string, t target,
) (v1alpha1.WorkloadReference, string, error) {
	if t.kind == targetPod {
		kind, name, err := ownerOfPod(ctx, cl, namespace, t.name)
		if err != nil {
			return v1alpha1.WorkloadReference{}, "", err
		}
		ref, err := confirmWorkload(ctx, cl, namespace, kind, name)
		return ref, t.name, err
	}
	kind := v1alpha1.WorkloadKindDeployment
	if t.kind == targetStatefulSet {
		kind = v1alpha1.WorkloadKindStatefulSet
	}
	ref, err := confirmWorkload(ctx, cl, namespace, kind, t.name)
	return ref, "", err
}

// ownerOfPod follows a pod's controller to the Workload that owns it. A pod of
// a Deployment is owned by a ReplicaSet, which is owned by the Deployment.
func ownerOfPod(
	ctx context.Context, cl client.Client, namespace, name string,
) (v1alpha1.WorkloadKind, string, error) {
	var pod corev1.Pod
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
		return "", "", notFoundOr(err, "pod", namespace, name)
	}
	owner := metav1.GetControllerOf(&pod)
	if owner == nil {
		return "", "", fmt.Errorf("pod %s/%s has no owner, so it belongs to no Workload", namespace, name)
	}
	switch owner.Kind {
	case "StatefulSet":
		return v1alpha1.WorkloadKindStatefulSet, owner.Name, nil
	case "ReplicaSet":
		var replicaSet appsv1.ReplicaSet
		key := client.ObjectKey{Namespace: namespace, Name: owner.Name}
		if err := cl.Get(ctx, key, &replicaSet); err != nil {
			return "", "", notFoundOr(err, "ReplicaSet", namespace, owner.Name)
		}
		deployment := metav1.GetControllerOf(&replicaSet)
		if deployment == nil || deployment.Kind != "Deployment" {
			return "", "", fmt.Errorf("pod %s/%s belongs to ReplicaSet %s, which has no Deployment",
				namespace, name, owner.Name)
		}
		return v1alpha1.WorkloadKindDeployment, deployment.Name, nil
	default:
		return "", "", fmt.Errorf("pod %s/%s is owned by %s %s, and a Triage targets a Deployment or StatefulSet",
			namespace, name, owner.Kind, owner.Name)
	}
}

// confirmWorkload checks that the Workload exists and returns its reference.
func confirmWorkload(
	ctx context.Context, cl client.Client, namespace string, kind v1alpha1.WorkloadKind, name string,
) (v1alpha1.WorkloadReference, error) {
	var obj client.Object = &appsv1.Deployment{}
	if kind == v1alpha1.WorkloadKindStatefulSet {
		obj = &appsv1.StatefulSet{}
	}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return v1alpha1.WorkloadReference{}, notFoundOr(err, strings.ToLower(string(kind)), namespace, name)
	}
	return v1alpha1.WorkloadReference{Kind: kind, Namespace: namespace, Name: name}, nil
}

// notFoundOr describes a missing object in words, and wraps any other error.
func notFoundOr(err error, kind, namespace, name string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %q not found in namespace %q", strings.ToLower(kind), name, namespace)
	}
	return fmt.Errorf("get %s %s/%s: %w", strings.ToLower(kind), namespace, name, err)
}

// newTriage returns a Triage for a Workload that the CLI starts.
func newTriage(workload v1alpha1.WorkloadReference, pod, hint string) *v1alpha1.Triage {
	return &v1alpha1.Triage{
		GenerateName: workload.Name + "-",
		Namespace:    TriageNamespace,
		Spec: v1alpha1.TriageSpec{
			Workload: workload,
			Hint:     hint,
			Trigger:  v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI, Pod: pod},
		},
	}
}

// existingActiveTriage returns the name of the Active Triage that a refused
// creation names. The admission webhook refuses a second Active Triage for a
// Workload with a conflict that carries the existing Triage's name. The name
// is confirmed before it is trusted.
func existingActiveTriage(
	ctx context.Context, cl client.Client, createErr error, workload v1alpha1.WorkloadReference,
) (string, bool) {
	var status apierrors.APIStatus
	if !errors.As(createErr, &status) || !apierrors.IsAlreadyExists(createErr) {
		return "", false
	}
	details := status.Status().Details
	if details == nil || details.Name == "" {
		return "", false
	}
	var existing v1alpha1.Triage
	key := client.ObjectKey{Namespace: TriageNamespace, Name: details.Name}
	if err := cl.Get(ctx, key, &existing); err != nil {
		return "", false
	}
	if existing.Spec.Workload != workload || !existing.Status.Phase.IsActive() {
		return "", false
	}
	return existing.Name, true
}

// triageRef returns the resource reference of a Triage, as kubectl prints it.
func triageRef(name string) string {
	return "triage." + v1alpha1.GroupVersion.Group + "/" + name
}
