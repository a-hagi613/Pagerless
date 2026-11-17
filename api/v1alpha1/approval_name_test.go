package v1alpha1_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

func TestApprovalNamesBindTheFullTriageIdentityAndProposalWithinADNSLabel(t *testing.T) {
	t.Parallel()
	tr := &v1alpha1.Triage{
		Namespace: "pagerless", Name: strings.Repeat("x", 252) + "a", UID: types.UID("triage-uid-1"),
	}
	hash := strings.Repeat("a", 64)
	first := v1alpha1.TriageApprovalName(tr, hash)
	if problems := validation.IsDNS1123Label(first); len(problems) != 0 {
		t.Fatalf("Approval name is not a bounded DNS label: %v", problems)
	}
	if repeated := v1alpha1.TriageApprovalName(tr.DeepCopy(), hash); repeated != first {
		t.Fatal("the same Triage identity and exact Proposal hash produced different decision names")
	}
	names := map[string]bool{first: true}
	for _, mutate := range []func(*v1alpha1.Triage){
		func(other *v1alpha1.Triage) { other.Name = strings.Repeat("x", 252) + "b" },
		func(other *v1alpha1.Triage) { other.Namespace = "another" },
		func(other *v1alpha1.Triage) { other.UID = types.UID("triage-uid-2") },
	} {
		other := tr.DeepCopy()
		mutate(other)
		names[v1alpha1.TriageApprovalName(other, hash)] = true
	}
	names[v1alpha1.TriageApprovalName(tr, strings.Repeat("b", 64))] = true
	if len(names) != 5 {
		t.Fatal("distinct full names, namespaces, UIDs, or Proposal hashes share a decision name")
	}
}
