package agentapi

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Limits that the server enforces on agent input, matching the Page CRD schema.
const (
	MaxDiagnosisLength   = 4000
	MaxFindingSourceLen  = 500
	MaxFindingSummaryLen = 1000
	MaxNarrativeLength   = 4000
	MaxStepSummaryLength = 300
	MaxToolNameLength    = 128
	// MaxInputTokens bounds the input tokens one investigation may report.
	MaxInputTokens int64 = 10_000_000
	// MaxOutputTokens bounds the output tokens one investigation may report.
	MaxOutputTokens int64 = 1_000_000
	// MaxCostUSD bounds the model cost one investigation may report.
	MaxCostUSD float64 = 100
)

var (
	findingKinds = []v1alpha1.FindingKind{
		v1alpha1.FindingKindLog, v1alpha1.FindingKindMetric, v1alpha1.FindingKindTrace,
		v1alpha1.FindingKindEvent, v1alpha1.FindingKindCommit,
	}
	stepKinds = []v1alpha1.StepKind{
		v1alpha1.StepKindToolCall, v1alpha1.StepKindFinding, v1alpha1.StepKindProgress,
	}
	actions = []v1alpha1.Action{
		v1alpha1.ActionScale, v1alpha1.ActionPatchResources,
		v1alpha1.ActionRollback, v1alpha1.ActionNoAction,
	}
)

// Bounded returns the event with its summary and tool name cut to their
// limits. The server bounds step events instead of refusing them, because they
// are informational.
func (e StepEvent) Bounded() StepEvent {
	e.Summary = truncate(e.Summary, MaxStepSummaryLength)
	e.Tool = truncate(e.Tool, MaxToolNameLength)
	return e
}

// truncate cuts s to at most limit characters.
func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

// Check returns the structural errors in a step event.
func (e StepEvent) Check() []ValidationError {
	var errs []ValidationError
	if !slices.Contains(stepKinds, e.Kind) {
		errs = append(errs, ValidationError{Field: "kind", Message: fmt.Sprintf("kind must be one of %v", stepKinds)})
	}
	if strings.TrimSpace(e.Summary) == "" {
		errs = append(errs, ValidationError{Field: "summary", Message: "summary is required"})
	}
	switch {
	case e.Kind == v1alpha1.StepKindFinding && e.Finding == nil:
		errs = append(errs, ValidationError{Field: "finding", Message: "a Finding step must carry the finding"})
	case e.Kind != v1alpha1.StepKindFinding && e.Finding != nil:
		errs = append(errs, ValidationError{Field: "finding", Message: "only a Finding step may carry a finding"})
	case e.Finding != nil:
		errs = append(errs, checkFinding("finding", *e.Finding)...)
	}
	return errs
}

// Check returns the structural errors in a report: the shape of the Diagnosis,
// the Findings, and the Proposal. Whether the Proposal's Action is accepted
// and within bounds is Validation, which the server performs afterwards.
func (r Report) Check() []ValidationError {
	var errs []ValidationError
	if strings.TrimSpace(r.Diagnosis) == "" {
		errs = append(errs, ValidationError{Field: "diagnosis", Message: "diagnosis is required"})
	}
	if utf8.RuneCountInString(r.Diagnosis) > MaxDiagnosisLength {
		errs = append(errs, ValidationError{Field: "diagnosis", Message: fmt.Sprintf("diagnosis must not exceed %d characters", MaxDiagnosisLength)})
	}
	if len(r.Findings) > v1alpha1.MaxFindings {
		errs = append(errs, ValidationError{Field: "findings", Message: fmt.Sprintf("at most %d findings are allowed", v1alpha1.MaxFindings)})
	}
	for i, f := range r.Findings {
		errs = append(errs, checkFinding(fmt.Sprintf("findings[%d]", i), f)...)
	}
	errs = append(errs, checkProposedAction("proposal", r.Proposal)...)
	errs = append(errs, checkUsage("usage", r.Usage)...)
	return errs
}

