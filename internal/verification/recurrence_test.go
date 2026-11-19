package verification_test

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// hash returns a distinct proposal hash for a test.
func hash(seed int) string { return strings.Repeat(fmt.Sprintf("%x", seed%16), 64) }

// page returns a Page with the given proposal hash and exclusions.
func page(name, proposal string, excluded ...string) *v1alpha1.Page {
	return &v1alpha1.Page{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pagerless"},
		Spec:       v1alpha1.PageSpec{ExcludedProposalHashes: excluded},
		Status:     v1alpha1.PageStatus{Proposal: &v1alpha1.Proposal{Hash: proposal}},
	}
}

func TestRecurrenceLinksThePredecessorAndExcludesItsHash(t *testing.T) {
	t.Parallel()
	previous := page("checkout-1", hash(1))
	previousPage, excluded := verification.RecurrenceFor(previous, false)
	if previousPage != "checkout-1" {
		t.Errorf("previousPage = %q, want checkout-1", previousPage)
	}
	if len(excluded) != 1 || excluded[0] != hash(1) {
		t.Errorf("excluded = %v, want the predecessor's hash", excluded)
	}
}

func TestRecurrenceCarriesTheChainForward(t *testing.T) {
	t.Parallel()
	previous := page("checkout-2", hash(2), hash(1))
	_, excluded := verification.RecurrenceFor(previous, false)
	if len(excluded) != 2 || excluded[0] != hash(1) || excluded[1] != hash(2) {
		t.Fatalf("excluded = %v, want the chain of hashes", excluded)
	}
}

func TestRecurrenceBoundsTheChain(t *testing.T) {
	t.Parallel()
	var chain []string
	for seed := range 12 {
		chain = append(chain, hash(seed))
	}
	previous := page("checkout-3", hash(12), chain...)
	_, excluded := verification.RecurrenceFor(previous, false)
	if len(excluded) != 10 {
		t.Fatalf("excluded = %d hashes, want the bound of 10", len(excluded))
	}
	if excluded[len(excluded)-1] != hash(12) {
		t.Errorf("excluded = %v, want the predecessor's own hash last", excluded)
	}
}

func TestRecurrenceOnlyFollowsNotRecovered(t *testing.T) {
	t.Parallel()
	previousPage, excluded := verification.RecurrenceFor(page("checkout-4", hash(4)), true)
	if previousPage != "" || excluded != nil {
		t.Fatalf("previousPage = %q, excluded = %v, want no Recurrence after Recovered", previousPage, excluded)
	}
}
