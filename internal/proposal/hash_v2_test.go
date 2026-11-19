package proposal_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/proposal"
)

// v2Page returns a Page with a v2 FilePatch Proposal and a Resolution.
func v2Page() *v1alpha1.Page {
	page := &v1alpha1.Page{
		Spec: v1alpha1.PageSpec{
			Target: v1alpha1.TargetReference{
				APIVersion: "apps/v1", Kind: v1alpha1.TargetKindDeployment,
				Namespace: "shop", Name: checkoutName,
			},
		},
		Status: v1alpha1.PageStatus{
			Diagnosis: "The memory limit is too low.",
			Repository: &v1alpha1.Resolution{
				URL: "https://github.com/acme/gitops", Branch: "main",
				Path: deploymentPath, EditTarget: deploymentPath,
				EditTargetKind: v1alpha1.EditTargetBaseManifest,
				Source:         v1alpha1.ResolutionSourceArgoCDTrackingID, Confidence: v1alpha1.ConfidenceExact,
				ResolvedAt: &metav1.Time{},
				Attempts: []v1alpha1.ResolutionAttempt{{
					Source:  v1alpha1.ResolutionSourceHint,
					Outcome: v1alpha1.ResolutionAttemptSkipped,
				}},
			},
			Proposal: &v1alpha1.Proposal{
				Kind: v1alpha1.ProposalKindFilePatch,
				Base: &v1alpha1.ProposalBase{Commit: "base-1", TargetGeneration: 7},
				Files: []v1alpha1.FileEdit{{
					Path: deploymentPath, BaseBlob: "blob-1",
					ContentDigest: "digest-1", Rationale: "the Deployment's manifest",
				}},
			},
		},
	}
	page.Namespace, page.Name, page.UID = pageNamespace, checkoutName, types.UID("uid-1")
	return page
}

// hashOf returns the Page's v2 hash.
func hashOf(t *testing.T, page *v1alpha1.Page) string {
	t.Helper()
	hash, err := proposal.ProposalHash(page)
	if err != nil {
		t.Fatalf("ProposalHash: %v", err)
	}
	if len(hash) != 64 {
		t.Fatalf("hash = %q, want a SHA-256 digest", hash)
	}
	return hash
}

func TestProposalHashIsStable(t *testing.T) {
	t.Parallel()
	first, second := hashOf(t, v2Page()), hashOf(t, v2Page())
	if first != second {
		t.Fatalf("equal Proposals hash differently: %s and %s", first, second)
	}
}

func TestProposalHashChangesWithItsInputs(t *testing.T) {
	t.Parallel()
	base := hashOf(t, v2Page())
	cases := map[string]func(*v1alpha1.Page){
		"base commit":  func(p *v1alpha1.Page) { p.Status.Proposal.Base.Commit = "base-2" },
		"file content": func(p *v1alpha1.Page) { p.Status.Proposal.Files[0].ContentDigest = "digest-2" },
		"resolution": func(p *v1alpha1.Page) {
			p.Status.Repository.EditTarget = patchPath
		},
		"target generation": func(p *v1alpha1.Page) { p.Status.Proposal.Base.TargetGeneration = 8 },
		"diagnosis":         func(p *v1alpha1.Page) { p.Status.Diagnosis = "Something else." },
		"proposal kind":     func(p *v1alpha1.Page) { p.Status.Proposal.Kind = v1alpha1.ProposalKindDiagnosisOnly },
		"target identity":   func(p *v1alpha1.Page) { p.Spec.Target.Name = "payments" },
		"rationale":         func(p *v1alpha1.Page) { p.Status.Proposal.Files[0].Rationale = "another file" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			page := v2Page()
			mutate(page)
			if got := hashOf(t, page); got == base {
				t.Errorf("the hash did not change when %s changed", name)
			}
		})
	}
}

func TestProposalHashIgnoresUsageAndResolutionTime(t *testing.T) {
	t.Parallel()
	first := v2Page()
	second := v2Page()
	second.Status.Usage = &v1alpha1.Usage{InputTokens: 120000, OutputTokens: 4000, CostUSD: 0.42}
	second.Status.Repository.ResolvedAt = &metav1.Time{Time: metav1.Now().Time}
	if a, b := hashOf(t, first), hashOf(t, second); a != b {
		t.Fatalf("usage or the resolution time changed the hash: %s and %s", a, b)
	}
}

func TestProposalHashNeedsAProposalAndAResolution(t *testing.T) {
	t.Parallel()
	page := v2Page()
	page.Status.Proposal = nil
	if _, err := proposal.ProposalHash(page); err == nil {
		t.Error("a Page without a Proposal hashed")
	}
	page = v2Page()
	page.Status.Repository = nil
	if _, err := proposal.ProposalHash(page); err == nil {
		t.Error("a Page without a Resolution hashed")
	}
}
