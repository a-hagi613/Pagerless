package proposal_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/proposal"
)

const patchedPath = "a.yaml"

func page() *v1alpha1.Page {
	return &v1alpha1.Page{
		Namespace: "pagerless", Name: "page-1", UID: "uid-1",
		Spec: v1alpha1.PageSpec{Target: v1alpha1.TargetReference{
			Kind: v1alpha1.TargetKindDeployment, Namespace: "sandbox", Name: "checkout",
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

func mustHash(t *testing.T, tr *v1alpha1.Page, a v1alpha1.ProposedAction, c *v1alpha1.RenderedChange) string {
	t.Helper()
	h, err := proposal.Hash(tr, a, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHashCoversEveryInputTheApprovalBindsTo(t *testing.T) {
	base := mustHash(t, page(), patch("512Mi"), &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p1"})

	otherUID := page()
	otherUID.UID = "uid-2"
	otherName := page()
	otherName.Name = "page-2"
	otherTarget := page()
	otherTarget.Spec.Target.Name = "cart"

	same := &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p1"}
	noAction := v1alpha1.ProposedAction{Action: v1alpha1.ActionNoAction}
	changed := map[string]string{
		"page UID":           mustHash(t, otherUID, patch("512Mi"), same),
		"page name":          mustHash(t, otherName, patch("512Mi"), same),
		"target":             mustHash(t, otherTarget, patch("512Mi"), same),
		"parameters":         mustHash(t, page(), patch("1Gi"), same),
		"action":             mustHash(t, page(), noAction, same),
		"rendered patch":     mustHash(t, page(), patch("512Mi"), &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p2"}),
		"rendered path":      mustHash(t, page(), patch("512Mi"), &v1alpha1.RenderedChange{Path: "b.yaml", Patch: "p1"}),
		"no rendered change": mustHash(t, page(), patch("512Mi"), nil),
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
		h, err := proposal.Hash(page(), patch("512Mi"), change, base)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hashWith(&v1alpha1.ProposalBase{Commit: "c1", TargetGeneration: 3})
	for name, other := range map[string]*v1alpha1.ProposalBase{
		"base commit":       {Commit: "c2", TargetGeneration: 3},
		"target generation": {Commit: "c1", TargetGeneration: 4},
		"no base":           nil,
	} {
		if hashWith(other) == base {
			t.Errorf("changing the %s did not change the hash", name)
		}
	}
}

func TestHashIsStableAndIndependentOfQuantityNotation(t *testing.T) {
	change := &v1alpha1.RenderedChange{Path: patchedPath, Patch: "p1"}
	a := mustHash(t, page(), patch("1Gi"), change)
	b := mustHash(t, page(), patch("1024Mi"), change)
	if a != b {
		t.Errorf("1Gi and 1024Mi hash differently: %s and %s", a, b)
	}
	if again := mustHash(t, page(), patch("1Gi"), change); again != a {
		t.Errorf("the hash is not deterministic: %s and %s", a, again)
	}
	if len(a) != 64 {
		t.Errorf("hash %q is not a SHA-256 digest in hexadecimal", a)
	}
}
