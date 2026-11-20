// Package tools is the server-side, read-only tool broker that the agent Job
// speaks to (spec §5). One GET returns the investigation manifest — the Context
// Bundle, the tool catalogue, the enforced scope, and the budgets — and each
// POST returns {ok, data, meta} with typed JSON data. There is no write tool.
package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Error codes a refused tool call answers with.
const (
	// CodeOutOfScope means the read would leave the Page's namespace, Target
	// Repository, connector set, or permitted kinds.
	CodeOutOfScope = "out_of_scope"
	// CodeUnknownTool means the name is not in the catalogue.
	CodeUnknownTool = "unknown_tool"
	// CodeBudgetExhausted means the investigation has spent its tool-call or byte budget.
	CodeBudgetExhausted = "budget_exhausted"
	// CodeInvalidArguments means the arguments do not match the tool's schema.
	CodeInvalidArguments = "invalid_arguments"
	// CodeToolError means the tool ran and could not answer.
	CodeToolError = "tool_error"
)

// Page is the Page a request belongs to.
type Page struct {
	// Name is the Page's name.
	Name string `json:"name"`
	// UID is the Page's UID.
	UID string `json:"uid"`
	// Namespace is the Page's namespace.
	Namespace string `json:"namespace"`
}

// Target is the object under investigation. A Node Target is cluster-scoped and
// has no namespace.
type Target struct {
	// Kind is the Target's kind.
	Kind string `json:"kind"`
	// Name is the Target's name.
	Name string `json:"name"`
	// Namespace is the Target's namespace, empty for a Node.
	Namespace string `json:"namespace,omitempty"`
	// UID is the Target's UID, when it has one.
	UID string `json:"uid,omitempty"`
	// Generation is the Target's metadata.generation, when it has one.
	Generation *int64 `json:"generation,omitempty"`
}

// Trigger is what opened the Page.
type Trigger struct {
	// Kind is the Trigger source.
	Kind string `json:"kind"`
	// Reason is the machine-readable cause.
	Reason string `json:"reason,omitempty"`
	// Pod is the pod that raised the Trigger.
	Pod string `json:"pod,omitempty"`
}

// Repository is the one Target Repository a Proposal may change.
type Repository struct {
	// Owner is the GitHub account or organisation.
	Owner string `json:"owner"`
	// Name is the repository name.
	Name string `json:"name"`
	// Ref is the branch the Proposal targets.
	Ref string `json:"ref"`
}

// Scope is what the server will allow. Every tool enforces it server-side; the
// model sees it only so that it does not waste calls on refusals.
type Scope struct {
	// Namespaces are the namespaces reads may touch.
	Namespaces []string `json:"namespaces"`
	// Repositories are the Target Repositories reads may touch.
	Repositories []Repository `json:"repositories"`
	// Connectors are the configured Connector names reads may use.
	Connectors []string `json:"connectors"`
	// Kinds are the object kinds reads may name.
	Kinds []string `json:"kinds"`
}

// Limits are the four budgets of one investigation.
type Limits struct {
	// ToolCalls is the number of tool calls the investigation may make.
	ToolCalls int `json:"toolCalls"`
	// ModelCalls is the number of model calls the investigation may make.
	ModelCalls int `json:"modelCalls"`
	// InputTokens is the input-token budget.
	InputTokens int64 `json:"inputTokens"`
	// Seconds is the wall-clock budget.
	Seconds float64 `json:"seconds"`
}

// ToolSpec is one catalogue entry, forwarded to the model verbatim.
type ToolSpec struct {
	// Name is the tool's stable name.
	Name string `json:"name"`
	// Description states what the tool reads and what it refuses.
	Description string `json:"description"`
	// InputSchema is the JSON Schema of the tool's arguments. The name is
	// snake_case because the agent's contract validates the manifest strictly
	// with extra="forbid"; a camelCase key here fails every investigation
	// before it starts.
	InputSchema map[string]any `json:"input_schema"`
}

// Capabilities records what the server supports beyond the catalogue.
type Capabilities struct {
	// Envelope is the name of the untrusted-data envelope the agent wraps results in.
	Envelope string `json:"envelope,omitempty"`
	// Evidence is the durability of the stored tool results: history or memory.
	Evidence string `json:"evidence,omitempty"`
}

