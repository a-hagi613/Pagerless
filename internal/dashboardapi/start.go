package dashboardapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// appsAPIVersion is the API version of the apps kinds.
const appsAPIVersion = "apps/v1"

// humanRequestReason is the reason class that a human request contributes to the
// Fingerprint.
const humanRequestReason = "ManualRequest"

// startRequest is the body of POST /api/pages. It accepts the CLI's nested
// target and the Portal's flat kind/namespace/name, so both surfaces create the
// same Page.
type startRequest struct {
	// Target is the object to investigate.
	Target v1alpha1.TargetReference `json:"target"`
	// Kind, Namespace, and Name are the flat form.
	Kind      v1alpha1.TargetKind `json:"kind"`
	Namespace string              `json:"namespace"`
	Name      string              `json:"name"`
	// Hint is optional guidance for the agent.
	Hint string `json:"hint"`
}

// target returns the Target reference, whichever form the caller sent.
func (r startRequest) target() v1alpha1.TargetReference {
	if r.Kind != "" || r.Name != "" {
		return v1alpha1.TargetReference{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name}
	}
	return r.Target
}

// startResponse names the Page that now covers the Target. AlreadyActive
// is true when an Active Page existed, in which case this request started
// nothing and Name is that Page.
type startResponse struct {
	Name          string `json:"name"`
	AlreadyActive bool   `json:"alreadyActive"`
}

// targetListResponse is the body of GET /api/targets.
type targetListResponse struct {
	Targets []v1alpha1.TargetReference `json:"targets"`
}

