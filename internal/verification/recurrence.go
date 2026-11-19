package verification

import (
	"regexp"
	"slices"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// maxExcludedProposalHashes matches the bound on
// PageSpec.ExcludedProposalHashes.
const maxExcludedProposalHashes = 10

// proposalHash matches a lowercase hexadecimal SHA-256 digest.
var proposalHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RecurrenceFor returns the spec fields that link a new Page to the Page whose
// Verification ended NotRecovered: its name, and the Proposal hashes that the
// new Page must not propose again. The predecessor's own hash is excluded so
// that the same change cannot be proposed forever, and the predecessor's
// exclusions are carried forward so that the chain accumulates them.
//
// Any other outcome returns no link, because only a NotRecovered Verification
// opens a Recurrence.
func RecurrenceFor(predecessor *v1alpha1.Page, recovered bool) (previousPage string, excluded []string) {
	if predecessor == nil || recovered {
		return "", nil
	}
	candidates := slices.Clone(predecessor.Spec.ExcludedProposalHashes)
	if predecessor.Status.Proposal != nil {
		candidates = append(candidates, predecessor.Status.Proposal.Hash)
	}
	seen := make(map[string]bool, len(candidates))
	for _, hash := range candidates {
		if !proposalHash.MatchString(hash) || seen[hash] {
			continue
		}
		seen[hash] = true
		excluded = append(excluded, hash)
	}
	if len(excluded) > maxExcludedProposalHashes {
		excluded = excluded[len(excluded)-maxExcludedProposalHashes:]
	}
	return predecessor.Name, excluded
}
