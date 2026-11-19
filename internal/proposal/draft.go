package proposal

import (
	"fmt"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// DraftRule is the Operator's rule for the unattended path: opening a draft
// Pull Request before any human has approved the Proposal (ADR-0039). It is
// never enabled implicitly.
type DraftRule struct {
	// AllowConfigured permits a Resolution whose Confidence is configured to
	// open an unattended draft. The Operator sets it per Target Repository, so a
	// repository rule that names the manifest explicitly may be trusted without
	// a human approval.
	AllowConfigured bool
}

// UnattendedDraft reports whether the Page's Proposal may be opened as a draft
// Pull Request without an Approval, and the first reason it may not. The
// decision reads only values recorded on the Page, so it is reproducible and
// cannot be influenced by model text.
//
// A draft requires all of: a Resolution of exact confidence, or configured
// under an explicit rule; a FilePatch Proposal that changes at least one file;
// a Validation that passed without degradation and without warnings; and a
// Trigger that is not a human request. An ambiguous Resolution is never
// eligible, because the model chose among rank-equal candidates and the choice
// needs a human's review. A draft is nevertheless never merged by Pagerless.
func UnattendedDraft(page *v1alpha1.Page, rule DraftRule) (bool, string) {
	if page == nil {
		return false, "the Page is missing"
	}
	if page.Spec.Trigger.Source.IsHumanRequest() {
		return false, "the Operator requested the Page"
	}
	resolution := page.Status.Repository
	switch {
	case resolution == nil:
		return false, "the Target Repository is not grounded in a Resolution"
	case resolution.Ambiguous:
		return false, "the Resolution is ambiguous"
	case resolution.Confidence == v1alpha1.ConfidenceNone || resolution.Confidence == "":
		return false, "the Resolution found no repository grounding"
	case !resolution.Confidence.PermitsUnattendedDraft():
		return false, fmt.Sprintf("a %s Resolution never opens an unattended draft", resolution.Confidence)
	case resolution.Confidence == v1alpha1.ConfidenceConfigured && !rule.AllowConfigured:
		return false, "a configured Resolution needs an explicit allowAutoDraft rule"
	}
	current := page.Status.Proposal
	switch {
	case current == nil:
		return false, "the Page has no Proposal"
	case current.Kind != v1alpha1.ProposalKindFilePatch || len(current.Files) == 0:
		return false, "the Proposal changes no files"
	}
	report := current.Validation
	switch {
	case report == nil:
		return false, "the Proposal has no Validation report"
	case !report.Passed:
		return false, "Validation refused the Proposal"
	case report.Degraded:
		return false, "Validation degraded, so a human must review the Proposal"
	case len(report.Warnings) > 0:
		return false, fmt.Sprintf("the Proposal carries a %s warning, so a human must review it",
			report.Warnings[0].Class)
	}
	return true, ""
}