// Manifest is everything the agent needs before its first model call: the
// Context Bundle, the catalogue, the scope, and the limits.
type Manifest struct {
	// Page identifies the Page. The agent refuses a manifest for another UID.
	Page Page `json:"page"`
	// Target is the object under investigation.
	Target Target `json:"target"`
	// Trigger is what opened the Page.
	Trigger *Trigger `json:"trigger,omitempty"`
	// Scope is the enforced scope.
	Scope Scope `json:"scope"`
	// Context is the sanitized Context Bundle.
	Context json.RawMessage `json:"context"`
	// Tools is the tool catalogue.
	Tools []ToolSpec `json:"tools"`
	// Limits are the budgets the agent enforces.
	Limits Limits `json:"limits"`
	// Hint is the human Hint, when there is one.
	Hint string `json:"hint,omitempty"`
	// Capabilities records what the server supports.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Meta is the metadata of one tool result.
type Meta struct {
	// CallID identifies the call, and is what a Finding cites.
	CallID string `json:"callId"`
	// Tool is the tool that ran.
	Tool string `json:"tool"`
	// Bytes is the size of the data before the envelope.
	Bytes int `json:"bytes"`
	// Redactions is the number of secret-shaped matches removed.
	Redactions int `json:"redactions"`
	// Truncated reports that a bound cut the result.
	Truncated bool `json:"truncated,omitempty"`
}

// Result is the body of a successful tool call.
type Result struct {
	// OK is true when the tool read something.
	OK bool `json:"ok"`
	// Data is the typed JSON result. It is never free text.
	Data any `json:"data"`
	// Meta describes the call.
	Meta Meta `json:"meta"`
}

// Refusal is the body of a refused tool call.
type Refusal struct {
	// OK is false.
	OK bool `json:"ok"`
	// Error names the refusal.
	Error RefusalError `json:"error"`
}

// RefusalError is one machine-readable refusal.
type RefusalError struct {
	// Code is the stable reason.
	Code string `json:"code"`
	// Message explains the refusal without quoting evidence.
	Message string `json:"message"`
}

// Record is the stored result of one tool call. It is what a Finding's citation
// is verified against.
type Record struct {
	// PageUID is the Page the call belongs to.
	PageUID string
	// CallID is the identifier a Finding cites.
	CallID string
	// Tool is the catalogue name that ran.
	Tool string
	// Arguments is the bounded rendering of the model's arguments.
	Arguments string
	// Data is the redacted JSON the model saw.
	Data json.RawMessage
	// Outcome is ok, out_of_scope, unknown_tool, budget_exhausted, or tool_error.
	Outcome string
	// Bytes is the size of Data.
	Bytes int64
	// Redactions is the number of matches removed from Data.
	Redactions int
	// At is when the call was served.
	At time.Time
}

// Store is the durable record of the calls the broker served. The server
// verifies every Finding citation against it, so a fabricated citation can never
// reach a Page.
type Store interface {
	// Record stores one call.
	Record(ctx context.Context, record Record) error
	// Get returns the stored call. found is false when the Page never made it.
	Get(ctx context.Context, pageUID, callID string) (Record, bool, error)
	// Served returns the number of tool calls the Page has made.
	Served(ctx context.Context, pageUID string) (int, error)
	// Bytes returns the bytes of results the Page has been served.
	Bytes(ctx context.Context, pageUID string) (int64, error)
	// Refusals increments and returns the number of times the Page asked for a
	// name outside the catalogue.
	Refusals(ctx context.Context, pageUID, tool string) (int, error)
}

// Sink receives the Page-level record of each call, so the Page can show an
// operator what was read. It is implemented by the server's status store.
type Sink interface {
	// RecordToolCall appends one call record and its redaction count to the Page.
	RecordToolCall(ctx context.Context, page *v1alpha1.Page, record Record) error
	// RecordSuspiciousOutput records one SuspiciousOutput event on the Page.
	RecordSuspiciousOutput(ctx context.Context, page *v1alpha1.Page, event string) error
}
