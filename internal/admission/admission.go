// Package admission holds the validating webhooks of pagerless-server for
// Triages: one Active Triage per Workload, and a single writer of status.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Webhook paths served by pagerless-server.
const (
	// CreatePath validates the creation of a Triage.
	CreatePath = "/validate-triage-create"
	// StatusPath validates updates to a Triage's status.
	StatusPath = "/validate-triage-status"
)

// CreateValidator refuses a Triage that is not in the server's namespace or
// that targets a Workload which already has an Active Triage. The refusal
// names the existing Triage.
//
// Admissions are serialized and read the API server directly, so two Triages
// created one after the other for the same Workload cannot both pass. Two
// created within the same instant, before the first is stored, can; that
// window is the price of not holding a lock across the API server write.
type CreateValidator struct {
	// Reader reads Triages directly from the API server.
	Reader client.Reader
	// Namespace is the only namespace in which Triages are accepted.
	Namespace string

	mu sync.Mutex
}

// Handle implements admission.Handler.
func (v *CreateValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("only creation is validated here")
	}
	var candidate v1alpha1.Triage
	if err := json.Unmarshal(req.Object.Raw, &candidate); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if req.Namespace != v.Namespace {
		return admission.Denied(fmt.Sprintf("Triages must be created in namespace %q, not %q", v.Namespace, req.Namespace))
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	var existing v1alpha1.TriageList
	if err := v.Reader.List(ctx, &existing, client.InNamespace(v.Namespace)); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("list Triages: %w", err))
	}
	for i := range existing.Items {
		other := &existing.Items[i]
		if other.Spec.Workload == candidate.Spec.Workload && other.Status.Phase.IsActive() {
			return denyDuplicate(other, candidate.Spec.Workload)
		}
	}
	return admission.Allowed("")
}

// denyDuplicate refuses the request with a Conflict status that carries the
// existing Triage's name, in the message and in the status details.
func denyDuplicate(existing *v1alpha1.Triage, workload v1alpha1.WorkloadReference) admission.Response {
	resp := admission.Denied(fmt.Sprintf("%s %s/%s already has the Active Triage %q",
		workload.Kind, workload.Namespace, workload.Name, existing.Name))
	resp.Result.Reason = metav1.StatusReasonAlreadyExists
	resp.Result.Code = http.StatusConflict
	resp.Result.Details = &metav1.StatusDetails{
		Group: v1alpha1.GroupVersion.Group,
		Kind:  "Triage",
		Name:  existing.Name,
	}
	return resp
}

// StatusGuard refuses every write to a Triage's status that does not come
// from pagerless-server's own identity (ADR-0001). RBAC already limits who may
// update the status subresource; the guard also stops cluster administrators
// from editing a Proposal after Validation.
type StatusGuard struct {
	// ServerUsername is the Kubernetes username of pagerless-server's service account.
	ServerUsername string
}

// Handle implements admission.Handler.
func (g *StatusGuard) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.UserInfo.Username == g.ServerUsername {
		return admission.Allowed("")
	}
	return admission.Denied(fmt.Sprintf("only pagerless-server writes Triage status; %q may not", req.UserInfo.Username))
}
