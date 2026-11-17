package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/triage"
)

// Webhook paths for TriageApprovals.
const (
	// ApprovalStampPath stamps the requester's identity into a new TriageApproval.
	ApprovalStampPath = "/mutate-triageapproval"
	// ApprovalValidatePath binds a new TriageApproval to the current Proposal.
	ApprovalValidatePath = "/validate-triageapproval"
)

// ApprovalStamper sets spec.approver of a new TriageApproval. A request from
// pagerless-server's own service account may assert an approver: it is the
// dashboard, which vouches for the user it verified through Keycloak
// (ADR-0020). Every other requester has spec.approver set to the identity that
// the API server authenticated, and any value it supplied is overwritten, so a
// client cannot name another approver (ADR-0004).
type ApprovalStamper struct {
	// ServerUsername is the Kubernetes username of pagerless-server's service account.
	ServerUsername string
}

// Handle implements admission.Handler.
func (s ApprovalStamper) Handle(_ context.Context, req admission.Request) admission.Response {
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
	if req.UserInfo.Username == s.ServerUsername {
		if approval.Spec.Approver == "" {
			return admission.Denied("pagerless-server must assert the signed-in user as the approver")
		}
	} else {
		approval.Spec.Approver = req.UserInfo.Username
	}
	stamped, err := json.Marshal(&approval)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("encode TriageApproval: %w", err))
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, stamped)
}

// ApproverGroups reports whether a user belongs to the Keycloak approvers group.
type ApproverGroups interface {
	// IsApprover reports whether the named Keycloak user may approve. An error
	// means the answer is unknown, and the decision must be refused.
	IsApprover(ctx context.Context, username string) (bool, error)
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
// and no earlier decision may exist on that Proposal. It also polices the
// approver: a requester other than pagerless-server may only approve as
// itself, and pagerless-server may assert a user only when that user is a
// member of the Keycloak approvers group (ADR-0020).
//
// Every decision for the same Triage identity and exact hash must use one deterministic resource name. The
// API server's atomic name reservation therefore admits at most one decision even when admissions overlap.
type ApprovalValidator struct {
	// Reader reads Triages and TriageApprovals directly from the API server.
	Reader client.Reader
	// Namespace is the only namespace in which TriageApprovals are accepted.
	Namespace string
	// Proposals reports whether a Proposal is a Stale Proposal.
	Proposals StalenessChecker
	// Clock decides whether a Proposal has expired.
	Clock clock.PassiveClock
	// ServerUsername is the Kubernetes username of pagerless-server's service account, the only requester
	// that may assert an approver.
	ServerUsername string
	// Approvers answers whether an asserted user may approve. It is required for asserted approvers; when it
	// is nil, every asserted approver is refused.
	Approvers ApproverGroups

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
	if req.UserInfo.Username == "" {
		return admission.Denied("the request carries no authenticated identity to record as the approver")
	}
	if refusal := v.checkApprover(ctx, req.UserInfo.Username, approval.Spec.Approver); refusal != nil {
		return *refusal
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
	name := v1alpha1.TriageApprovalName(&tr, approval.Spec.ProposalHash)
	if approval.Name != name || approval.GenerateName != "" {
		return admission.Denied(fmt.Sprintf(
			"a decision must use deterministic metadata.name %q with no metadata.generateName", name))
	}
	var existing v1alpha1.TriageApproval
	if err := v.Reader.Get(ctx, types.NamespacedName{Namespace: v.Namespace, Name: name}, &existing); err == nil {
		response := admission.Denied(fmt.Sprintf("TriageApproval %s already exists for Triage %s in phase %s",
			name, tr.Name, phaseName(&tr)))
		response.Result.Reason = metav1.StatusReasonAlreadyExists
		response.Result.Code = http.StatusConflict
		response.Result.Details = &metav1.StatusDetails{
			Group: v1alpha1.GroupVersion.Group, Kind: "triageapprovals", Name: name,
		}
		return response
	} else if !apierrors.IsNotFound(err) {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("get TriageApproval: %w", err))
	}
	if refusal := v.checkProposal(ctx, &tr, &approval); refusal != nil {
		return *refusal
	}
	winner, _, err := triage.CanonicalActive(ctx, v.Reader, tr.Namespace, tr.Spec.Workload)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if winner == nil || winner.UID != tr.UID {
		return admission.Denied("only the canonical Active Triage for the Workload accepts a decision")
	}
	return admission.Allowed("")
}

// checkApprover returns a refusal unless the approver is acceptable for the
// requester. A requester other than pagerless-server may only approve as
// itself: the stamping webhook rewrites any other value, and this check holds
// if the stamping webhook is bypassed. pagerless-server may assert another
// user, but only one that Keycloak reports as a member of the approvers group.
func (v *ApprovalValidator) checkApprover(ctx context.Context, requester, approver string) *admission.Response {
	deny := func(format string, args ...any) *admission.Response {
		resp := admission.Denied(fmt.Sprintf(format, args...))
		return &resp
	}
	if approver == "" {
		return deny("spec.approver is empty; the approver is set by the server")
	}
	if requester != v.ServerUsername {
		if approver != requester {
			return deny("spec.approver is %q but the request comes from %q; only pagerless-server may assert an approver",
				approver, requester)
		}
		return nil
	}
	if approver == v.ServerUsername {
		return deny("pagerless-server must assert the signed-in user as the approver, not itself")
	}
	if v.Approvers == nil {
		resp := admission.Errored(http.StatusInternalServerError,
			errors.New("no Keycloak approver-group check is configured"))
		return &resp
	}
	ok, err := v.Approvers.IsApprover(ctx, approver)
	if err != nil {
		resp := admission.Errored(http.StatusInternalServerError,
			fmt.Errorf("check whether %q may approve: %w", approver, err))
		return &resp
	}
	if !ok {
		return deny("the asserted approver %q is not a member of the Keycloak group that may approve", approver)
	}
	return nil
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
