package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// ErrOutOfScope is wrapped by every refusal that would leave the Page's scope.
var ErrOutOfScope = errors.New("out of scope")

// ErrInvalidArguments is wrapped by a refusal whose arguments do not match the
// tool's schema.
var ErrInvalidArguments = errors.New("invalid arguments")

// The kinds the broker reads. They are constants so that the scope check, the
// catalogue, and the tests cannot drift.
const (
	groupApps  = "apps"
	groupBatch = "batch"
	versionV1  = "v1"

	kindPod                   = "Pod"
	kindEvent                 = "Event"
	kindConfigMap             = "ConfigMap"
	kindService               = "Service"
	kindEndpoints             = "Endpoints"
	kindPersistentVolumeClaim = "PersistentVolumeClaim"
	kindDeployment            = "Deployment"
	kindStatefulSet           = "StatefulSet"
	kindDaemonSet             = "DaemonSet"
	kindReplicaSet            = "ReplicaSet"
	kindJob                   = "Job"
	kindCronJob               = "CronJob"
)

// readableKinds are the namespaced kinds the broker will read. Secrets, RBAC,
// admission configuration, and everything not named here are refused: the agent
// investigates a failure, it does not browse the cluster.
var readableKinds = map[string]schema.GroupVersionKind{
	kindPod:                   {Group: "", Version: versionV1, Kind: kindPod},
	kindEvent:                 {Group: "", Version: versionV1, Kind: kindEvent},
	kindConfigMap:             {Group: "", Version: versionV1, Kind: kindConfigMap},
	kindService:               {Group: "", Version: versionV1, Kind: kindService},
	kindEndpoints:             {Group: "", Version: versionV1, Kind: kindEndpoints},
	kindPersistentVolumeClaim: {Group: "", Version: versionV1, Kind: kindPersistentVolumeClaim},
	kindDeployment:            {Group: groupApps, Version: versionV1, Kind: kindDeployment},
	kindStatefulSet:           {Group: groupApps, Version: versionV1, Kind: kindStatefulSet},
	kindDaemonSet:             {Group: groupApps, Version: versionV1, Kind: kindDaemonSet},
	kindReplicaSet:            {Group: groupApps, Version: versionV1, Kind: kindReplicaSet},
	kindJob:                   {Group: groupBatch, Version: versionV1, Kind: kindJob},
	kindCronJob:               {Group: groupBatch, Version: versionV1, Kind: kindCronJob},
}

// kinds returns the readable kind names in a stable order.
func readableKindNames() []string {
	return []string{
		kindCronJob, kindDaemonSet, kindDeployment, kindEndpoints, kindEvent, kindJob,
		kindPersistentVolumeClaim, kindPod, kindReplicaSet, kindService, kindStatefulSet,
	}
}

// scopeFor builds the enforced scope of one Page.
func scopeFor(page *v1alpha1.Page) Scope {
	scope := Scope{Kinds: readableKindNames()}
	if page.Spec.Target.Namespace != "" {
		scope.Namespaces = []string{page.Spec.Target.Namespace}
	}
	if repository, ok := RepositoryForPage(page); ok {
		scope.Repositories = []Repository{repository}
	}
	return scope
}

// repositoryFor returns the Page's Target Repository, from the declared
// reference or from the recorded resolution.
func RepositoryForPage(page *v1alpha1.Page) (Repository, bool) {
	url, branch := "", ""
	switch {
	case page.Spec.Repository != nil && page.Spec.Repository.URL != "":
		url, branch = page.Spec.Repository.URL, page.Spec.Repository.Branch
	case page.Status.Repository != nil && page.Status.Repository.URL != "":
		url, branch = page.Status.Repository.URL, page.Status.Repository.Branch
	default:
		return Repository{}, false
	}
	owner, name, ok := parseRepositoryURL(url)
	if !ok {
		return Repository{}, false
	}
	if branch == "" {
		branch = "main"
	}
	return Repository{Owner: owner, Name: name, Ref: branch}, true
}

// parseRepositoryURL extracts owner and name from an HTTPS Git URL.
func parseRepositoryURL(raw string) (string, string, bool) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(raw, "/"), ".git")
	prefix, name, found := strings.CutLast(trimmed, "/")
	if !found || prefix == "" || name == "" {
		return "", "", false
	}
	_, owner, found := strings.CutLast(prefix, "/")
	if !found {
		return "", "", false
	}
	if owner == "" || name == "" || strings.ContainsAny(owner+name, " \t\r\n") {
		return "", "", false
	}
	return owner, name, true
}