// targetList answers with the Deployments and StatefulSets that Start Page
// may target, so the browser offers a choice instead of a free-text name. With
// no configured allowlist, every namespace the read identity can see is
// offered; the chart passes the list when it restricts the watch.
func (h *Handler) targetList(w http.ResponseWriter, r *http.Request) {
	namespaces := h.namespaces
	if len(namespaces) == 0 {
		var list corev1.NamespaceList
		if err := h.targets.List(r.Context(), &list); err != nil {
			h.internal(w, r.Context(), fmt.Errorf("list namespaces: %w", err))
			return
		}
		namespaces = make([]string, 0, len(list.Items))
		for i := range list.Items {
			namespaces = append(namespaces, list.Items[i].Name)
		}
		slices.Sort(namespaces)
	}
	var targets []v1alpha1.TargetReference
	for _, namespace := range namespaces {
		var deployments appsv1.DeploymentList
		if err := h.targets.List(r.Context(), &deployments, client.InNamespace(namespace)); err != nil {
			h.internal(w, r.Context(), fmt.Errorf("list Deployments in %s: %w", namespace, err))
			return
		}
		for i := range deployments.Items {
			targets = append(targets, v1alpha1.TargetReference{
				Kind: v1alpha1.TargetKindDeployment, Namespace: namespace, Name: deployments.Items[i].Name,
			})
		}
		var statefulSets appsv1.StatefulSetList
		if err := h.targets.List(r.Context(), &statefulSets, client.InNamespace(namespace)); err != nil {
			h.internal(w, r.Context(), fmt.Errorf("list StatefulSets in %s: %w", namespace, err))
			return
		}
		for i := range statefulSets.Items {
			targets = append(targets, v1alpha1.TargetReference{
				Kind: v1alpha1.TargetKindStatefulSet, Namespace: namespace, Name: statefulSets.Items[i].Name,
			})
		}
	}
	slices.SortFunc(targets, func(a, b v1alpha1.TargetReference) int {
		if order := strings.Compare(a.Namespace, b.Namespace); order != 0 {
			return order
		}
		if order := strings.Compare(string(a.Kind), string(b.Kind)); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
	if targets == nil {
		targets = []v1alpha1.TargetReference{}
	}
	writeJSON(w, http.StatusOK, targetListResponse{Targets: targets})
}

// start creates a Page for the chosen Target with the optional Hint, from
// the Dashboard Trigger. A Target has at most one Active Page, so when one
// exists the answer names it and reports that nothing was started, which is the
// same behaviour as the CLI (ADR-0018).
func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	if h.requireCSRF(w, r) == nil {
		return
	}
	var request startRequest
	if !decodeWrite(w, r, &request) {
		return
	}
	if err := h.validateStart(request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	target := request.target()
	if err := h.confirmTarget(ctx, &target); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	fingerprint := v1alpha1.Fingerprint(target, humanRequestReason)
	if name, ok, err := h.activePage(ctx, fingerprint); err != nil {
		h.internal(w, ctx, err)
	} else if ok {
		writeJSON(w, http.StatusOK, startResponse{Name: name, AlreadyActive: true})
		return
	}
	page := &v1alpha1.Page{
		GenerateName: target.Name + "-",
		Namespace:    h.namespace,
		Spec: v1alpha1.PageSpec{
			Target:      target,
			Fingerprint: fingerprint,
			Hint:        strings.TrimSpace(request.Hint),
			Trigger:     v1alpha1.Trigger{Source: v1alpha1.TriggerSourcePortal, Reason: humanRequestReason},
		},
	}
	if err := h.writer.Create(ctx, page); err != nil {
		h.writeWriteError(w, ctx, err)
		return
	}
	writeJSON(w, http.StatusCreated, startResponse{Name: page.Name})
}

// validateStart checks the request before anything reaches the API server.
func (h *Handler) validateStart(request startRequest) error {
	target := request.target()
	switch target.Kind {
	case v1alpha1.TargetKindDeployment, v1alpha1.TargetKindStatefulSet, v1alpha1.TargetKindDaemonSet,
		v1alpha1.TargetKindJob, v1alpha1.TargetKindCronJob, v1alpha1.TargetKindPod,
		v1alpha1.TargetKindPersistentVolumeClaim, v1alpha1.TargetKindNode:
	default:
		return fmt.Errorf("target.kind must be one of Deployment, StatefulSet, DaemonSet, Job, CronJob, Pod, " +
			"PersistentVolumeClaim, or Node")
	}
	if target.Name == "" {
		return errors.New("target.name is required")
	}
	if target.Kind.IsClusterScoped() {
		if target.Namespace != "" {
			return fmt.Errorf("target.namespace must be empty for a %s", target.Kind)
		}
	} else {
		if target.Namespace == "" {
			return fmt.Errorf("target.namespace is required for a %s", target.Kind)
		}
		// The Portal only offers the configured namespaces. An empty list means
		// the chart did not restrict the offer, so the server's read RBAC decides.
		if len(h.namespaces) > 0 && !slices.Contains(h.namespaces, target.Namespace) {
			return fmt.Errorf("start Page does not offer Targets in namespace %q", target.Namespace)
		}
	}
	if len(request.Hint) > 1000 {
		return fmt.Errorf("the hint is %d characters long; at most 1000 are allowed", len(request.Hint))
	}
	return nil
}

// confirmTarget checks that the Target exists, so a typo cannot start a
// Page that can never gather a Context Bundle.
func (h *Handler) confirmTarget(ctx context.Context, ref *v1alpha1.TargetReference) error {
	obj, apiVersion, err := targetObject(ref.Kind)
	if err != nil {
		return err
	}
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := h.targets.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%s %q not found in namespace %q", strings.ToLower(string(ref.Kind)), ref.Name, ref.Namespace)
		}
		return fmt.Errorf("read %s %s/%s: %w", strings.ToLower(string(ref.Kind)), ref.Namespace, ref.Name, err)
	}
	// The UID is part of the Fingerprint, so a recreated Target is a new failure
	// rather than a continuation of the old Page.
	ref.APIVersion = apiVersion
	ref.UID = string(obj.GetUID())
	return nil
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

// activePage returns the canonical Active Page for the Fingerprint, if any.
// The chart installs no admission webhook, so the Portal checks before it
// creates and the controller supersedes a duplicate that races with it.
func (h *Handler) activePage(ctx context.Context, fingerprint string) (string, bool, error) {
	var list v1alpha1.PageList
	if err := h.reader.List(ctx, &list, client.InNamespace(h.namespace)); err != nil {
		return "", false, fmt.Errorf("list Pages: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Fingerprint == fingerprint && list.Items[i].Status.Phase.IsActive() {
			return list.Items[i].Name, true, nil
		}
	}
	return "", false, nil
}