// checkUsage verifies that the reported model usage is inside the bounds the CRD declares.
func checkUsage(path string, usage *v1alpha1.Usage) []ValidationError {
	if usage == nil {
		return nil
	}
	var errs []ValidationError
	if usage.InputTokens < 0 || usage.InputTokens > MaxInputTokens {
		errs = append(errs, ValidationError{
			Field:   path + ".inputTokens",
			Message: fmt.Sprintf("inputTokens must be between 0 and %d", MaxInputTokens),
		})
	}
	if usage.OutputTokens < 0 || usage.OutputTokens > MaxOutputTokens {
		errs = append(errs, ValidationError{
			Field:   path + ".outputTokens",
			Message: fmt.Sprintf("outputTokens must be between 0 and %d", MaxOutputTokens),
		})
	}
	if math.IsNaN(usage.CostUSD) || math.IsInf(usage.CostUSD, 0) ||
		usage.CostUSD < 0 || usage.CostUSD > MaxCostUSD {
		errs = append(errs, ValidationError{
			Field:   path + ".costUSD",
			Message: fmt.Sprintf("costUSD must be between 0 and %g", MaxCostUSD),
		})
	}
	return errs
}

// Check returns the structural errors in a Narrative: it must be non-blank and
// within the length the Page stores.
func (n NarrativeReport) Check() []ValidationError {
	if strings.TrimSpace(n.Narrative) == "" {
		return []ValidationError{{Field: "narrative", Message: "narrative is required"}}
	}
	if utf8.RuneCountInString(n.Narrative) > MaxNarrativeLength {
		return []ValidationError{{
			Field:   "narrative",
			Message: fmt.Sprintf("narrative must not exceed %d characters", MaxNarrativeLength),
		}}
	}
	return nil
}

func checkFinding(path string, f v1alpha1.Finding) []ValidationError {
	var errs []ValidationError
	if !slices.Contains(findingKinds, f.Kind) {
		errs = append(errs, ValidationError{Field: path + ".kind", Message: fmt.Sprintf("kind must be one of %v", findingKinds)})
	}
	if strings.TrimSpace(f.Source) == "" {
		errs = append(errs, ValidationError{Field: path + ".source", Message: "every finding must cite its source"})
	}
	if utf8.RuneCountInString(f.Source) > MaxFindingSourceLen {
		errs = append(errs, ValidationError{Field: path + ".source", Message: fmt.Sprintf("source must not exceed %d characters", MaxFindingSourceLen)})
	}
	if strings.TrimSpace(f.Summary) == "" {
		errs = append(errs, ValidationError{Field: path + ".summary", Message: "summary is required"})
	}
	if utf8.RuneCountInString(f.Summary) > MaxFindingSummaryLen {
		errs = append(errs, ValidationError{Field: path + ".summary", Message: fmt.Sprintf("summary must not exceed %d characters", MaxFindingSummaryLen)})
	}
	return errs
}

// checkProposedAction verifies that the Action is on the menu and that exactly
// the parameters that belong to it are set.
func checkProposedAction(path string, p v1alpha1.ProposedAction) []ValidationError {
	if !slices.Contains(actions, p.Action) {
		return []ValidationError{{Field: path + ".action", Message: fmt.Sprintf("action must be one of %v", actions)}}
	}
	var scale, patch, rollback bool
	if p.Parameters != nil {
		scale = p.Parameters.Scale != nil
		patch = p.Parameters.PatchResources != nil
		rollback = p.Parameters.Rollback != nil
	}
	var errs []ValidationError
	for _, c := range []struct {
		action v1alpha1.Action
		field  string
		set    bool
	}{
		{v1alpha1.ActionScale, "scale", scale},
		{v1alpha1.ActionPatchResources, "patchResources", patch},
		{v1alpha1.ActionRollback, "rollback", rollback},
	} {
		switch {
		case p.Action == c.action && !c.set:
			errs = append(errs, ValidationError{Field: path + ".parameters." + c.field, Message: fmt.Sprintf("parameters.%s is required for %s", c.field, c.action)})
		case p.Action != c.action && c.set:
			errs = append(errs, ValidationError{Field: path + ".parameters." + c.field, Message: fmt.Sprintf("parameters.%s is not allowed for %s", c.field, p.Action)})
		}
	}
	return errs
}
