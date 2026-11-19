package proposal_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/proposal"
)

// bodyInput returns a body input with the given number of Findings.
func bodyInput(findings int) proposal.BodyInput {
	input := proposal.BodyInput{
		PageName: checkoutName, PageNamespace: pageNamespace,
		Target: v1alpha1.TargetReference{
			Kind: v1alpha1.TargetKindDeployment, Namespace: "shop", Name: checkoutName,
		},
		Trigger: v1alpha1.Trigger{
			Source: v1alpha1.TriggerSourceEvent, Reason: "Unhealthy", Message: "Readiness probe failed",
		},
		Diagnosis: "The memory limit is below the working set.",
		Resolution: v1alpha1.Resolution{
			Source: v1alpha1.ResolutionSourceArgoCDTrackingID, Confidence: v1alpha1.ConfidenceExact,
			Path: "apps/checkout/base/deployment.yaml", EditTarget: overlayPath,
			EditTargetKind: v1alpha1.EditTargetOverlayPatch,
		},
		Files: []v1alpha1.FileEdit{{
			Path:          overlayPath,
			ContentDigest: strings.Repeat("a", 64),
			Rationale:     "the overlay that targets the Deployment",
		}},
		Validation: &v1alpha1.ProposalValidation{
			Passed: true, Admission: v1alpha1.AdmissionNotEvaluated,
			Warnings: []v1alpha1.ProposalWarning{{
				Class: v1alpha1.WarningImage, Field: "containers[app].image",
				Message: "the image changes from 1.0.0 to 1.1.0",
			}},
		},
		Warnings: []v1alpha1.ProposalWarning{{
			Class: v1alpha1.WarningImage, Field: "containers[app].image",
			Message: "the image changes from 1.0.0 to 1.1.0",
		}},
		ApprovedBy: "operator@example.com", ApprovedAt: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC),
	}
	for index := range findings {
		input.Findings = append(input.Findings, v1alpha1.Finding{
			Kind:    v1alpha1.FindingKindLog,
			Source:  fmt.Sprintf("tool logs pod=checkout-%d", index),
			Summary: fmt.Sprintf("line %d shows the allocation failure", index),
		})
	}
	return input
}

func TestPullRequestBodyHasItsSections(t *testing.T) {
	t.Parallel()
	body := proposal.PullRequestBody(bodyInput(2))
	for _, section := range []string{"## What failed", "## Root cause", "## Evidence", "## Change", "## Review notes"} {
		if !strings.Contains(body, section) {
			t.Errorf("the body is missing %q:\n%s", section, body)
		}
	}
	for _, fragment := range []string{
		"Deployment shop/checkout", "Unhealthy", "The memory limit is below the working set.",
		"ArgoCDTrackingID", "confidence **exact**", overlayPath,
		"the overlay that targets the Deployment", v1alpha1.AdmissionNotEvaluated,
		"**Image**", "Approved by operator@example.com",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("the body is missing %q", fragment)
		}
	}
}

func TestPullRequestBodyCitesAtMostTenFindings(t *testing.T) {
	t.Parallel()
	body := proposal.PullRequestBody(bodyInput(14))
	if strings.Contains(body, "line 10 shows") {
		t.Error("the body cites an eleventh Finding")
	}
	if !strings.Contains(body, "and 4 more on the Page") {
		t.Error("the body does not say how many Findings it left on the Page")
	}
}

func TestPullRequestBodyStripsControlCharacters(t *testing.T) {
	t.Parallel()
	input := bodyInput(1)
	input.Trigger.Message = "failed\x00 after \x1b[31mrestart\x1b[0m"
	body := proposal.PullRequestBody(input)
	if strings.ContainsAny(body, "\x00\x1b") {
		t.Errorf("the body carries a control character: %q", body)
	}
}

func TestPullRequestBodyIsBounded(t *testing.T) {
	t.Parallel()
	input := bodyInput(10)
	input.Diagnosis = strings.Repeat("the limit is too low. ", 1000)
	body := proposal.PullRequestBody(input)
	if len(body) > 8<<10 {
		t.Fatalf("the body is %d bytes, above the bound", len(body))
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("the body does not end with a line break")
	}
}
