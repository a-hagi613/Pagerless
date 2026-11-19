package proposal_test

import (
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/proposal"
)

// eligible returns a Page whose Proposal satisfies every unattended-draft rule.
func eligible() *v1alpha1.Page {
	return &v1alpha1.Page{
		Spec: v1alpha1.PageSpec{
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceEvent},
		},
		Status: v1alpha1.PageStatus{
			Repository: &v1alpha1.Resolution{
				Source: v1alpha1.ResolutionSourceArgoCDTrackingID, Confidence: v1alpha1.ConfidenceExact,
			},
			Proposal: &v1alpha1.Proposal{
				Kind: v1alpha1.ProposalKindFilePatch,
				Files: []v1alpha1.FileEdit{{
					Path: deploymentPath, BaseBlob: strings.Repeat("a", 40),
					ContentDigest: strings.Repeat("b", 64), Rationale: "the Deployment that crash-loops",
				}},
				Validation: &v1alpha1.ProposalValidation{Passed: true},
			},
		},
	}
}

func TestUnattendedDraftRequiresEveryRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		rule   proposal.DraftRule
		mutate func(*v1alpha1.Page)
		want   string
	}{
		{
			name: "eligible exact resolution",
			want: "",
		},
		{
			name: "human request",
			mutate: func(p *v1alpha1.Page) {
				p.Spec.Trigger.Source = v1alpha1.TriggerSourceCLI
			},
			want: "the Operator requested the Page",
		},
		{
			name: "no resolution",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Repository = nil
			},
			want: "not grounded",
		},
		{
			name: "ambiguous",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Repository.Ambiguous = true
			},
			want: "ambiguous",
		},
		{
			name: "identity resolution",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Repository.Confidence = v1alpha1.ConfidenceIdentity
			},
			want: "never opens an unattended draft",
		},
		{
			name: "configured without a rule",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Repository.Confidence = v1alpha1.ConfidenceConfigured
			},
			want: "allowAutoDraft",
		},
		{
			name: "configured with a rule",
			rule: proposal.DraftRule{AllowConfigured: true},
			mutate: func(p *v1alpha1.Page) {
				p.Status.Repository.Confidence = v1alpha1.ConfidenceConfigured
			},
			want: "",
		},
		{
			name: "diagnosis only",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Proposal.Kind = v1alpha1.ProposalKindDiagnosisOnly
			},
			want: "changes no files",
		},
		{
			name: "no validation report",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Proposal.Validation = nil
			},
			want: "no Validation report",
		},
		{
			name: "validation refused",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Proposal.Validation.Passed = false
			},
			want: "Validation refused",
		},
		{
			name: "validation degraded",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Proposal.Validation.Degraded = true
			},
			want: "degraded",
		},
		{
			name: "validation warned",
			mutate: func(p *v1alpha1.Page) {
				p.Status.Proposal.Validation.Warnings = []v1alpha1.ProposalWarning{{
					Class: v1alpha1.WarningImage, Field: "spec.template.spec.containers[0].image",
					Message: "the image changed",
				}}
			},
			want: "Image warning",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			page := eligible()
			if tc.mutate != nil {
				tc.mutate(page)
			}
			got, reason := proposal.UnattendedDraft(page, tc.rule)
			if tc.want == "" {
				if !got || reason != "" {
					t.Fatalf("UnattendedDraft = %v, %q; want eligible", got, reason)
				}
				return
			}
			if got {
				t.Fatalf("UnattendedDraft = true, want a refusal: %s", tc.want)
			}
			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason = %q, want it to contain %q", reason, tc.want)
			}
		})
	}
}

func TestUnattendedDraftIsNotChangedByModelUsage(t *testing.T) {
	t.Parallel()
	page := eligible()
	page.Status.Usage = &v1alpha1.Usage{InputTokens: 1_000, OutputTokens: 100, CostUSD: 0.02}
	if ok, reason := proposal.UnattendedDraft(page, proposal.DraftRule{}); !ok {
		t.Fatalf("UnattendedDraft = false, %q; model usage must not decide eligibility", reason)
	}
}

func TestUnattendedDraftDeniesANilPage(t *testing.T) {
	t.Parallel()
	if ok, _ := proposal.UnattendedDraft(nil, proposal.DraftRule{}); ok {
		t.Fatal("UnattendedDraft = true for a nil Page")
	}
}
