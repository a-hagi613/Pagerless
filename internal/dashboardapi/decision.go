package dashboardapi

import (
	"context"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Annotations that record how a dashboard decision reached the cluster. The
// approver itself is asserted in the spec and verified by the admission
// webhook; these annotations keep the audit trail complete.
const (
	// AnnotationDecisionSource names how the decision was submitted. It is dashboard for this path.
	AnnotationDecisionSource = "pagerless.dev/decision-source"
	// AnnotationApproverSubject is the Keycloak subject of the asserted approver.
	AnnotationApproverSubject = "pagerless.dev/approver-subject"
	// DecisionSourceDashboard is the AnnotationDecisionSource value of a dashboard decision.
	DecisionSourceDashboard = "dashboard"
)

// decisionRequest is the body of POST /api/triages/{name}/decision.
type decisionRequest struct {
	Decision v1alpha1.DecisionOutcome `json:"decision"`
	Reason   string                   `json:"reason"`
}

// decisionResponse is the answer to a recorded decision.
type decisionResponse struct {
	Name     string `json:"name"`
	Triage   string `json:"triage"`
	Decision string `json:"decision"`
	Approver string `json:"approver"`
}

// decide creates the TriageApproval for the Triage's current Proposal under
// pagerless-server's service account, asserting the signed-in user as the
// approver (ADR-0020). The admission webhook refuses the user unless the
// Keycloak group check confirms them.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	user := h.requireCSRF(w, r)
	if user == nil {
		return
	}
	var request decisionRequest
	if !decodeWrite(w, r, &request) {
		return
	}
	if !user.Approver {
		writeError(w, http.StatusForbidden, "only a member of the approvers group may decide a Proposal")
		return
	}
	switch request.Decision {
	case v1alpha1.DecisionApproved, v1alpha1.DecisionRejected:
	default:
		writeError(w, http.StatusBadRequest, "decision must be Approved or Rejected")
		return
	}
	if len(request.Reason) > v1alpha1.MaxReasonLength {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("the reason is %d characters long; at most %d are allowed",
				len(request.Reason), v1alpha1.MaxReasonLength))
		return
	}

	ctx := r.Context()
	var tr v1alpha1.Triage
	key := types.NamespacedName{Namespace: h.namespace, Name: r.PathValue("name")}
	if err := h.reader.Get(ctx, key, &tr); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("Triage %q not found", key.Name))
			return
		}
		h.internal(w, ctx, fmt.Errorf("read Triage %s: %w", key.Name, err))
		return
	}
	if tr.Status.Phase != v1alpha1.PhaseProposed || tr.Status.Proposal == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"Triage %s is in phase %s; only a Proposed Triage accepts a decision", tr.Name, phaseOrUnset(tr.Status.Phase)))
		return
	}

	approval := &v1alpha1.TriageApproval{
		Name:      v1alpha1.TriageApprovalName(&tr, tr.Status.Proposal.Hash),
		Namespace: h.namespace,
		Annotations: map[string]string{
			AnnotationDecisionSource:  DecisionSourceDashboard,
			AnnotationApproverSubject: user.Subject,
		},
		Spec: v1alpha1.TriageApprovalSpec{
			Triage:       tr.Name,
			ProposalHash: tr.Status.Proposal.Hash,
			Decision:     request.Decision,
			Reason:       request.Reason,
			Approver:     user.Username,
		},
	}
	if err := h.writer.Create(ctx, approval); err != nil {
		h.writeWriteError(w, ctx, err)
		return
	}
	writeJSON(w, http.StatusCreated, decisionResponse{
		Name: approval.Name, Triage: tr.Name, Decision: string(request.Decision), Approver: user.Username,
	})
}

// writeWriteError answers a refused create. Admission refusals are the
// approver's business: the message says what changed, such as a stale Proposal
// or an expired window.
func (h *Handler) writeWriteError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	case apierrors.IsAlreadyExists(err):
		writeError(w, http.StatusConflict, err.Error())
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsForbidden(err):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		logf.FromContext(ctx).Error(err, "dashboard decision failed")
		writeError(w, http.StatusInternalServerError, "the decision could not be recorded")
	}
}

// phaseOrUnset renders a Phase, naming the unset phase in words.
func phaseOrUnset(phase v1alpha1.Phase) string {
	if phase == "" {
		return "unset"
	}
	return string(phase)
}
