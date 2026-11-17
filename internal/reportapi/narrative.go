package reportapi

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// maxNarrativeRecordChars bounds the text a Narrative Job is given, and so the
// Narrative prompt.
const maxNarrativeRecordChars = 12_000

// noValue stands for a section that the Triage does not have.
const noValue = "none"

// narrativeRecordText renders a Triage for its Narrative Job. It describes what
// the Triage concluded and what was decided, so that the Narrative can be a
// complete account. It is untrusted data for the model, and the Narrative prompt
// forbids quoting the volatile identifiers that some of it contains.
func narrativeRecordText(t *v1alpha1.Triage) string {
	var b strings.Builder
	status := &t.Status
	workload := t.Spec.Workload
	b.WriteString("Workload: " + string(workload.Kind) + " " + workload.Namespace + "/" + workload.Name + "\n")
	b.WriteString("Trigger: " + string(t.Spec.Trigger.Source) + "\n")
	b.WriteString("Phase: " + string(status.Phase) + "\n")
	if t.Spec.Hint != "" {
		b.WriteString("Hint from the engineer: " + t.Spec.Hint + "\n")
	}
	if t.Spec.FollowsTriage != "" {
		b.WriteString("Follows Triage: " + t.Spec.FollowsTriage + "\n")
	}

	b.WriteString("\nDiagnosis:\n" + orNone(status.Diagnosis) + "\n")

	b.WriteString("\nFindings:\n")
	if len(status.Findings) == 0 {
		b.WriteString(noValue + "\n")
	}
	for _, finding := range status.Findings {
		b.WriteString(string(finding.Kind) + ": " + finding.Source + "\n  " + finding.Summary + "\n")
	}

	b.WriteString("\nProposal:\n")
	if status.Proposal == nil {
		b.WriteString(noValue + "\n")
	} else {
		proposal := status.Proposal
		b.WriteString("Action: " + string(proposal.Action) + "\n")
		b.WriteString("Parameters: " + narrativeParameters(proposal) + "\n")
		if change := proposal.RenderedChange; change != nil {
			if change.Path != "" {
				b.WriteString("Changed file: " + change.Path + "\n")
			}
			if change.Patch != "" {
				b.WriteString("Change:\n" + change.Patch + "\n")
			}
		}
	}

	b.WriteString("\nDecision:\n")
	if status.Decision == nil {
		b.WriteString(noValue + "\n")
	} else {
		decision := status.Decision
		b.WriteString("Outcome: " + string(decision.Outcome) + "\n")
		b.WriteString("Decided by: " + decision.Approver + "\n")
		if decision.Reason != "" {
			b.WriteString("Reason: " + decision.Reason + "\n")
		}
	}

	b.WriteString("\nExecution:\n")
	if status.Execution == nil {
		b.WriteString(noValue + "\n")
	} else {
		execution := status.Execution
		if execution.PullRequestURL != "" {
			b.WriteString("A pull request was opened and merged\n")
		}
		if len(execution.RestartedPods) > 0 {
			b.WriteString("Restarted pods: " + strconv.Itoa(len(execution.RestartedPods)) + "\n")
		}
	}

	b.WriteString("\nVerification:\n")
	if len(status.VerificationChecks) == 0 {
		b.WriteString(noValue + "\n")
	}
	for _, check := range status.VerificationChecks {
		result := "FAIL"
		if check.Passed {
			result = "PASS"
		}
		b.WriteString(result + " " + check.Name)
		if check.Message != "" {
			b.WriteString(": " + check.Message)
		}
		b.WriteString("\n")
	}

	if status.FollowUpTriage != "" {
		b.WriteString("\nFollow-up Triage: " + status.FollowUpTriage + "\n")
	}
	return truncateRecord(b.String())
}

// narrativeParameters describes the Proposal's parameters. A Rollback's commit is
// left out, because the Narrative describes the revert rather than the commit.
func narrativeParameters(proposal *v1alpha1.Proposal) string {
	parameters := proposal.Parameters
	switch {
	case parameters == nil:
		return noValue
	case parameters.Scale != nil:
		return "replicas " + strconv.Itoa(int(parameters.Scale.Replicas))
	case parameters.PatchResources != nil:
		patch := parameters.PatchResources
		parts := []string{"container " + patch.Container}
		if len(patch.Requests) > 0 {
			parts = append(parts, "requests "+narrativeResources(patch.Requests))
		}
		if len(patch.Limits) > 0 {
			parts = append(parts, "limits "+narrativeResources(patch.Limits))
		}
		return strings.Join(parts, ", ")
	case parameters.Rollback != nil:
		return "revert the most recent change"
	default:
		return noValue
	}
}

// narrativeResources writes a resource list in name order.
func narrativeResources(list corev1.ResourceList) string {
	parts := make([]string, 0, len(list))
	for _, name := range slices.Sorted(maps.Keys(list)) {
		quantity := list[name]
		parts = append(parts, string(name)+"="+quantity.String())
	}
	return strings.Join(parts, " ")
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return noValue
	}
	return value
}

func truncateRecord(text string) string {
	if len(text) <= maxNarrativeRecordChars {
		return text
	}
	return text[:maxNarrativeRecordChars] + "\n[truncated]"
}
