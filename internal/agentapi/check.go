package agentapi

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Limits that the server enforces on agent input, matching the Page CRD schema.
const (
	// MaxDiagnosisLength is the longest Diagnosis the Page stores.
	MaxDiagnosisLength = 4000
	// MaxFindingSourceLen is the longest Finding source the Page stores.
	MaxFindingSourceLen = 300
	// MaxFindingSummaryLen is the longest Finding summary the Page stores.
	MaxFindingSummaryLen = 500
	// MaxNarrativeLength is the longest Narrative the Page stores.
	MaxNarrativeLength = 4000
	// MaxStepSummaryLength is the longest step summary the Page stores.
	MaxStepSummaryLength = 200
	// MaxToolNameLength is the longest tool name a step may name.
	MaxToolNameLength = 64
	// MaxInputTokens bounds the input tokens one investigation may report.
	MaxInputTokens int64 = 10_000_000
	// MaxOutputTokens bounds the output tokens one investigation may report.
	MaxOutputTokens int64 = 1_000_000
	// MaxCostUSD bounds the model cost one investigation may report.
	MaxCostUSD float64 = 100
	// MaxModelCalls bounds the model calls one investigation may report.
	MaxModelCalls = 1000
	// MaxSuspiciousOutputs bounds the events a Page keeps.
	MaxSuspiciousOutputs = 20
)

// CheckFilePatch validates a FilePatch Proposal's files and confidence. It is
// exported so the server's Proposal pipeline and the report endpoint share one
// policy.
func CheckFilePatch(path string, files []v1alpha1.FileEdit, confidence string) []ValidationError {
	return checkFilePatch(path, files, confidence)
}

// digestPattern matches a lowercase hexadecimal SHA-256 digest.
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The Proposal shapes a v2 report may send.
const (
	// ProposalKindFilePatch changes whole files in the Target Repository.
	ProposalKindFilePatch = "FilePatch"
	// ProposalKindDiagnosisOnly recommends no repository change.
	ProposalKindDiagnosisOnly = "DiagnosisOnly"
)

// diagnosisOnlyReasons are the documented reasons a DiagnosisOnly Proposal may give.
var diagnosisOnlyReasons = []string{
	"external-cause", "unmanaged-resource", "infrastructure",
	"no-git-repository", "insufficient-evidence", "operator-action-required",
}

var (
	findingKinds = []v1alpha1.FindingKind{
		v1alpha1.FindingKindLog, v1alpha1.FindingKindMetric, v1alpha1.FindingKindTrace,
		v1alpha1.FindingKindEvent, v1alpha1.FindingKindCommit, v1alpha1.FindingKindObject,
	}
	stepKinds = []v1alpha1.StepKind{
		v1alpha1.StepKindToolCall, v1alpha1.StepKindFinding, v1alpha1.StepKindProgress,
		v1alpha1.StepKindSuspiciousOutput,
	}
	actions = []v1alpha1.Action{
		v1alpha1.ActionFilePatch, v1alpha1.ActionScale, v1alpha1.ActionPatchResources,
		v1alpha1.ActionRollback, v1alpha1.ActionNoAction,
	}
)

