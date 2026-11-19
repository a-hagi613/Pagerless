// Package agentapi defines the contract between an agent Job and
// pagerless-server: the environment the server gives the Job, the investigation
// manifest, the tool calls, the step events and the report the agent posts, and
// the server's answers.
//
// The agent authenticates every request with a projected service account token
// whose audience is bound to the one Page the Job investigates. The server
// answers a report synchronously with acceptance or Validation errors.
package agentapi

import (
	"net/url"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// AudiencePrefix is the prefix of the audience of the projected service account
// token that an agent presents. A token minted for one Page is therefore not
// valid for another.
const AudiencePrefix = "pagerless-server/page/"

// AudienceForPage returns the audience of the token that investigates one Page.
func AudienceForPage(pageUID string) string {
	return AudiencePrefix + pageUID
}

// Environment variables that the server sets on the agent Job's container. They
// are the whole configuration of the agent: two base URLs, the Page's identity,
// the model endpoint, and the four budgets. The agent reads no other source.
const (
	// EnvServerURL is the base URL of pagerless-server's agent API.
	EnvServerURL = "PAGERLESS_SERVER_URL"
	// EnvTokenFile is the path of the projected service account token.
	EnvTokenFile = "PAGERLESS_TOKEN_FILE"
	// EnvPage is the name of the Page the agent investigates.
	EnvPage = "PAGERLESS_PAGE"
	// EnvPageUID is the UID of the Page, which the agent attributes every request to.
	EnvPageUID = "PAGERLESS_PAGE_UID"
	// EnvTask selects the Job's work: TaskInvestigate or TaskNarrative.
	EnvTask = "PAGERLESS_TASK"
	// EnvHint is the human Hint, when there is one.
	EnvHint = "PAGERLESS_HINT"

	// EnvModelBaseURL is the OpenAI-compatible endpoint the agent calls.
	EnvModelBaseURL = "PAGERLESS_MODEL_BASE_URL"
	// EnvModelName is the model identifier the endpoint expects.
	EnvModelName = "PAGERLESS_MODEL_NAME"
	// EnvModelAPIKeyFile is the path of the mounted model API key.
	EnvModelAPIKeyFile = "PAGERLESS_MODEL_API_KEY_FILE"
	// EnvModelHeaders are optional request headers as a JSON object.
	EnvModelHeaders = "PAGERLESS_MODEL_HEADERS"
	// EnvModelMaxOutputTokens caps one model call's output.
	EnvModelMaxOutputTokens = "PAGERLESS_MODEL_MAX_OUTPUT_TOKENS"
	// EnvModelContextTokens is the model's context window.
	EnvModelContextTokens = "PAGERLESS_MODEL_CONTEXT_TOKENS"
	// EnvModelStructuredOutput selects the first structured-output rung, or auto.
	EnvModelStructuredOutput = "PAGERLESS_MODEL_STRUCTURED_OUTPUT"
	// EnvModelMaxTokensField selects max_tokens or max_completion_tokens, or auto.
	EnvModelMaxTokensField = "PAGERLESS_MODEL_MAX_TOKENS_FIELD"
	// EnvModelSupportsTemperature states whether the endpoint accepts a temperature.
	EnvModelSupportsTemperature = "PAGERLESS_MODEL_SUPPORTS_TEMPERATURE"
	// EnvModelConnectTimeout is the connect timeout in seconds.
	EnvModelConnectTimeout = "PAGERLESS_MODEL_CONNECT_TIMEOUT_S"
	// EnvModelReadTimeout is the read timeout in seconds.
	EnvModelReadTimeout = "PAGERLESS_MODEL_READ_TIMEOUT_S"
	// EnvAllowInsecureModelEndpoint permits a plain-HTTP model endpoint.
	EnvAllowInsecureModelEndpoint = "PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT"

	// EnvBudgetToolCalls is the number of tool calls one investigation may make.
	EnvBudgetToolCalls = "PAGERLESS_BUDGET_TOOL_CALLS"
	// EnvBudgetModelCalls is the number of model calls one investigation may make.
	EnvBudgetModelCalls = "PAGERLESS_BUDGET_MODEL_CALLS"
	// EnvBudgetInputTokens is the input-token budget of one investigation.
	EnvBudgetInputTokens = "PAGERLESS_BUDGET_INPUT_TOKENS"
	// EnvBudgetSeconds is the wall-clock budget of one investigation.
	EnvBudgetSeconds = "PAGERLESS_BUDGET_SECONDS"
)

// The values of EnvTask: the two kinds of Job the server starts from the agent image.
const (
	// TaskInvestigate runs the investigation graph.
	TaskInvestigate = "investigate"
	// TaskNarrative writes the Narrative of a finished Page.
	TaskNarrative = "narrative"
)

// The four budgets of one investigation, matching the Page's manifest and the
// agent's defaults (spec §3.6.1).
const (
	// DefaultToolCallBudget is the number of tool calls one investigation may make.
	DefaultToolCallBudget = 15
	// DefaultModelCallBudget is the number of model calls one investigation may make.
	DefaultModelCallBudget = 12
	// DefaultInputTokenBudget is the input-token budget of one investigation.
	DefaultInputTokenBudget int64 = 300_000
	// DefaultWallClockBudget is the wall-clock budget of one investigation, in seconds.
	DefaultWallClockBudget = 300
)

// PageUIDHeader attributes a request to the Page whose UID it carries. The
// server refuses a header that disagrees with the Page and the token audience.
const PageUIDHeader = "X-Pagerless-Page-Uid"

// PageUIDHeaderCompat is the second attribution header, accepted as an alias.
const PageUIDHeaderCompat = "X-Page-UID"

// InvestigationPath is the path an agent reads the manifest from.
func InvestigationPath(page string) string {
	return "/v1/pages/" + url.PathEscape(page) + "/investigation"
}

// ToolPath is the path an agent calls one tool on.
func ToolPath(page, tool string) string {
	return "/v1/pages/" + url.PathEscape(page) + "/tools/" + url.PathEscape(tool)
}

// EventsPath is the path an agent posts step events to.
func EventsPath(page string) string {
	return "/v1/pages/" + url.PathEscape(page) + "/events"
}

// ReportPath is the path an agent posts its report to.
func ReportPath(page string) string {
	return "/v1/pages/" + url.PathEscape(page) + "/report"
}

// NarrativePath is the path a Narrative Job reads its record from and posts the Narrative to.
func NarrativePath(page string) string {
	return "/v1/pages/" + url.PathEscape(page) + "/narrative"
}

// PageIdentity is the Page a request belongs to. The agent echoes it so that a
// report decoded from the wrong Page is refused rather than applied.
type PageIdentity struct {
	// Name is the Page's name.
	Name string `json:"name"`
	// UID is the Page's UID.
	UID string `json:"uid"`
	// Namespace is the Page's namespace.
	Namespace string `json:"namespace"`
}

// StepEvent reports one step of the investigation. A Finding event also
// carries the Finding, so the server preserves it even if the agent never
// submits a report.
type StepEvent struct {
	// Kind classifies the step.
	Kind v1alpha1.StepKind `json:"kind"`
	// Tool names the tool for a ToolCall step.
	Tool string `json:"tool,omitempty"`
	// Summary is a short description of the step.
	Summary string `json:"summary"`
	// Finding is the recorded Finding, for a Finding step.
	Finding *v1alpha1.Finding `json:"finding,omitempty"`
	// ToolCallID names the tool call the step belongs to, when it has one.
	ToolCallID string `json:"toolCallId,omitempty"`
}

// ReportProposal is the wire form of the one Proposal in a report. The v2 agent
// sends a discriminator and its variant's fields; a report from a v1 agent
// instead sends Action and Parameters, and is still accepted while the fixed
// Action menu is retired.
type ReportProposal struct {
	v1alpha1.ProposedAction `json:",inline"`

	// Kind is FilePatch or DiagnosisOnly for a v2 report.
	Kind string `json:"kind,omitempty"`
	// Files are the whole-file edits of a FilePatch Proposal.
	Files []v1alpha1.FileEdit `json:"files,omitempty"`
	// Confidence records how the edit target was chosen.
	Confidence string `json:"confidence,omitempty"`
}

// Report is the agent's final output: a Diagnosis, its cited Findings, one
// Proposal, the model usage, and the capabilities the endpoint actually had.
type Report struct {
	// Page is the Page the report belongs to. A v2 agent always sends it.
	Page *PageIdentity `json:"page,omitempty"`
	// Diagnosis is the root-cause conclusion.
	Diagnosis string `json:"diagnosis"`
	// Findings are the cited evidence for the Diagnosis.
	Findings []v1alpha1.Finding `json:"findings"`
	// Proposal is the single remediation the agent recommends.
	Proposal ReportProposal `json:"proposal"`
	// Usage is the model usage of the investigation.
	Usage *v1alpha1.Usage `json:"usage,omitempty"`
	// Capabilities records what the endpoint actually supported.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities records what the model endpoint supported during a run, so a
// degraded installation is visible rather than invisible.
type Capabilities struct {
	// StructuredOutput maps each stage to the rung that produced its output.
	StructuredOutput map[string]string `json:"structuredOutput,omitempty"`
	// UsageSource is provider, estimated, or mixed.
	UsageSource string `json:"usageSource,omitempty"`
}

// ValidationError describes one reason a report was refused.
type ValidationError struct {
	// Field is the path of the offending field in the report, such as "proposal.kind".
	Field string `json:"field"`
	// Message explains the problem so that the agent can correct it.
	Message string `json:"message"`
}

// ReportResponse is the server's synchronous answer to a report.
type ReportResponse struct {
	// Accepted reports whether the server accepted the report.
	Accepted bool `json:"accepted"`
	// Phase is the Page's phase after the report, when it was accepted or
	// when it failed Validation. After a refusal, Investigating means the
	// agent may submit one revised Proposal, and Invalid means the second
	// failure ended the Page.
	Phase v1alpha1.Phase `json:"phase,omitempty"`
	// Errors lists the Validation errors, when the report was refused for its content.
	Errors []ValidationError `json:"errors,omitempty"`
}

// ErrorResponse is the body of every other non-success answer.
type ErrorResponse struct {
	// Message explains the failure.
	Message string `json:"message"`
}

// ToolError is the error object a tool call answers with. The code is stable so
// the agent can act on it: out_of_scope, unknown_tool, budget_exhausted, or
// tool_error.
type ToolError struct {
	// Code is the stable machine-readable reason.
	Code string `json:"code"`
	// Message explains the refusal without quoting the evidence.
	Message string `json:"message"`
}

// ToolErrorResponse is the body of a refused tool call.
type ToolErrorResponse struct {
	// OK is false for every refusal.
	OK bool `json:"ok"`
	// Error is the refusal.
	Error ToolError `json:"error"`
}

// NarrativeRecord is what a Narrative Job reads: the Page rendered as text,
// and the Target it belongs to.
type NarrativeRecord struct {
	// Target is the Target that the Page investigated.
	Target v1alpha1.TargetReference `json:"target"`
	// Text describes the Page for the model: its Diagnosis, Findings,
	// Proposal, decision, execution, and Verification checks.
	Text string `json:"text"`
}

// NarrativeReport is the Narrative Job's answer: the short account of a finished Page.
type NarrativeReport struct {
	// Narrative is the account, which the server stores in the Page's status.
	Narrative string `json:"narrative"`
}
