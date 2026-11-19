package proposal

import (
	"fmt"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const (
	// maxBodyFindings is the number of Findings that the Pull Request body cites.
	maxBodyFindings = 10
	// maxBodyBytes bounds the body, so that a Proposal cannot make the Pull
	// Request unreadable.
	maxBodyBytes = 8 << 10
)

// BodyInput is everything the Pull Request body needs. It deliberately contains
// no tool output and no logs: a Finding's bounded summary is the only evidence
// text that reaches the body.
type BodyInput struct {
	// PageName and PageNamespace identify the Page.
	PageName, PageNamespace string
	// Target is the failing object.
	Target v1alpha1.TargetReference
	// Trigger is the observation that opened the Page.
	Trigger v1alpha1.Trigger
	// Diagnosis is the agent's root-cause conclusion.
	Diagnosis string
	// Findings are the cited evidence.
	Findings []v1alpha1.Finding
	// Resolution is the provenance ladder's result.
	Resolution v1alpha1.Resolution
	// Files are the approved whole-file changes.
	Files []v1alpha1.FileEdit
	// Warnings are the permitted changes that the Operator must review.
	Warnings []v1alpha1.ProposalWarning
	// Validation is the deterministic Validation report.
	Validation *v1alpha1.ProposalValidation
	// ApprovedBy and ApprovedAt record the Approval.
	ApprovedBy string
	ApprovedAt time.Time
}

// PullRequestBody renders the Pull Request body with the five sections that
// explain the change: What failed, Root cause, Evidence, Change, and Review
// notes.
func PullRequestBody(input BodyInput) string {
	var b strings.Builder
	target := fmt.Sprintf("%s %s", input.Target.Kind, qualified(input.Target.Namespace, input.Target.Name))
	fmt.Fprintf(&b, "## What failed\n\n%s is failing on Page `%s/%s`.", target, input.PageNamespace, input.PageName)
	if input.Trigger.Reason != "" {
		fmt.Fprintf(&b, " The Trigger was `%s`", input.Trigger.Reason)
		if input.Trigger.Message != "" {
			fmt.Fprintf(&b, ": %s", sanitize(input.Trigger.Message, 500))
		}
		b.WriteString(".")
	}
	b.WriteString("\n\n")

	b.WriteString("## Root cause\n\n")
	if input.Diagnosis == "" {
		b.WriteString("The Diagnosis is recorded on the Page.\n\n")
	} else {
		fmt.Fprintf(&b, "%s\n\n", sanitize(input.Diagnosis, 4000))
	}

	b.WriteString("## Evidence\n\n")
	if len(input.Findings) == 0 {
		b.WriteString("No Finding was recorded.\n\n")
	}
	for index, finding := range input.Findings {
		if index == maxBodyFindings {
			fmt.Fprintf(&b, "- and %d more on the Page\n", len(input.Findings)-maxBodyFindings)
			break
		}
		fmt.Fprintf(&b, "%d. **%s** — %s: %s\n", index+1, finding.Kind,
			sanitize(finding.Source, 500), sanitize(finding.Summary, 1000))
	}
	if len(input.Findings) > 0 {
		b.WriteString("\n")
	}

	b.WriteString("## Change\n\n")
	fmt.Fprintf(&b, "Provenance: **%s** with confidence **%s**", input.Resolution.Source, input.Resolution.Confidence)
	if input.Resolution.Ambiguous {
		b.WriteString(" (ambiguous: the model chose among the recorded candidates)")
	}
	b.WriteString(".\n\n")
	if input.Resolution.EditTarget != "" {
		fmt.Fprintf(&b, "Edit target: `%s` (%s).\n\n", input.Resolution.EditTarget, input.Resolution.EditTargetKind)
	}
	for _, file := range input.Files {
		fmt.Fprintf(&b, "- `%s` — %s\n", file.Path, sanitize(file.Rationale, 500))
	}
	b.WriteString("\n")

	b.WriteString("## Review notes\n\n")
	if input.Validation != nil {
		switch {
		case input.Validation.Passed && input.Validation.Degraded:
			b.WriteString("Validation passed, and schema validation degraded.\n\n")
		case input.Validation.Passed:
			b.WriteString("Validation passed.\n\n")
		default:
			b.WriteString("Validation refused this Proposal; it must not be merged.\n\n")
		}
		if input.Validation.Admission != "" {
			fmt.Fprintf(&b, "%s.\n\n", input.Validation.Admission)
		}
	}
	if len(input.Warnings) > 0 {
		b.WriteString("Review these permitted changes:\n\n")
		for _, warning := range input.Warnings {
			fmt.Fprintf(&b, "- **%s** `%s`: %s\n", warning.Class, warning.Field, sanitize(warning.Message, 500))
		}
		b.WriteString("\n")
	}
	if input.ApprovedBy != "" {
		fmt.Fprintf(&b, "Approved by %s at %s.\n\n", input.ApprovedBy, input.ApprovedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("Pagerless never merges. This Pull Request merges only after the Target Repository's " +
		"Checks pass and the Operator merges it. Admission, quota, and defaulting were not evaluated: " +
		"the repository's Checks are authoritative.\n")
	return truncateBody(b.String())
}

// qualified returns namespace/name, or the name for a cluster-scoped object.
func qualified(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// sanitize removes control characters, keeps line breaks, and bounds the text.
func sanitize(text string, limit int) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			continue
		default:
			b.WriteRune(r)
		}
	}
	cleaned := strings.ReplaceAll(b.String(), "```", "'''")
	if runes := []rune(cleaned); len(runes) > limit {
		return string(runes[:limit])
	}
	return cleaned
}

// truncateBody bounds the body without cutting a line in half.
func truncateBody(body string) string {
	if len(body) <= maxBodyBytes {
		return body
	}
	cut := body[:maxBodyBytes]
	if index := strings.LastIndex(cut, "\n"); index > 0 {
		cut = cut[:index]
	}
	return cut + "\n\n…the rest of the report is recorded on the Page.\n"
}