// checkNamespace refuses a read outside the Page's namespace.
func (s Scope) checkNamespace(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("%w: the read names no namespace", ErrOutOfScope)
	}
	if len(s.Namespaces) == 0 {
		return fmt.Errorf("%w: the Page has no namespace to read", ErrOutOfScope)
	}
	if slices.Contains(s.Namespaces, namespace) {
		return nil
	}
	return fmt.Errorf("%w: namespace %s is outside the Page's scope", ErrOutOfScope, namespace)
}

// checkArgumentsNamespace refuses a model-supplied namespace that is not the
// Page's. An empty argument means the model did not name one, which reads the
// Page's namespace.
func (s Scope) checkArgumentsNamespace(requested, pageNamespace string) error {
	if requested != "" && requested != pageNamespace {
		return fmt.Errorf("%w: namespace %s is outside the Page's scope", ErrOutOfScope, requested)
	}
	return s.checkNamespace(pageNamespace)
}

// checkKind refuses an object kind the broker does not read, including Secret
// and every cluster-scoped kind.
func (s Scope) checkKind(kind string) error {
	if _, ok := readableKinds[kind]; ok {
		return nil
	}
	return fmt.Errorf("%w: kind %s is not a readable namespaced kind", ErrOutOfScope, kind)
}

// gvk resolves a kind and optional apiVersion to the group/version/kind the
// reader uses. A mismatched apiVersion is refused rather than guessed.
func gvk(kind, apiVersion string) (schema.GroupVersionKind, error) {
	known, ok := readableKinds[kind]
	if !ok {
		return schema.GroupVersionKind{}, fmt.Errorf("%w: kind %s is not a readable namespaced kind", ErrOutOfScope, kind)
	}
	if apiVersion == "" {
		return known, nil
	}
	group, version, found := strings.Cut(apiVersion, "/")
	if !found {
		group, version = "", apiVersion
	}
	resolved := schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
	if resolved.Group != known.Group || resolved.Kind != known.Kind {
		return schema.GroupVersionKind{},
			fmt.Errorf("%w: %s %s is not the kind %s", ErrOutOfScope, apiVersion, kind, known.Kind)
	}
	return resolved, nil
}

// targetPodNames returns the pods that belong to the Page's Target. A pod that
// is not in the set is refused, so a model cannot read another workload's logs
// by naming its pod.
func targetPodNames(
	ctx context.Context, reader client.Reader, target v1alpha1.TargetReference,
) (map[string]struct{}, error) {
	pods := map[string]struct{}{}
	if target.Kind == v1alpha1.TargetKindPod {
		pods[target.Name] = struct{}{}
		return pods, nil
	}
	if target.Namespace == "" {
		return pods, nil
	}
	var list corev1.PodList
	if err := reader.List(ctx, &list, client.InNamespace(target.Namespace)); err != nil {
		return nil, fmt.Errorf("list the Target's pods: %w", err)
	}
	for i := range list.Items {
		pod := &list.Items[i]
		if podBelongsToTarget(ctx, reader, pod, target) {
			pods[pod.Name] = struct{}{}
		}
	}
	return pods, nil
}

// podBelongsToTarget walks a pod's owner chain to the Target. A pod whose chain
// does not reach the Target is not evidence for this Page.
func podBelongsToTarget(
	ctx context.Context, reader client.Reader, pod *corev1.Pod, target v1alpha1.TargetReference,
) bool {
	if pod.UID != "" && string(pod.UID) == target.UID {
		return true
	}
	if target.Kind == v1alpha1.TargetKindPod && pod.Name == target.Name {
		return true
	}
	var current metav1.Object = pod
	for range 8 {
		owner := metav1.GetControllerOf(current)
		if owner == nil {
			return false
		}
		if owner.Kind == string(target.Kind) && owner.Name == target.Name {
			return true
		}
		next := &unstructured.Unstructured{}
		next.SetGroupVersionKind(schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind))
		if err := reader.Get(ctx, client.ObjectKey{Namespace: current.GetNamespace(), Name: owner.Name}, next); err != nil {
			return false
		}
		if string(next.GetUID()) != string(owner.UID) {
			return false
		}
		current = next
	}
	return false
}
