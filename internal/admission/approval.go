package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Webhook paths for TriageApprovals.
const (
	// ApprovalStampPath stamps the requester's identity into a new TriageApproval.
	ApprovalStampPath = "/mutate-triageapproval"
	// ApprovalValidatePath binds a new TriageApproval to the current Proposal.
	ApprovalValidatePath = "/validate-triageapproval"
)

// ApprovalStamper sets spec.approver of a new TriageApproval to the identity
// that the API server authenticated for the request, and overwrites any value
// the client supplied. A client therefore cannot name another approver
// (ADR-0004). Impersonation is honored: a dashboard that impersonates a user
// authenticated through Keycloak stamps that user.
type ApprovalStamper struct{}

// Handle implements admission.Handler.
func (ApprovalStamper) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("only creation is stamped")
	}
	var approval v1alpha1.TriageApproval
	if err := json.Unmarshal(req.Object.Raw, &approval); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if req.UserInfo.Username == "" {
		return admission.Denied("the request carries no authenticated identity to record as the approver")
	}
	approval.Spec.Approver = req.UserInfo.Username
	stamped, err := json.Marshal(&approval)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("encode TriageApproval: %w", err))
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, stamped)
}

// StalenessChecker reports whether a Triage's Proposal is a Stale Proposal.
type StalenessChecker interface {
	// Stale returns the reason the Triage's current Proposal is a Stale
	// Proposal, or an empty string when its base still holds.
	Stale(ctx context.Context, t *v1alpha1.Triage) (string, error)
}

// ApprovalValidator refuses a TriageApproval that does not bind to the
// Triage's current Proposal: its Triage must exist and be Proposed, its hash
// must be the current Proposal's, the Proposal must not be Stale or expired,
// and no earlier decision may exist on that Proposal. It also refuses an
// approver that is not the requester, so a missing or bypassed stamping
// webhook cannot let a forged identity through.
//
// Admissions are serialized and read the API server directly, so two decisions
// created one after the other cannot both pass. Two created within the same
// instant, before the first is stored, can; the controller then applies
// whichever it processes first and ignores the other.
type ApprovalValidator struct {
	// Reader reads Triages and TriageApprovals directly from the API server.
	Reader client.Reader
	// Namespace is the only namespace in which TriageApprovals are accepted.
	Namespace string
	// Proposals reports whether a Proposal is a Stale Proposal.
	Proposals StalenessChecker
	// Clock decides whether a Proposal has expired.
	Clock clock.PassiveClock

	mu sync.Mutex
}

// Handle implements admission.Handler.
func (v *ApprovalValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("only creation is validated here")
	}
	var approval v1alpha1.TriageApproval
	if err := json.Unmarshal(req.Object.Raw, &approval); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if req.Namespace != v.Namespace {
		return admission.Denied(fmt.Sprintf("TriageApprovals must be created in namespace %q, not %q",
			v.Namespace, req.Namespace))
	}
	if approval.Spec.Approver != req.UserInfo.Username || req.UserInfo.Username == "" {
		return admission.Denied(fmt.Sprintf(
			"spec.approver is %q but the request comes from %q; the approver is set by the server",
			approval.Spec.Approver, req.UserInfo.Username))
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	var tr v1alpha1.Triage
	key := types.NamespacedName{Namespace: v.Namespace, Name: approval.Spec.Triage}
	if err := v.Reader.Get(ctx, key, &tr); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Denied(fmt.Sprintf("Triage %q not found in namespace %q", approval.Spec.Triage, v.Namespace))
		}
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("get Triage: %w", err))
	}
	if refusal := v.checkProposal(ctx, &tr, &approval); refusal != nil {
		return *refusal
	}
	return admission.Allowed("")
}

// checkProposal returns a refusal unless the approval binds to the Triage's current Proposal.
func (v *ApprovalValidator) checkProposal(
	ctx context.Context, tr *v1alpha1.Triage, approval *v1alpha1.TriageApproval,
) *admission.Response {
	deny := func(format string, args ...any) *admission.Response {
		resp := admission.Denied(fmt.Sprintf(format, args...))
		return &resp
	}
	current := tr.Status.Proposal
	if tr.Status.Phase != v1alpha1.PhaseProposed || current == nil {
		return deny("Triage %s is in phase %s; only a Proposed Triage accepts a decision", tr.Name, phaseName(tr))
	}
	if approval.Spec.ProposalHash != current.Hash {
		return deny("proposal hash %s does not match the current Proposal's hash %s; "+
			"review the current Proposal and decide again", approval.Spec.ProposalHash, current.Hash)
	}
	if !v.Clock.Now().Before(current.ExpiresAt.Time) {
		return deny("the Proposal expired at %s; start a new Triage", current.ExpiresAt.UTC().Format(time.RFC3339))
	}
	reason, err := v.Proposals.Stale(ctx, tr)
	if err != nil {
		resp := admission.Errored(http.StatusInternalServerError, fmt.Errorf("check the Proposal for staleness: %w", err))
		return &resp
	}
	if reason != "" {
		return deny("the Proposal is stale (%s); it will be re-validated under a new hash, review it again then", reason)
	}

	var decisions v1alpha1.TriageApprovalList
	if err := v.Reader.List(ctx, &decisions, client.InNamespace(v.Namespace)); err != nil {
		resp := admission.Errored(http.StatusInternalServerError, fmt.Errorf("list TriageApprovals: %w", err))
		return &resp
	}
	for i := range decisions.Items {
		earlier := &decisions.Items[i]
		if earlier.Spec.Triage == tr.Name && earlier.Spec.ProposalHash == current.Hash {
			return deny("TriageApproval %s by %s already decides this Proposal", earlier.Name, earlier.Spec.Approver)
		}
	}
	return nil
}

func phaseName(tr *v1alpha1.Triage) string {
	if tr.Status.Phase == "" {
		return "unset"
	}
	return string(tr.Status.Phase)
}
