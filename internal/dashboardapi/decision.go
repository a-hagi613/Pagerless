package dashboardapi

import (
	"context"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Annotations that record how a dashboard decision reached the cluster. The
// decision itself is expressed by the decision annotations the Page controller
// reconciles; these extra annotations keep the Portal's audit trail complete.
const (
	// AnnotationDecisionSource names how the decision was submitted. It is dashboard for this path.
	AnnotationDecisionSource = "pagerless.dev/decision-source"
	// AnnotationApproverSubject is the identity provider's subject for the acting user.
	AnnotationApproverSubject = "pagerless.dev/approver-subject"
	// DecisionSourceDashboard is the AnnotationDecisionSource value of a dashboard decision.
	DecisionSourceDashboard = "dashboard"
)

// decisionRequest is the body of POST /api/pages/{name}/decision.
type decisionRequest struct {
	Decision v1alpha1.DecisionOutcome `json:"decision"`
	Reason   string                   `json:"reason"`
}

// decisionResponse is the answer to a recorded decision.
type decisionResponse struct {
	Name      string `json:"name"`
	Page      string `json:"page"`
	Decision  string `json:"decision"`
	DecidedBy string `json:"decidedBy"`
}

// decide records the signed-in user's decision as annotations on the Page. The
// Page controller reconciles them into status.decision, which keeps the Page
// status written by pagerless-server alone (ADR-0001, ADR-0031).
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
	var p v1alpha1.Page
	key := types.NamespacedName{Namespace: h.namespace, Name: r.PathValue("name")}
	if err := h.reader.Get(ctx, key, &p); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("Page %q not found", key.Name))
			return
		}
		h.internal(w, ctx, fmt.Errorf("read Page %s: %w", key.Name, err))
		return
	}
	if p.Status.Phase != v1alpha1.PhaseProposed || p.Status.Proposal == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"Page %s is in phase %s; only a Proposed Page accepts a decision", p.Name, phaseOrUnset(p.Status.Phase)))
		return
	}

	patch := client.MergeFrom(p.DeepCopy())
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	if request.Decision == v1alpha1.DecisionRejected {
		p.Annotations[v1alpha1.AnnotationDecision] = v1alpha1.DecisionAnnotationReject
	} else {
		p.Annotations[v1alpha1.AnnotationDecision] = v1alpha1.DecisionAnnotationApprove
	}
	p.Annotations[v1alpha1.AnnotationDecisionProposal] = p.Status.Proposal.Hash
	p.Annotations[v1alpha1.AnnotationDecidedBy] = user.Username
	p.Annotations[AnnotationDecisionSource] = DecisionSourceDashboard
	p.Annotations[AnnotationApproverSubject] = user.Subject
	if request.Reason != "" {
		p.Annotations[v1alpha1.AnnotationDecisionReason] = request.Reason
	}
	if err := h.writer.Patch(ctx, &p, patch); err != nil {
		h.writeWriteError(w, ctx, err)
		return
	}
	writeJSON(w, http.StatusCreated, decisionResponse{
		Name: p.Name, Page: p.Name, Decision: string(request.Decision), DecidedBy: user.Username,
	})
}

// writeWriteError answers a refused decision. A conflict means the Page moved
// under the operator, and a forbidden or invalid write is the operator's to fix.
func (h *Handler) writeWriteError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
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
