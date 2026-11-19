package cli

import (
	"context"
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

// manualReason is the reason class that a human request contributes to the
// Fingerprint. It separates a human-requested investigation from the watcher's
// failure fingerprints for the same Target.
const manualReason = "ManualRequest"

// target resources that the start command accepts.
const (
	targetPod         = "pod"
	targetDeployment  = "deploy"
	targetStatefulSet = "statefulset"
)

// startOptions holds what the start action needs from the command line.
type startOptions struct {
	conn *connection
	hint string
}

// run starts a Page for the Target that the argument names, or prints the name
// of the Active Page that already covers it.
func (o *startOptions) run(cmd *cobra.Command, args []string) error {
	requested, err := parseTarget(args[0])
	if err != nil {
		return err
	}
	cl, err := o.conn.connect()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	target, pod, err := resolveTarget(ctx, cl.client, cl.namespace, requested)
	if err != nil {
		return err
	}
	p := newPage(target, pod, o.hint)
	out := &printer{w: cmd.OutOrStdout()}
	if name, ok, err := activePage(ctx, cl.client, p.Spec.Fingerprint); err != nil {
		return err
	} else if ok {
		out.printf("%s already active\n", pageRef(name))
		return out.err
	}
	if err := cl.client.Create(ctx, p); err != nil {
		return fmt.Errorf("create Page: %w", err)
	}
	out.printf("%s created\n", pageRef(p.Name))
	return out.err
}

// targetArg is the resource that a start request names.
type targetArg struct {
	resource string
	name     string
}

// parseTarget reads a target such as pod/checkout-7d9f-x2x4q or deploy/checkout.
func parseTarget(arg string) (targetArg, error) {
	resource, name, ok := strings.Cut(arg, "/")
	if !ok || name == "" {
		return targetArg{}, fmt.Errorf("invalid target %q: want pod/<name>, deploy/<name>, or sts/<name>", arg)
	}
	switch resource {
	case "pod":
		return targetArg{resource: targetPod, name: name}, nil
	case "deploy", "deployment":
		return targetArg{resource: targetDeployment, name: name}, nil
	case "sts", "statefulset":
		return targetArg{resource: targetStatefulSet, name: name}, nil
	default:
		return targetArg{}, fmt.Errorf("invalid target %q: unknown resource %q, want pod, deploy, or sts", arg, resource)
	}
}

// resolveTarget returns the Target that a request names. For a pod, it also
// returns the pod's name, which becomes the Trigger's pod.
func resolveTarget(
	ctx context.Context, cl client.Client, namespace string, t targetArg,
) (v1alpha1.TargetReference, string, error) {
	if t.resource == targetPod {
		kind, name, err := ownerOfPod(ctx, cl, namespace, t.name)
		if err != nil {
			return v1alpha1.TargetReference{}, "", err
		}
		ref, err := confirmTarget(ctx, cl, namespace, kind, name)
		return ref, t.name, err
	}
	kind := v1alpha1.TargetKindDeployment
	if t.resource == targetStatefulSet {
		kind = v1alpha1.TargetKindStatefulSet
	}
	ref, err := confirmTarget(ctx, cl, namespace, kind, t.name)
	return ref, "", err
}

// ownerOfPod follows a pod's controller to the Target that owns it. A pod of a
// Deployment is owned by a ReplicaSet, which is owned by the Deployment.
func ownerOfPod(
	ctx context.Context, cl client.Client, namespace, name string,
) (v1alpha1.TargetKind, string, error) {
	var pod corev1.Pod
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
		return "", "", notFoundOr(err, "pod", namespace, name)
	}
	owner := metav1.GetControllerOf(&pod)
	if owner == nil {
		return "", "", fmt.Errorf("pod %s/%s has no owner, so it belongs to no Target", namespace, name)
	}
	switch owner.Kind {
	case "StatefulSet":
		return v1alpha1.TargetKindStatefulSet, owner.Name, nil
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
		return v1alpha1.TargetKindDeployment, deployment.Name, nil
	default:
		return "", "", fmt.Errorf("pod %s/%s is owned by %s %s, and this command targets a Deployment or StatefulSet",
			namespace, name, owner.Kind, owner.Name)
	}
}

// confirmTarget checks that the Target exists and returns its reference.
func confirmTarget(
	ctx context.Context, cl client.Client, namespace string, kind v1alpha1.TargetKind, name string,
) (v1alpha1.TargetReference, error) {
	var obj client.Object = &appsv1.Deployment{}
	apiVersion := "apps/v1"
	if kind == v1alpha1.TargetKindStatefulSet {
		obj = &appsv1.StatefulSet{}
	}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		return v1alpha1.TargetReference{}, notFoundOr(err, strings.ToLower(string(kind)), namespace, name)
	}
	return v1alpha1.TargetReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Namespace:  namespace,
		Name:       name,
		UID:        string(obj.GetUID()),
	}, nil
}

// notFoundOr describes a missing object in words, and wraps any other error.
func notFoundOr(err error, kind, namespace, name string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %q not found in namespace %q", strings.ToLower(kind), name, namespace)
	}
	return fmt.Errorf("get %s %s/%s: %w", strings.ToLower(kind), namespace, name, err)
}

// newPage returns a Page for a Target that the CLI starts.
func newPage(target v1alpha1.TargetReference, pod, hint string) *v1alpha1.Page {
	fingerprint := v1alpha1.Fingerprint(target, manualReason)
	return &v1alpha1.Page{
		GenerateName: target.Name + "-",
		Namespace:    PageNamespace,
		Spec: v1alpha1.PageSpec{
			Target:      target,
			Fingerprint: fingerprint,
			Hint:        hint,
			Trigger:     v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI, Pod: pod},
		},
	}
}

// activePage returns the name of the Active Page that already covers the
// Fingerprint. The chart installs no admission webhook, so the CLI checks before
// it creates and the controller supersedes a duplicate that races with it.
func activePage(ctx context.Context, cl client.Client, fingerprint string) (string, bool, error) {
	var list v1alpha1.PageList
	if err := cl.List(ctx, &list, client.InNamespace(PageNamespace)); err != nil {
		return "", false, fmt.Errorf("list Pages: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Fingerprint == fingerprint && list.Items[i].Status.Phase.IsActive() {
			return list.Items[i].Name, true, nil
		}
	}
	return "", false, nil
}

// pageRef returns the resource reference of a Page, as kubectl prints it.
func pageRef(name string) string {
	return "page." + v1alpha1.GroupVersion.Group + "/" + name
}
