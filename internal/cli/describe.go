package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// describe writes the presentation of a Page: its header, its Diagnosis and
// Findings, its Proposal with the rendered change, the human decision, the
// execution result, the Verification checks, and the Narrative.
func describe(w io.Writer, tr *v1alpha1.Page) error {
	p := &printer{w: w}
	status := &tr.Status
	p.field("Name", tr.Name)
	p.field("Phase", phaseLabel(status.Phase))
	p.field("Target", targetLabel(tr.Spec.Target))
	p.field("Trigger", triggerLabel(tr.Spec.Trigger))
	p.field("Triggers", strconv.Itoa(int(status.TriggerCount)))
	p.field("Created", tr.CreationTimestamp.Format(time.RFC3339))
	if tr.Spec.Hint != "" {
		p.field("Hint", tr.Spec.Hint)
	}
	if tr.Spec.PreviousPage != "" {
		p.field("Previous", tr.Spec.PreviousPage)
	}
	if failure := failureLabel(tr); failure != "" {
		p.field("Failure", failure)
	}

	p.section("Diagnosis")
	p.block("  ", status.Diagnosis)

	p.section("Findings")
	writeFindings(p, status.Findings)

	p.section("Proposal")
	writeProposal(p, status.Proposal)

	p.section("Decision")
	writeDecision(p, status.Decision)

	p.section("Pull request")
	writePullRequest(p, status.PullRequest)

	p.section("Verification")
	writeVerification(p, status.VerificationChecks)

	p.section("Narrative")
	p.block("  ", status.Narrative)
	return p.err
}

// triggerLabel describes what started a Page, with the pod and alert that the Trigger names.
func triggerLabel(trigger v1alpha1.Trigger) string {
	var details []string
	if trigger.Pod != "" {
		details = append(details, "pod "+trigger.Pod)
	}
	if trigger.AlertFingerprint != "" {
		details = append(details, "alert "+trigger.AlertFingerprint)
	}
	if len(details) == 0 {
		return string(trigger.Source)
	}
	return string(trigger.Source) + " (" + strings.Join(details, ", ") + ")"
}

// failureLabel describes the reason and message of the Failed condition, or returns an empty string.
func failureLabel(tr *v1alpha1.Page) string {
	condition := meta.FindStatusCondition(tr.Status.Conditions, v1alpha1.ConditionFailed)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return ""
	}
	if condition.Message == "" {
		return condition.Reason
	}
	return condition.Reason + ": " + condition.Message
}

// writeFindings writes the Findings with their sources.
func writeFindings(p *printer, findings []v1alpha1.Finding) {
	if len(findings) == 0 {
		p.block("  ", "")
		return
	}
	for i, finding := range findings {
		p.printf("  %d. %s: %s\n", i+1, finding.Kind, finding.Source)
		p.block("     ", finding.Summary)
	}
}

// writeProposal writes the Proposal, its parameters, and the change that Validation rendered.
func writeProposal(p *printer, proposal *v1alpha1.Proposal) {
	if proposal == nil {
		p.block("  ", "")
		return
	}
	p.printf("  Attempt:    %d\n", proposal.Attempt)
	p.printf("  Hash:       %s\n", proposal.Hash)
	p.printf("  Expires:    %s\n", proposal.ExpiresAt.Format(time.RFC3339))
	p.printf("  Action:     %s\n", proposal.Action)
	p.printf("  Parameters: %s\n", parametersLabel(proposal.Parameters))
	if base := proposal.Base; base != nil {
		var parts []string
		if base.Commit != "" {
			parts = append(parts, "commit "+base.Commit)
		}
		if base.TargetGeneration != 0 {
			parts = append(parts, "Target generation "+strconv.FormatInt(base.TargetGeneration, 10))
		}
		p.printf("  Base:       %s\n", orNone(strings.Join(parts, ", ")))
	}
	if change := proposal.RenderedChange; change != nil {
		if change.Path != "" {
			p.printf("  Path:       %s\n", change.Path)
		}
		if change.Patch != "" {
			p.printf("  Patch:\n")
			p.block("    ", change.Patch)
		}
	}
}

// parametersLabel describes the parameters of the Proposal's Action.
func parametersLabel(params *v1alpha1.ActionParameters) string {
	switch {
	case params == nil:
		return placeholder
	case params.Scale != nil:
		return fmt.Sprintf("replicas %d", params.Scale.Replicas)
	case params.PatchResources != nil:
		patch := params.PatchResources
		parts := []string{"container " + patch.Container}
		if len(patch.Requests) > 0 {
			parts = append(parts, "requests "+resourceListLabel(patch.Requests))
		}
		if len(patch.Limits) > 0 {
			parts = append(parts, "limits "+resourceListLabel(patch.Limits))
		}
		return strings.Join(parts, ", ")
	case params.Rollback != nil:
		return "commit " + params.Rollback.Commit
	default:
		return placeholder
	}
}

// resourceListLabel formats a resource list in name order.
func resourceListLabel(list corev1.ResourceList) string {
	names := slices.Sorted(maps.Keys(list))
	parts := make([]string, 0, len(names))
	for _, name := range names {
		quantity := list[name]
		parts = append(parts, string(name)+"="+quantity.String())
	}
	return strings.Join(parts, " ")
}

// writeDecision writes the human decision on the Proposal.
func writeDecision(p *printer, decision *v1alpha1.Decision) {
	if decision == nil {
		p.block("  ", "")
		return
	}
	p.printf("  Outcome:    %s\n", decision.Outcome)
	p.printf("  Proposal:   %s\n", decision.ProposalHash)
	p.printf("  Decided by: %s\n", decision.DecidedBy)
	p.printf("  Decided:    %s\n", decision.DecidedAt.Format(time.RFC3339))
	if decision.Reason != "" {
		p.printf("  Reason:     %s\n", decision.Reason)
	}
}

// writePullRequest writes the pull request that carries the approved Proposal.
func writePullRequest(p *printer, pullRequest *v1alpha1.PullRequest) {
	if pullRequest == nil {
		p.block("  ", "")
		return
	}
	p.printf("  Provider:     %s\n", orNone(pullRequest.Provider))
	p.printf("  Number:       %d\n", pullRequest.Number)
	p.printf("  URL:          %s\n", orNone(pullRequest.URL))
	p.printf("  State:        %s\n", orNone(string(pullRequest.State)))
	p.printf("  Checks:       %s\n", orNone(string(pullRequest.Checks)))
	p.printf("  Merge commit: %s\n", orNone(pullRequest.MergeCommit))
}

// writeVerification writes the result of each Verification check.
func writeVerification(p *printer, checks []v1alpha1.VerificationCheck) {
	if len(checks) == 0 {
		p.block("  ", "")
		return
	}
	for _, check := range checks {
		result := "FAIL"
		if check.Passed {
			result = "PASS"
		}
		line := fmt.Sprintf("  %s  %s", result, check.Name)
		if check.Message != "" {
			line += ": " + check.Message
		}
		p.printf("%s\n", line)
	}
}

// orNone returns the value, or the placeholder when the value is empty.
func orNone(value string) string {
	if value == "" {
		return placeholder
	}
	return value
}
