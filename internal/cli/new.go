package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// appsAPIVersion is the API version of the apps kinds.
const appsAPIVersion = "apps/v1"

// manualReason is the reason class that a human request contributes to the
// Fingerprint. It separates a human-requested investigation from the watcher's
// failure fingerprints for the same Target.
const manualReason = "ManualRequest"

// targetArg is the resource that a new request names.
type targetArg struct {
	kind v1alpha1.TargetKind
	name string
}

// targetForms maps each accepted kind prefix to its Target kind, in the order
// the error message lists them.
var targetForms = []struct {
	prefix string
	kind   v1alpha1.TargetKind
}{
	{"deployment", v1alpha1.TargetKindDeployment},
	{"deploy", v1alpha1.TargetKindDeployment},
	{"statefulset", v1alpha1.TargetKindStatefulSet},
	{"sts", v1alpha1.TargetKindStatefulSet},
	{"daemonset", v1alpha1.TargetKindDaemonSet},
	{"ds", v1alpha1.TargetKindDaemonSet},
	{"job", v1alpha1.TargetKindJob},
	{"cronjob", v1alpha1.TargetKindCronJob},
	{"cj", v1alpha1.TargetKindCronJob},
	{"pod", v1alpha1.TargetKindPod},
	{"persistentvolumeclaim", v1alpha1.TargetKindPersistentVolumeClaim},
	{"pvc", v1alpha1.TargetKindPersistentVolumeClaim},
	{"node", v1alpha1.TargetKindNode},
}

// acceptedForms is the list a refusal names.
const acceptedForms = "deployment/<name>, statefulset/<name>, daemonset/<name>, job/<name>, cronjob/<name>, " +
	"pod/<name>, pvc/<name>, or node/<name>"

// parseTarget reads a kind-prefixed Target such as deployment/checkout. A bare
// name is refused because a Deployment, a Job, and a Node may share it.
func parseTarget(arg string) (targetArg, error) {
	prefix, name, ok := strings.Cut(strings.TrimSpace(arg), "/")
	if !ok || name == "" {
		return targetArg{}, usagef("a Target must name its kind: use %s", acceptedForms)
	}
	for _, form := range targetForms {
		if prefix == form.prefix {
			return targetArg{kind: form.kind, name: name}, nil
		}
	}
	return targetArg{}, usagef("unknown Target kind %q: use %s", prefix, acceptedForms)
}

// newNewCommand returns the command that opens a Page for a Target.
func newNewCommand(opts *options) *cobra.Command {
	var hint string
	cmd := &cobra.Command{
		Use:   "new <kind>/<name>",
		Short: "Open a Page for a Deployment, StatefulSet, DaemonSet, Job, CronJob, Pod, PVC, or Node",
		Long: `Open a Page for the Target that a kind-prefixed argument names.

A pod is resolved to its owning Deployment or StatefulSet. A Node is
cluster-scoped, so it takes no namespace; every other kind does. When an Active
Page already covers the same failure, the command prints that Page instead of
opening a second one.`,
		Args: exactlyOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := parseTarget(args[0])
			if err != nil {
				return err
			}
			s, err := openSession(cmd, opts)
			if err != nil {
				return err
			}
			namespace, err := s.targetNamespace(target.kind)
			if err != nil {
				return err
			}
			ref, pod, err := s.resolve(cmd.Context(), namespace, target)
			if err != nil {
				return err
			}
			name, active, err := s.create(cmd.Context(), ref, pod, hint)
			if err != nil {
				return err
			}
			out := &printer{w: cmd.OutOrStdout()}
			if active {
				out.printf("%s already active\n", pageRef(name))
			} else {
				out.printf("%s created\n", pageRef(name))
			}
			return out.err
		},
	}
	cmd.Flags().StringVar(&hint, "hint", "",
		"guidance for the agent, passed to it as human input (at most 1000 characters)")
	return cmd
}

// targetNamespace returns the namespace of a Target, or an empty string for a
// Node. Server mode needs an explicit namespace because it cannot read the
// kubeconfig's current one.
func (s *session) targetNamespace(kind v1alpha1.TargetKind) (string, error) {
	if kind.IsClusterScoped() {
		return "", nil
	}
	if s.opts.namespace != "" {
		return s.opts.namespace, nil
	}
	if s.clus != nil && s.clus.namespace != "" {
		return s.clus.namespace, nil
	}
	return "", usagef("a namespaced Target needs a namespace: pass -n")
}

