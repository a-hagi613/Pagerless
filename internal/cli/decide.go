package cli

import (
	"context"
	"fmt"
	"os"

	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// loadProposedPage returns the named Page, which must be Proposed so that it has
// a Proposal for a decision to bind to.
func loadProposedPage(ctx context.Context, cl client.Client, name string) (*v1alpha1.Page, error) {
	var p v1alpha1.Page
	if err := cl.Get(ctx, client.ObjectKey{Namespace: PageNamespace, Name: name}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("page %q not found in namespace %q", name, PageNamespace)
		}
		return nil, fmt.Errorf("get Page %s: %w", name, err)
	}
	if p.Status.Phase != v1alpha1.PhaseProposed || p.Status.Proposal == nil {
		return nil, fmt.Errorf("page %s is in phase %s; only a Proposed Page accepts a decision",
			name, phaseOrUnset(p.Status.Phase))
	}
	return &p, nil
}

func phaseOrUnset(phase v1alpha1.Phase) string {
	if phase == "" {
		return "unset"
	}
	return string(phase)
}

// submitDecision records the decision as annotations on the Page. pagerless-server
// reconciles them into status.decision, which is the only place a decision is
// authoritative. The caller's authority is its patch permission on Pages
// (ADR-0031).
func submitDecision(
	ctx context.Context, cl client.Client, p *v1alpha1.Page,
	outcome v1alpha1.DecisionOutcome, reason, decidedBy string,
) error {
	patch := client.MergeFrom(p.DeepCopy())
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	if outcome == v1alpha1.DecisionRejected {
		p.Annotations[v1alpha1.AnnotationDecision] = v1alpha1.DecisionAnnotationReject
	} else {
		p.Annotations[v1alpha1.AnnotationDecision] = v1alpha1.DecisionAnnotationApprove
	}
	p.Annotations[v1alpha1.AnnotationDecisionProposal] = p.Status.Proposal.Hash
	p.Annotations[v1alpha1.AnnotationDecidedBy] = decidedBy
	if reason != "" {
		p.Annotations[v1alpha1.AnnotationDecisionReason] = reason
	}
	if err := cl.Patch(ctx, p, patch); err != nil {
		return fmt.Errorf("record the decision on Page %s: %w", p.Name, err)
	}
	return nil
}

// authenticatedUser returns the username that the kubeconfig authenticates as,
// which the decision records as its declared identity. A cluster that does not
// serve SelfSubjectReview, or a request that fails, falls back to the local user
// name so that a decision is never blocked by attribution.
func authenticatedUser(ctx context.Context, cl client.Client) string {
	var review authv1.SelfSubjectReview
	if err := cl.Create(ctx, &review); err == nil && review.Status.UserInfo.Username != "" {
		return review.Status.UserInfo.Username
	}
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return "unknown"
}
