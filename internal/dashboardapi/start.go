package dashboardapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// startRequest is the body of POST /api/triages.
type startRequest struct {
	// Workload is the Deployment or StatefulSet to investigate.
	Workload v1alpha1.WorkloadReference `json:"workload"`
	// Hint is optional guidance for the agent.
	Hint string `json:"hint"`
}

// startResponse names the Triage that now covers the Workload. AlreadyActive
// is true when an Active Triage existed, in which case this request started
// nothing and Name is that Triage.
type startResponse struct {
	Name          string `json:"name"`
	AlreadyActive bool   `json:"alreadyActive"`
}

// workloadListResponse is the body of GET /api/workloads.
type workloadListResponse struct {
	Workloads []v1alpha1.WorkloadReference `json:"workloads"`
}

// workloadList answers with the Deployments and StatefulSets that Start Triage
// may target, so the browser offers a choice instead of a free-text name.
func (h *Handler) workloadList(w http.ResponseWriter, r *http.Request) {
	var workloads []v1alpha1.WorkloadReference
	for _, namespace := range h.namespaces {
		var deployments appsv1.DeploymentList
		if err := h.workloads.List(r.Context(), &deployments, client.InNamespace(namespace)); err != nil {
			h.internal(w, r.Context(), fmt.Errorf("list Deployments in %s: %w", namespace, err))
			return
		}
		for i := range deployments.Items {
			workloads = append(workloads, v1alpha1.WorkloadReference{
				Kind: v1alpha1.WorkloadKindDeployment, Namespace: namespace, Name: deployments.Items[i].Name,
			})
		}
		var statefulSets appsv1.StatefulSetList
		if err := h.workloads.List(r.Context(), &statefulSets, client.InNamespace(namespace)); err != nil {
			h.internal(w, r.Context(), fmt.Errorf("list StatefulSets in %s: %w", namespace, err))
			return
		}
		for i := range statefulSets.Items {
			workloads = append(workloads, v1alpha1.WorkloadReference{
				Kind: v1alpha1.WorkloadKindStatefulSet, Namespace: namespace, Name: statefulSets.Items[i].Name,
			})
		}
	}
	slices.SortFunc(workloads, func(a, b v1alpha1.WorkloadReference) int {
		if order := strings.Compare(a.Namespace, b.Namespace); order != 0 {
			return order
		}
		if order := strings.Compare(string(a.Kind), string(b.Kind)); order != 0 {
			return order
		}
		return strings.Compare(a.Name, b.Name)
	})
	if workloads == nil {
		workloads = []v1alpha1.WorkloadReference{}
	}
	writeJSON(w, http.StatusOK, workloadListResponse{Workloads: workloads})
}

// start creates a Triage for the chosen Workload with the optional Hint, from
// the Dashboard Trigger. A Workload has at most one Active Triage, so when one
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
	if err := h.confirmWorkload(ctx, request.Workload); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	tr := &v1alpha1.Triage{
		GenerateName: request.Workload.Name + "-",
		Namespace:    h.namespace,
		Spec: v1alpha1.TriageSpec{
			Workload: request.Workload,
			Hint:     strings.TrimSpace(request.Hint),
			Trigger:  v1alpha1.Trigger{Source: v1alpha1.TriggerSourceDashboard},
		},
	}
	if err := h.writer.Create(ctx, tr); err != nil {
		if name, ok := h.activeTriage(ctx, err, request.Workload); ok {
			writeJSON(w, http.StatusOK, startResponse{Name: name, AlreadyActive: true})
			return
		}
		h.writeWriteError(w, ctx, err)
		return
	}
	writeJSON(w, http.StatusCreated, startResponse{Name: tr.Name})
}

// validateStart checks the request before anything reaches the API server.
func (h *Handler) validateStart(request startRequest) error {
	switch request.Workload.Kind {
	case v1alpha1.WorkloadKindDeployment, v1alpha1.WorkloadKindStatefulSet:
	default:
		return fmt.Errorf("workload.kind must be Deployment or StatefulSet")
	}
	if request.Workload.Name == "" {
		return errors.New("workload.name is required")
	}
	if !slices.Contains(h.namespaces, request.Workload.Namespace) {
		return fmt.Errorf("start Triage does not offer Workloads in namespace %q", request.Workload.Namespace)
	}
	if len(request.Hint) > 1000 {
		return fmt.Errorf("the hint is %d characters long; at most 1000 are allowed", len(request.Hint))
	}
	return nil
}

// confirmWorkload checks that the Workload exists, so a typo cannot start a
// Triage that can never gather a Context Bundle.
func (h *Handler) confirmWorkload(ctx context.Context, ref v1alpha1.WorkloadReference) error {
	var obj client.Object = &appsv1.Deployment{}
	if ref.Kind == v1alpha1.WorkloadKindStatefulSet {
		obj = &appsv1.StatefulSet{}
	}
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := h.workloads.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%s %q not found in namespace %q", strings.ToLower(string(ref.Kind)), ref.Name, ref.Namespace)
		}
		return fmt.Errorf("read %s %s/%s: %w", strings.ToLower(string(ref.Kind)), ref.Namespace, ref.Name, err)
	}
	return nil
}

// activeTriage returns the name of the Active Triage that a refused creation
// names. The create webhook refuses a second Active Triage for a Workload with
// a conflict that carries the existing Triage's name; the name is confirmed
// before it is trusted.
func (h *Handler) activeTriage(
	ctx context.Context, createErr error, workload v1alpha1.WorkloadReference,
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
	key := client.ObjectKey{Namespace: h.namespace, Name: details.Name}
	if err := h.reader.Get(ctx, key, &existing); err != nil {
		return "", false
	}
	if existing.Spec.Workload != workload || !existing.Status.Phase.IsActive() {
		return "", false
	}
	return existing.Name, true
}
