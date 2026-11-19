// Package page holds the helpers that every component of pagerless-server
// uses to read and write Page resources.
package page

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// ErrConflictingApprovals means older data already contains multiple Active
// Pages for one Fingerprint that each carry approved or delivered work.
// Choosing one would abandon a human-approved change, so duplicate resolution
// fails closed instead.
var ErrConflictingApprovals = errors.New("multiple Active Pages carry approved work for one Fingerprint")

// HasApprovedWork reports whether superseding the Page could abandon a
// human-approved change or a pull request that already exists.
func HasApprovedWork(p *v1alpha1.Page) bool {
	return p.Status.Phase == v1alpha1.PhaseApproved || p.Status.Phase == v1alpha1.PhasePullRequestOpen ||
		p.Status.Phase == v1alpha1.PhaseVerifying || p.Status.PullRequest != nil ||
		(p.Status.Decision != nil && p.Status.Decision.Outcome == v1alpha1.DecisionApproved)
}

// CanonicalActive reads the API directly through reader and selects the one
// canonical Active Page for fingerprint. Approved work wins over an unapproved
// duplicate; otherwise the oldest creation time wins, with name and UID as
// stable tie-breakers. The other Active Pages are returned as duplicates.
//
// The standalone chart installs no admission webhook, so this selection is the
// only place canonical uniqueness is enforced. It is deterministic and is
// repaired on every resync, which is why a duplicate Page that already exists
// is superseded rather than refused at creation (ADR-0031).
func CanonicalActive(
	ctx context.Context, reader client.Reader, namespace, fingerprint string,
) (*v1alpha1.Page, []v1alpha1.Page, error) {
	var all v1alpha1.PageList
	if err := reader.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("read Active Pages: %w", err)
	}
	active := make([]v1alpha1.Page, 0, len(all.Items))
	for _, p := range all.Items {
		if p.Spec.Fingerprint == fingerprint && p.Status.Phase.IsActive() && p.DeletionTimestamp.IsZero() {
			active = append(active, p)
		}
	}
	switch len(active) {
	case 0:
		return nil, nil, nil
	case 1:
		return &active[0], nil, nil
	}
	slices.SortFunc(active, func(a, b v1alpha1.Page) int {
		if order := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); order != 0 {
			return order
		}
		if order := strings.Compare(a.Name, b.Name); order != 0 {
			return order
		}
		return strings.Compare(string(a.UID), string(b.UID))
	})
	winner, approved := 0, 0
	for i := range active {
		if HasApprovedWork(&active[i]) {
			winner, approved = i, approved+1
		}
	}
	if approved > 1 {
		return nil, nil, fmt.Errorf("%w for Fingerprint %s", ErrConflictingApprovals, fingerprint)
	}
	duplicates := make([]v1alpha1.Page, 0, len(active)-1)
	for i := range active {
		if i != winner {
			duplicates = append(duplicates, active[i])
		}
	}
	return &active[winner], duplicates, nil
}
