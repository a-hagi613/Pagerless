package proposal_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/proposal"
)

const patchedPath = "a.yaml"

func triage() *v1alpha1.Triage {
	return &v1alpha1.Triage{
		Namespace: "pagerless", Name: "triage-1", UID: "uid-1",
		Spec: v1alpha1.TriageSpec{Workload: v1alpha1.WorkloadReference{
			Kind: v1alpha1.WorkloadKindDeployment, Namespace: "sandbox", Name: "checkout",
		}},
	}
}

func patch(memory string) v1alpha1.ProposedAction {
	return v1alpha1.ProposedAction{
		Action: v1alpha1.ActionPatchResources,
		Parameters: &v1alpha1.ActionParameters{PatchResources: &v1alpha1.PatchResourcesParameters{
			Container: "app",
			Limits:    corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)},
		}},
	}
}

func mustHash(t *testing.T, tr *v1alpha1.Triage, a v1alpha1.ProposedAction, c *v1alpha1.RenderedChange) string {
	t.Helper()
	h, err := proposal.Hash(tr, a, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHashCoversEveryInputTheApprovalBindsTo(t *testing.T) {
	base := mustHash(t, triage(), patch("512Mi"), &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p1"})

	otherUID := triage()
	otherUID.UID = "uid-2"
	otherName := triage()
	otherName.Name = "triage-2"
	otherWorkload := triage()
	otherWorkload.Spec.Workload.Name = "cart"

	same := &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p1"}
	noAction := v1alpha1.ProposedAction{Action: v1alpha1.ActionNoAction}
	changed := map[string]string{
		"triage UID":         mustHash(t, otherUID, patch("512Mi"), same),
		"triage name":        mustHash(t, otherName, patch("512Mi"), same),
		"workload":           mustHash(t, otherWorkload, patch("512Mi"), same),
		"parameters":         mustHash(t, triage(), patch("1Gi"), same),
		"action":             mustHash(t, triage(), noAction, same),
		"rendered patch":     mustHash(t, triage(), patch("512Mi"), &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p2"}),
		"rendered path":      mustHash(t, triage(), patch("512Mi"), &v1alpha1.RenderedChange{Path: "b.yaml", Patch: "p1"}),
		"no rendered change": mustHash(t, triage(), patch("512Mi"), nil),
	}
	for input, hash := range changed {
		if hash == base {
			t.Errorf("changing the %s did not change the hash", input)
		}
	}
}

func TestHashCoversTheBaseTheChangeWasRenderedAgainst(t *testing.T) {
	change := &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p1"}
	hashWith := func(base *v1alpha1.ProposalBase) string {
		h, err := proposal.Hash(triage(), patch("512Mi"), change, base)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hashWith(&v1alpha1.ProposalBase{Commit: "c1", WorkloadGeneration: 3})
	for name, other := range map[string]*v1alpha1.ProposalBase{
		"base commit":         {Commit: "c2", WorkloadGeneration: 3},
		"workload generation": {Commit: "c1", WorkloadGeneration: 4},
		"no base":             nil,
	} {
		if hashWith(other) == base {
			t.Errorf("changing the %s did not change the hash", name)
		}
	}
}

func TestHashIsStableAndIndependentOfQuantityNotation(t *testing.T) {
	change := &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p1"}
	a := mustHash(t, triage(), patch("1Gi"), change)
	b := mustHash(t, triage(), patch("1024Mi"), change)
	if a != b {
		t.Errorf("1Gi and 1024Mi hash differently: %s and %s", a, b)
	}
	if again := mustHash(t, triage(), patch("1Gi"), change); again != a {
		t.Errorf("the hash is not deterministic: %s and %s", a, again)
	}
	if len(a) != 64 {
		t.Errorf("hash %q is not a SHA-256 digest in hexadecimal", a)
	}
}
