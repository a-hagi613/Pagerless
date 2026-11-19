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

// RecurrenceFor returns the spec fields that link a new Page to the Page that
// recorded the previous outcome of its Fingerprint: its name, and the Proposal
// hashes that the new Page must not propose again.
//
// Every Recurrence links to its predecessor, whichever Phase it ended in. A
// predecessor whose Verification ended NotRecovered additionally excludes its
// own Proposal hash, so the change that did not work cannot be proposed
// forever, and the predecessor's exclusions are carried forward so that the
// chain accumulates them. A predecessor that Recovered contributes no exclusion
// of its own: its change did work, and a later Recurrence may need it again.
func RecurrenceFor(predecessor *v1alpha1.Page) (previousPage string, excluded []string) {
	if predecessor == nil {
		return "", nil
	}
	candidates := slices.Clone(predecessor.Spec.ExcludedProposalHashes)
	if predecessor.Status.Phase == v1alpha1.PhaseNotRecovered && predecessor.Status.Proposal != nil {
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
