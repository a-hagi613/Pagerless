package verification_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// hash returns a distinct proposal hash for a test.
func hash(seed int) string { return strings.Repeat(fmt.Sprintf("%x", seed%16), 64) }

// page returns a Page that ended in the phase with the given proposal hash and
// exclusions.
func page(name string, phase v1alpha1.Phase, proposal string, excluded ...string) *v1alpha1.Page {
	page := &v1alpha1.Page{
		Spec:   v1alpha1.PageSpec{ExcludedProposalHashes: excluded},
		Status: v1alpha1.PageStatus{Phase: phase, Proposal: &v1alpha1.Proposal{Hash: proposal}},
	}
	page.Name, page.Namespace = name, "pagerless"
	return page
}

func TestRecurrenceLinksThePredecessorAndExcludesAChangeThatDidNotWork(t *testing.T) {
	t.Parallel()
	previous := page("checkout-1", v1alpha1.PhaseNotRecovered, hash(1))
	previousPage, excluded := verification.RecurrenceFor(previous)
	if previousPage != "checkout-1" {
		t.Errorf("previousPage = %q, want checkout-1", previousPage)
	}
	if len(excluded) != 1 || excluded[0] != hash(1) {
		t.Errorf("excluded = %v, want the predecessor's hash", excluded)
	}
}

func TestRecurrenceLinksARecoveredPredecessorWithoutExcludingIt(t *testing.T) {
	t.Parallel()
	previous := page("checkout-1", v1alpha1.PhaseRecovered, hash(1))
	previousPage, excluded := verification.RecurrenceFor(previous)
	if previousPage != "checkout-1" {
		t.Errorf("previousPage = %q, want the link a Recurrence always has", previousPage)
	}
	if len(excluded) != 0 {
		t.Errorf("excluded = %v, want no exclusion after Recovered", excluded)
	}
}

func TestRecurrenceCarriesTheChainForward(t *testing.T) {
	t.Parallel()
	previous := page("checkout-2", v1alpha1.PhaseNotRecovered, hash(2), hash(1))
	_, excluded := verification.RecurrenceFor(previous)
	if len(excluded) != 2 || excluded[0] != hash(1) || excluded[1] != hash(2) {
		t.Fatalf("excluded = %v, want the chain of hashes", excluded)
	}
}

func TestRecurrenceBoundsTheChain(t *testing.T) {
	t.Parallel()
	chain := make([]string, 0, 12)
	for seed := range 12 {
		chain = append(chain, hash(seed))
	}
	previous := page("checkout-3", v1alpha1.PhaseNotRecovered, hash(12), chain...)
	_, excluded := verification.RecurrenceFor(previous)
	if len(excluded) != 10 {
		t.Fatalf("excluded = %d hashes, want the bound of 10", len(excluded))
	}
	if excluded[len(excluded)-1] != hash(12) {
		t.Errorf("excluded = %v, want the predecessor's own hash last", excluded)
	}
}

func TestRecurrenceWithoutAPredecessorIsEmpty(t *testing.T) {
	t.Parallel()
	if previousPage, excluded := verification.RecurrenceFor(nil); previousPage != "" || excluded != nil {
		t.Fatalf("previousPage = %q, excluded = %v, want no link", previousPage, excluded)
	}
}