// Bounded returns the event with its summary and tool name cut to their
// limits. The server bounds step events instead of refusing them, because they
// are informational.
func (e StepEvent) Bounded() StepEvent {
	e.Summary = truncate(e.Summary, MaxStepSummaryLength)
	e.Tool = truncate(e.Tool, MaxToolNameLength)
	e.ToolCallID = truncate(e.ToolCallID, MaxToolNameLength*2)
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
// the Findings, the Proposal, and the usage. Citation verification against the
// recorded tool calls is the server's separate, authoritative check.
func (r Report) Check() []ValidationError {
	var errs []ValidationError
	if r.Page != nil {
		if strings.TrimSpace(r.Page.Name) == "" {
			errs = append(errs, ValidationError{Field: "page.name", Message: "page.name is required"})
		}
		if strings.TrimSpace(r.Page.UID) == "" {
			errs = append(errs, ValidationError{Field: "page.uid", Message: "page.uid is required"})
		}
		if strings.TrimSpace(r.Page.Namespace) == "" {
			errs = append(errs, ValidationError{Field: "page.namespace", Message: "page.namespace is required"})
		}
	}
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
	errs = append(errs, r.Proposal.check("proposal")...)
	errs = append(errs, checkUsage("usage", r.Usage)...)
	if r.Capabilities != nil && r.Capabilities.UsageSource != "" {
		switch r.Capabilities.UsageSource {
		case "provider", "estimated", "mixed":
		default:
			errs = append(errs, ValidationError{
				Field:   "capabilities.usageSource",
				Message: "usageSource must be provider, estimated, or mixed",
			})
		}
	}
	return errs
}

// IsV2 reports whether the report uses the v2 proposal shape. A v2 agent always
// identifies its Page and cites every Finding; a v1 report does neither.
func (r Report) IsV2() bool {
	return r.Page != nil || r.Proposal.Kind != ""
}

// ActionFor returns the Proposal as the Page stores it. A v2 FilePatch becomes
// the FilePatch Action; a v2 DiagnosisOnly becomes the no-change Action with
// its reason and recommendation.
func (r Report) ActionFor() v1alpha1.ProposedAction {
	switch r.Proposal.Kind {
	case ProposalKindFilePatch:
		return v1alpha1.ProposedAction{
			Action: v1alpha1.ActionFilePatch,
			Parameters: &v1alpha1.ActionParameters{FilePatch: &v1alpha1.FilePatchParameters{
				Files:      slices.Clone(r.Proposal.Files),
				Confidence: r.Proposal.Confidence,
			}},
		}
	case ProposalKindDiagnosisOnly:
		return v1alpha1.ProposedAction{
			Action:         v1alpha1.ActionNoAction,
			Reason:         r.Proposal.Reason,
			Recommendation: r.Proposal.Recommendation,
		}
	default:
		proposal := r.Proposal.ProposedAction
		if r.Proposal.Parameters != nil {
			proposal.Parameters = r.Proposal.Parameters.DeepCopy()
		}
		return proposal
	}
}

// check validates the proposal variant.
func (p ReportProposal) check(path string) []ValidationError {
	switch p.Kind {
	case "":
		return checkProposedAction(path, p.ProposedAction)
	case ProposalKindFilePatch:
		return checkFilePatch(path, p.Files, p.Confidence)
	case ProposalKindDiagnosisOnly:
		return checkDiagnosisOnly(path, p.Reason, p.Recommendation)
	default:
		return []ValidationError{{
			Field:   path + ".kind",
			Message: "kind must be FilePatch or DiagnosisOnly",
		}}
	}
}

// checkFilePatch validates the whole-file edits of a FilePatch Proposal.
func checkFilePatch(path string, files []v1alpha1.FileEdit, confidence string) []ValidationError {
	var errs []ValidationError
	if len(files) == 0 || len(files) > v1alpha1.MaxProposalFiles {
		errs = append(errs, ValidationError{
			Field:   path + ".files",
			Message: fmt.Sprintf("files must contain between 1 and %d edits", v1alpha1.MaxProposalFiles),
		})
	}
	seen := make(map[string]struct{}, len(files))
	for i, file := range files {
		field := fmt.Sprintf("%s.files[%d]", path, i)
		errs = append(errs, checkFileEdit(field, file, seen)...)
	}
	if confidence != "" {
		switch confidence {
		case "exact", "configured", "identity", "name-only":
		default:
			errs = append(errs, ValidationError{
				Field:   path + ".confidence",
				Message: "confidence must be exact, configured, identity, or name-only",
			})
		}
	}
	return errs
}

// checkFileEdit validates one whole-file edit against the documented path and content policy.
func checkFileEdit(path string, file v1alpha1.FileEdit, seen map[string]struct{}) []ValidationError {
	var errs []ValidationError
	switch {
	case strings.TrimSpace(file.Path) == "":
		errs = append(errs, ValidationError{Field: path + ".path", Message: "path is required"})
	case utf8.RuneCountInString(file.Path) > 500:
		errs = append(errs, ValidationError{Field: path + ".path", Message: "path must not exceed 500 characters"})
	case !ValidRepositoryPath(file.Path):
		errs = append(errs, ValidationError{
			Field:   path + ".path",
			Message: "path must be a clean relative .yaml or .yml path outside .git and .github",
		})
	}
	if _, duplicate := seen[file.Path]; duplicate {
		errs = append(errs, ValidationError{Field: path + ".path", Message: "the same path appears twice"})
	}
	seen[file.Path] = struct{}{}
	switch {
	case strings.TrimSpace(file.UpdatedContent) == "" && file.ContentDigest == "":
		errs = append(errs, ValidationError{Field: path + ".updatedContent", Message: "updatedContent is required"})
	case strings.ContainsRune(file.UpdatedContent, 0):
		errs = append(errs, ValidationError{Field: path + ".updatedContent", Message: "updatedContent must not contain a NUL byte"})
	case strings.ContainsRune(file.UpdatedContent, '\r'):
		errs = append(errs, ValidationError{Field: path + ".updatedContent", Message: "updatedContent must use LF line endings"})
	case len(file.UpdatedContent) > v1alpha1.MaxFileContentBytes:
		errs = append(errs, ValidationError{
			Field:   path + ".updatedContent",
			Message: fmt.Sprintf("updatedContent must not exceed %d bytes", v1alpha1.MaxFileContentBytes),
		})
	}
	if file.ContentDigest != "" && !digestPattern.MatchString(file.ContentDigest) {
		errs = append(errs, ValidationError{Field: path + ".contentDigest", Message: "contentDigest must be a SHA-256 digest"})
	}
	switch {
	case strings.TrimSpace(file.Rationale) == "":
		errs = append(errs, ValidationError{Field: path + ".rationale", Message: "rationale is required"})
	case utf8.RuneCountInString(file.Rationale) > 1000:
		errs = append(errs, ValidationError{Field: path + ".rationale", Message: "rationale must not exceed 1000 characters"})
	}
	return errs
}

// checkDiagnosisOnly validates a no-change Proposal.
func checkDiagnosisOnly(path, reason, recommendation string) []ValidationError {
	var errs []ValidationError
	if !slices.Contains(diagnosisOnlyReasons, reason) {
		errs = append(errs, ValidationError{
			Field:   path + ".reason",
			Message: fmt.Sprintf("reason must be one of %v", diagnosisOnlyReasons),
		})
	}
	switch {
	case strings.TrimSpace(recommendation) == "":
		errs = append(errs, ValidationError{Field: path + ".recommendation", Message: "recommendation is required"})
	case utf8.RuneCountInString(recommendation) > 2000:
		errs = append(errs, ValidationError{Field: path + ".recommendation", Message: "recommendation must not exceed 2000 characters"})
	}
	return errs
}

// ValidRepositoryPath reports whether a path is a clean relative YAML manifest
// outside the repository's automation and Git metadata. It is the same policy
// the agent applies locally, so a refusal is never a surprise.
func ValidRepositoryPath(path string) bool {
	if path == "" || len(path) > 500 {
		return false
	}
	if strings.HasPrefix(path, "/") || strings.ContainsRune(path, '\\') || strings.ContainsRune(path, 0) {
		return false
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, r := range segment {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '.', r == '_', r == '-':
			default:
				return false
			}
		}
	}
	if segments[0] == ".git" || segments[0] == ".github" {
		return false
	}
	return strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")
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
	if usage.ModelCalls < 0 || usage.ModelCalls > MaxModelCalls {
		errs = append(errs, ValidationError{
			Field:   path + ".modelCalls",
			Message: fmt.Sprintf("modelCalls must be between 0 and %d", MaxModelCalls),
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
	if utf8.RuneCountInString(f.ToolCallID) > MaxToolNameLength {
		errs = append(errs, ValidationError{Field: path + ".toolCallId", Message: fmt.Sprintf("toolCallId must not exceed %d characters", MaxToolNameLength)})
	}
	if utf8.RuneCountInString(f.Pointer) > 500 {
		errs = append(errs, ValidationError{Field: path + ".pointer", Message: "pointer must not exceed 500 characters"})
	}
	if f.Pointer != "" && !validPointer(f.Pointer) {
		errs = append(errs, ValidationError{
			Field:   path + ".pointer",
			Message: "pointer must be a JSON Pointer into the /data or /meta object",
		})
	}
	return errs
}

// validPointer reports whether a pointer has the documented shape: a path into
// the tool result's data or meta object.
func validPointer(pointer string) bool {
	if !strings.HasPrefix(pointer, "/data") && !strings.HasPrefix(pointer, "/meta") {
		return false
	}
	if remainder := strings.TrimPrefix(strings.TrimPrefix(pointer, "/data"), "/meta"); remainder != "" {
		if !strings.HasPrefix(remainder, "/") {
			return false
		}
	}
	for _, segment := range strings.Split(pointer, "/")[1:] {
		if strings.ContainsAny(segment, " \t\r\n") {
			return false
		}
	}
	return true
}

// checkProposedAction verifies that the Action is on the menu and that exactly
// the parameters that belong to it are set.
func checkProposedAction(path string, p v1alpha1.ProposedAction) []ValidationError {
	if !slices.Contains(actions, p.Action) {
		return []ValidationError{{Field: path + ".action", Message: fmt.Sprintf("action must be one of %v", actions)}}
	}
	if p.Action == v1alpha1.ActionFilePatch {
		if p.Parameters == nil || p.Parameters.FilePatch == nil {
			return []ValidationError{{
				Field:   path + ".parameters.filePatch",
				Message: "parameters.filePatch is required for FilePatch",
			}}
		}
		return checkFilePatch(path+".parameters.filePatch", p.Parameters.FilePatch.Files, p.Parameters.FilePatch.Confidence)
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
