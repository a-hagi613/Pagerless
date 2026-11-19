package v1alpha1_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// maxStatusBytes is the bound that Page status must stay under: no secret, no
// raw tool output, and no model output beyond the Diagnosis, Findings, and
// Narrative.
const maxStatusBytes = 100 << 10

// TestMaximalPageStatusStaysUnderTheStatusBudget fills every bounded field with
// its largest permitted value, and asserts that the encoded status stays inside
// the budget.
func TestMaximalPageStatusStaysUnderTheStatusBudget(t *testing.T) {
	t.Parallel()
	text := func(size int) string { return strings.Repeat("a", size) }
	now := metav1.NewTime(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	status := v1alpha1.PageStatus{
		Phase:              v1alpha1.PhaseNotRecovered,
		Severity:           v1alpha1.SeverityCritical,
		Diagnosis:          text(4000),
		Narrative:          text(4000),
		Steps:              make([]v1alpha1.Step, v1alpha1.MaxSteps),
		Findings:           make([]v1alpha1.Finding, v1alpha1.MaxFindings),
		VerificationChecks: make([]v1alpha1.VerificationCheck, 20),
		Repository: &v1alpha1.Resolution{
			URL: text(500), Branch: text(253), Path: text(500), EditTarget: text(500),
			Source: v1alpha1.ResolutionSourceArgoCDTrackingID, Confidence: v1alpha1.ConfidenceExact,
			BaseCommit: text(64), Reason: text(300), ResolvedAt: &now,
			Candidates: make([]v1alpha1.ResolutionCandidate, 5),
			Attempts:   make([]v1alpha1.ResolutionAttempt, 9),
		},
		Proposal: &v1alpha1.Proposal{
			Kind: v1alpha1.ProposalKindFilePatch,
			Base: &v1alpha1.ProposalBase{Commit: text(64), TargetGeneration: 1},
			Hash: text(64), Attempt: 2, ExpiresAt: now,
			Files: make([]v1alpha1.FileEdit, v1alpha1.MaxProposalFiles),
			Validation: &v1alpha1.ProposalValidation{
				Passed: true, Degraded: true, Admission: v1alpha1.AdmissionNotEvaluated,
				Errors:   make([]v1alpha1.ProposalValidationError, 10),
				Warnings: make([]v1alpha1.ProposalWarning, 10),
			},
		},
	}
	for index := range status.Steps {
		status.Steps[index] = v1alpha1.Step{Time: now, Kind: v1alpha1.StepKindToolCall, Tool: text(64), Summary: text(200)}
	}
	for index := range status.Findings {
		status.Findings[index] = v1alpha1.Finding{
			Kind: v1alpha1.FindingKindLog, Source: text(300), Summary: text(500),
		}
	}
	for index := range status.VerificationChecks {
		status.VerificationChecks[index] = v1alpha1.VerificationCheck{Name: text(64), Passed: true, Message: text(300)}
	}
	for index := range status.Repository.Candidates {
		status.Repository.Candidates[index] = v1alpha1.ResolutionCandidate{
			Path: text(500), EditTarget: text(500), EditTargetKind: v1alpha1.EditTargetOverlayPatch,
		}
	}
	for index := range status.Repository.Attempts {
		status.Repository.Attempts[index] = v1alpha1.ResolutionAttempt{
			Source: v1alpha1.ResolutionSourceIdentitySearch, Outcome: v1alpha1.ResolutionAttemptFailed, Reason: text(200),
		}
	}
	for index := range status.Proposal.Files {
		status.Proposal.Files[index] = v1alpha1.FileEdit{
			Path: text(500), BaseBlob: text(64), ContentDigest: text(64), Rationale: text(300),
		}
	}
	for index := range status.Proposal.Validation.Errors {
		status.Proposal.Validation.Errors[index] = v1alpha1.ProposalValidationError{Field: text(200), Message: text(300)}
	}
	for index := range status.Proposal.Validation.Warnings {
		status.Proposal.Validation.Warnings[index] = v1alpha1.ProposalWarning{
			Class: v1alpha1.WarningImage, Field: text(200), Message: text(300),
		}
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxStatusBytes {
		t.Fatalf("the maximal Page status is %d bytes, above the %d-byte budget", len(encoded), maxStatusBytes)
	}
}
