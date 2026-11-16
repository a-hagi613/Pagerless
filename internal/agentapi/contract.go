// Package agentapi defines the contract between an agent Job and
// pagerless-server (ADR-0001): the environment the server gives the Job, the
// step events and the report the agent posts, and the server's answers.
//
// The agent authenticates every request with a projected service account
// token whose audience is Audience. The server answers a report synchronously
// with acceptance or Validation errors.
package agentapi

import (
	"net/url"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Audience is the audience of the projected service account token that an agent presents.
const Audience = "pagerless-server"

// Environment variables that the server sets on the agent Job's container.
const (
	// EnvServerURL is the base URL of pagerless-server's agent API.
	EnvServerURL = "PAGERLESS_SERVER_URL"
	// EnvTokenFile is the path of the projected service account token.
	EnvTokenFile = "PAGERLESS_TOKEN_FILE"
	// EnvTriage is the name of the Triage the agent investigates.
	EnvTriage = "PAGERLESS_TRIAGE"
	// EnvWorkloadKind is the kind of the target Workload.
	EnvWorkloadKind = "PAGERLESS_WORKLOAD_KIND"
	// EnvWorkloadNamespace is the namespace of the target Workload.
	EnvWorkloadNamespace = "PAGERLESS_WORKLOAD_NAMESPACE"
	// EnvWorkloadName is the name of the target Workload.
	EnvWorkloadName = "PAGERLESS_WORKLOAD_NAME"
	// EnvTriggeringPod is the pod that raised the Trigger, when there is one.
	EnvTriggeringPod = "PAGERLESS_TRIGGERING_POD"
	// EnvHint is the human Hint, when there is one.
	EnvHint = "PAGERLESS_HINT"
)

// EventsPath is the path an agent posts step events to.
func EventsPath(triage string) string {
	return "/v1/triages/" + url.PathEscape(triage) + "/events"
}

// ReportPath is the path an agent posts its report to.
func ReportPath(triage string) string {
	return "/v1/triages/" + url.PathEscape(triage) + "/report"
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
}

// Report is the agent's final output: a Diagnosis, its Findings, and one Proposal.
type Report struct {
	// Diagnosis is the root-cause conclusion.
	Diagnosis string `json:"diagnosis"`
	// Findings are the cited evidence for the Diagnosis.
	Findings []v1alpha1.Finding `json:"findings"`
	// Proposal is the single remediation the agent recommends.
	Proposal v1alpha1.ProposedAction `json:"proposal"`
}

// ValidationError describes one reason a report was refused.
type ValidationError struct {
	// Field is the path of the offending field in the report, such as "proposal.action".
	Field string `json:"field"`
	// Message explains the problem so that the agent can correct it.
	Message string `json:"message"`
}

// ReportResponse is the server's synchronous answer to a report.
type ReportResponse struct {
	// Accepted reports whether the server accepted the report.
	Accepted bool `json:"accepted"`
	// Phase is the Triage's phase after the report, when it was accepted.
	Phase v1alpha1.Phase `json:"phase,omitempty"`
	// Errors lists the Validation errors, when the report was refused for its content.
	Errors []ValidationError `json:"errors,omitempty"`
}

// ErrorResponse is the body of every other non-success answer.
type ErrorResponse struct {
	// Message explains the failure.
	Message string `json:"message"`
}