// resolve returns the Target reference for a request and, for a pod, its name,
// which becomes the Trigger's pod.
func (s *session) resolve(
	ctx context.Context, namespace string, target targetArg,
) (v1alpha1.TargetReference, string, error) {
	if s.api != nil {
		// The server resolves the Target's UID and refuses a missing object.
		return v1alpha1.TargetReference{Kind: target.kind, Namespace: namespace, Name: target.name}, "", nil
	}
	if target.kind == v1alpha1.TargetKindPod {
		kind, name, err := ownerOfPod(ctx, s.clus.client, namespace, target.name)
		if err != nil {
			return v1alpha1.TargetReference{}, "", err
		}
		return v1alpha1.TargetReference{Kind: kind, Namespace: namespace, Name: name}, target.name, nil
	}
	return v1alpha1.TargetReference{Kind: target.kind, Namespace: namespace, Name: target.name}, "", nil
}

// confirmTarget checks that the Target exists and fills its API version and UID.
// The UID is part of the Fingerprint, so a recreated Target is a new failure
// rather than a continuation of the old Page.
func confirmTarget(ctx context.Context, s *session, ref v1alpha1.TargetReference) (v1alpha1.TargetReference, error) {
	if s.api != nil {
		return ref, nil
	}
	obj, apiVersion, err := targetObject(ref.Kind)
	if err != nil {
		return ref, err
	}
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := s.clus.client.Get(ctx, key, obj); err != nil {
		return ref, notFoundOr(err, string(ref.Kind), ref.Namespace, ref.Name)
	}
	ref.APIVersion = apiVersion
	ref.UID = string(obj.GetUID())
	return ref, nil
}

// targetObject returns an empty object of the Target's kind and its API version.
func targetObject(kind v1alpha1.TargetKind) (client.Object, string, error) {
	switch kind {
	case v1alpha1.TargetKindDeployment:
		return &appsv1.Deployment{}, appsAPIVersion, nil
	case v1alpha1.TargetKindStatefulSet:
		return &appsv1.StatefulSet{}, appsAPIVersion, nil
	case v1alpha1.TargetKindDaemonSet:
		return &appsv1.DaemonSet{}, appsAPIVersion, nil
	case v1alpha1.TargetKindJob:
		return &batchv1.Job{}, "batch/v1", nil
	case v1alpha1.TargetKindCronJob:
		return &batchv1.CronJob{}, "batch/v1", nil
	case v1alpha1.TargetKindPod:
		return &corev1.Pod{}, "v1", nil
	case v1alpha1.TargetKindPersistentVolumeClaim:
		return &corev1.PersistentVolumeClaim{}, "v1", nil
	case v1alpha1.TargetKindNode:
		return &corev1.Node{}, "v1", nil
	default:
		return nil, "", fmt.Errorf("unsupported Target kind %q", kind)
	}
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

// notFoundOr describes a missing object in words, and wraps any other error.
func notFoundOr(err error, kind, namespace, name string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %q not found in namespace %q", strings.ToLower(kind), name, namespace)
	}
	return fmt.Errorf("get %s %s/%s: %w", strings.ToLower(kind), namespace, name, err)
}

// newPage returns a Page for a Target that the CLI starts.
func newPage(target v1alpha1.TargetReference, namespace, hint, pod string) *v1alpha1.Page {
	return &v1alpha1.Page{
		GenerateName: target.Name + "-",
		Namespace:    namespace,
		Spec: v1alpha1.PageSpec{
			Target:      target,
			Fingerprint: v1alpha1.Fingerprint(target, manualReason),
			Hint:        hint,
			Trigger:     v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI, Pod: pod},
		},
	}
}

// activePage returns the name of the Active Page that already covers the
// Fingerprint. The chart installs no admission webhook, so the CLI checks before
// it creates and the controller supersedes a duplicate that races with it.
func activePage(ctx context.Context, cl client.Client, namespace, fingerprint string) (string, bool, error) {
	var list v1alpha1.PageList
	if err := cl.List(ctx, &list, client.InNamespace(namespace)); err != nil {
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
