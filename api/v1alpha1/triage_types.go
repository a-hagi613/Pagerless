/*
Copyright 2026 Abdullahi Hagi.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase is the stage a Triage is in.
// +kubebuilder:validation:Enum=Pending;Investigating;Proposed;Approved;Executing;Verifying;Recovered;NotRecovered;Diagnosed;Rejected;Expired;Invalid;Failed
type Phase string

const (
	// PhasePending is the phase of a Triage that the server has accepted but not yet started.
	PhasePending Phase = "Pending"
	// PhaseInvestigating means the agent Job is running.
	PhaseInvestigating Phase = "Investigating"
	// PhaseProposed means a Proposal passed Validation and awaits a human decision.
	PhaseProposed Phase = "Proposed"
	// PhaseApproved means a human approved the current Proposal.
	PhaseApproved Phase = "Approved"
	// PhaseExecuting means the Executor is carrying out the approved Proposal.
	PhaseExecuting Phase = "Executing"
	// PhaseVerifying means Verification is running after execution.
	PhaseVerifying Phase = "Verifying"
	// PhaseRecovered is terminal: Verification found the Workload recovered.
	PhaseRecovered Phase = "Recovered"
	// PhaseNotRecovered is terminal: Verification found the Workload not recovered.
	PhaseNotRecovered Phase = "NotRecovered"
	// PhaseDiagnosed is terminal: a NoAction Proposal was acknowledged.
	PhaseDiagnosed Phase = "Diagnosed"
	// PhaseRejected is terminal: a human rejected the Proposal.
	PhaseRejected Phase = "Rejected"
	// PhaseExpired is terminal: no decision arrived within one hour.
	PhaseExpired Phase = "Expired"
	// PhaseInvalid is terminal: the Proposal failed Validation twice.
	PhaseInvalid Phase = "Invalid"
	// PhaseFailed is terminal: the agent errored or exhausted its budget.
	PhaseFailed Phase = "Failed"
)

// IsTerminal reports whether the phase is a terminal phase.
func (p Phase) IsTerminal() bool {
	switch p {
	case PhaseRecovered, PhaseNotRecovered, PhaseDiagnosed, PhaseRejected,
		PhaseExpired, PhaseInvalid, PhaseFailed:
		return true
	default:
		return false
	}
}

// IsActive reports whether a Triage in this phase is an Active Triage. A
// Triage that has not yet been given a phase is active.
func (p Phase) IsActive() bool {
	return !p.IsTerminal()
}

// ConditionFailed is the condition type set on a Triage that ended in the Failed phase.
const ConditionFailed = "Failed"

// Reasons recorded on the Failed condition.
const (
	// ReasonAgentDeadlineExceeded means the agent Job exceeded its active deadline without a report.
	ReasonAgentDeadlineExceeded = "AgentDeadlineExceeded"
	// ReasonAgentEndedWithoutReport means the agent Job ended without submitting a report.
	ReasonAgentEndedWithoutReport = "AgentEndedWithoutReport"
	// ReasonAgentJobMissing means the agent Job disappeared before the agent reported.
	ReasonAgentJobMissing = "AgentJobMissing"
)

// Condition types and reasons that Validation and the Proposal lifecycle record.
const (
	// ConditionInvalid is set on a Triage that ended in the Invalid phase.
	ConditionInvalid = "Invalid"
	// ConditionProposalRevalidated records that a Stale Proposal was re-validated under a new hash.
	ConditionProposalRevalidated = "ProposalRevalidated"

	// ReasonValidationFailedTwice means the agent's second Proposal also failed Validation.
	ReasonValidationFailedTwice = "ValidationFailedTwice"
	// ReasonStaleProposalInvalid means a Stale Proposal failed re-validation.
	ReasonStaleProposalInvalid = "StaleProposalInvalid"

	// ReasonWorkloadGenerationChanged means the Workload's generation changed after the Proposal was rendered.
	ReasonWorkloadGenerationChanged = "WorkloadGenerationChanged"
	// ReasonSourcePathChanged means the Workload's pagerless.dev/source-path annotation changed.
	ReasonSourcePathChanged = "SourcePathChanged"
	// ReasonSandboxFileChanged means the Sandbox file that the patch changes was modified.
	ReasonSandboxFileChanged = "SandboxFileChanged"
	// ReasonWorkloadDeleted means the Workload no longer exists.
	ReasonWorkloadDeleted = "WorkloadDeleted"
)

// MaxValidationAttempts is the number of Proposals an agent may submit for one
// Triage. The second Proposal that fails Validation ends the Triage as Invalid.
const MaxValidationAttempts = 2

// AnnotationSourcePath is the annotation on a Workload that names the Sandbox
// file holding its manifest, relative to the root of the Sandbox.
const AnnotationSourcePath = "pagerless.dev/source-path"

// WorkloadKind is the kind of a Workload that a Triage can target.
// +kubebuilder:validation:Enum=Deployment;StatefulSet
type WorkloadKind string

const (
	// WorkloadKindDeployment is a Deployment.
	WorkloadKindDeployment WorkloadKind = "Deployment"
	// WorkloadKindStatefulSet is a StatefulSet.
	WorkloadKindStatefulSet WorkloadKind = "StatefulSet"
)

// WorkloadReference names the Workload a Triage investigates.
type WorkloadReference struct {
	// Kind is the Workload's kind.
	// +required
	Kind WorkloadKind `json:"kind"`

	// Namespace is the Workload's namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// Name is the Workload's name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// TriggerSource identifies what started a Triage.
// +kubebuilder:validation:Enum=Alertmanager;CLI;Dashboard
type TriggerSource string

const (
	// TriggerSourceAlertmanager is an opted-in Alertmanager alert.
	TriggerSourceAlertmanager TriggerSource = "Alertmanager"
	// TriggerSourceCLI is a human request through kubectl triage.
	TriggerSourceCLI TriggerSource = "CLI"
	// TriggerSourceDashboard is a human request through the dashboard.
	TriggerSourceDashboard TriggerSource = "Dashboard"
)

// Trigger records the event that started a Triage.
type Trigger struct {
	// Source is what started the Triage.
	// +required
	Source TriggerSource `json:"source"`

	// Pod is the pod that raised the Trigger. It is evidence, not the target.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Pod string `json:"pod,omitempty"`

	// AlertFingerprint is the Alertmanager fingerprint of the triggering alert.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	AlertFingerprint string `json:"alertFingerprint,omitempty"`
}

// TriageSpec describes what to investigate. It is immutable after creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type TriageSpec struct {
	// Workload is the Workload to investigate.
	// +required
	Workload WorkloadReference `json:"workload"`

	// Hint is optional guidance from a human, passed to the agent as human input.
	// +optional
	// +kubebuilder:validation:MaxLength=1000
	Hint string `json:"hint,omitempty"`

	// Trigger records what started the Triage.
	// +required
	Trigger Trigger `json:"trigger"`

	// FollowsTriage names the Triage that this Follow-up Triage follows.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	FollowsTriage string `json:"followsTriage,omitempty"`
}

// StepKind classifies a step event reported by the agent.
// +kubebuilder:validation:Enum=ToolCall;Finding;Progress
type StepKind string

const (
	// StepKindToolCall records one tool call.
	StepKindToolCall StepKind = "ToolCall"
	// StepKindFinding records that the agent recorded a Finding.
	StepKindFinding StepKind = "Finding"
	// StepKindProgress records a stage change in the agent's graph.
	StepKindProgress StepKind = "Progress"
)

// MaxSteps is the number of most recent steps kept in a Triage's status.
const MaxSteps = 50

// MaxFindings is the number of Findings kept in a Triage's status.
const MaxFindings = 20

// Step is one entry of the bounded step summary.
type Step struct {
	// Time is when the server recorded the step.
	// +required
	Time metav1.Time `json:"time"`

	// Kind classifies the step.
	// +required
	Kind StepKind `json:"kind"`

	// Tool names the tool for a ToolCall step.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Tool string `json:"tool,omitempty"`

	// Summary is a short description of the step.
	// +required
	// +kubebuilder:validation:MaxLength=300
	Summary string `json:"summary"`
}

// FindingKind classifies the evidence behind a Finding.
// +kubebuilder:validation:Enum=Log;Metric;Trace;Event;Commit
type FindingKind string

const (
	// FindingKindLog is a log line.
	FindingKindLog FindingKind = "Log"
	// FindingKindMetric is a metric.
	FindingKindMetric FindingKind = "Metric"
	// FindingKindTrace is a trace.
	FindingKindTrace FindingKind = "Trace"
	// FindingKindEvent is a Kubernetes event.
	FindingKindEvent FindingKind = "Event"
	// FindingKindCommit is a Sandbox commit.
	FindingKindCommit FindingKind = "Commit"
)

// Finding is one cited piece of evidence that supports a Diagnosis.
type Finding struct {
	// Kind classifies the evidence.
	// +required
	Kind FindingKind `json:"kind"`

	// Source cites where the evidence came from, such as a tool call and its query.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=500
	Source string `json:"source"`

	// Summary states what the evidence shows.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1000
	Summary string `json:"summary"`
}

// DecisionOutcome is the result of a human decision.
// +kubebuilder:validation:Enum=Approved;Rejected
type DecisionOutcome string

const (
	// DecisionApproved records an Approval.
	DecisionApproved DecisionOutcome = "Approved"
	// DecisionRejected records a Rejection.
	DecisionRejected DecisionOutcome = "Rejected"
)

// Decision records the human decision on a Proposal.
type Decision struct {
	// Outcome is the decision.
	// +required
	Outcome DecisionOutcome `json:"outcome"`

	// Approver is the identity of the requester, stamped by an admission webhook.
	// +required
	// +kubebuilder:validation:MaxLength=253
	Approver string `json:"approver"`

	// Reason is the optional reason the human gave.
	// +optional
	// +kubebuilder:validation:MaxLength=1000
	Reason string `json:"reason,omitempty"`

	// DecidedAt is when the decision was made.
	// +required
	DecidedAt metav1.Time `json:"decidedAt"`
}

// Execution records the result of executing a Proposal.
type Execution struct {
	// PullRequestURL is the pull request the Executor opened, for pull request Actions.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	PullRequestURL string `json:"pullRequestURL,omitempty"`

	// MergeCommit is the commit that merged the pull request.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	MergeCommit string `json:"mergeCommit,omitempty"`

	// RestartedPods lists the pods the Executor deleted, for RestartWorkload.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	// +listType=atomic
	RestartedPods []string `json:"restartedPods,omitempty"`

	// ExecutedAt is when the Executor finished.
	// +optional
	ExecutedAt *metav1.Time `json:"executedAt,omitempty"`
}

// VerificationCheck is the result of one deterministic Verification check.
type VerificationCheck struct {
	// Name identifies the check.
	// +required
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// Passed reports whether the check passed.
	// +required
	Passed bool `json:"passed"`

	// Message explains the result.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Message string `json:"message,omitempty"`
}

// TriageStatus is the observed state of a Triage. Only pagerless-server writes it (ADR-0001).
type TriageStatus struct {
	// Phase is the stage the Triage is in.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// Conditions describe the Triage's state in machine-readable form.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// TriggerCount is the number of Triggers that started or joined the Triage.
	// +optional
	TriggerCount int32 `json:"triggerCount,omitempty"`

	// JobName is the name of the agent Job.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	JobName string `json:"jobName,omitempty"`

	// Steps is the bounded summary of the agent's most recent steps.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +listType=atomic
	Steps []Step `json:"steps,omitempty"`

	// StepCount is the total number of steps the agent reported.
	// +optional
	StepCount int32 `json:"stepCount,omitempty"`

	// ToolCallCount is the number of tool calls the agent reported.
	// +optional
	ToolCallCount int32 `json:"toolCallCount,omitempty"`

	// Findings are the cited evidence the agent reported.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +listType=atomic
	Findings []Finding `json:"findings,omitempty"`

	// Diagnosis is the agent's root-cause conclusion.
	// +optional
	// +kubebuilder:validation:MaxLength=4000
	Diagnosis string `json:"diagnosis,omitempty"`

	// ValidationAttempts is the number of Proposals from the agent that failed Validation.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2
	ValidationAttempts int32 `json:"validationAttempts,omitempty"`

	// Proposal is the current Proposal.
	// +optional
	Proposal *Proposal `json:"proposal,omitempty"`

	// Decision is the human decision on the current Proposal.
	// +optional
	Decision *Decision `json:"decision,omitempty"`

	// Execution is the result of executing the Proposal.
	// +optional
	Execution *Execution `json:"execution,omitempty"`

	// VerificationChecks are the results of the deterministic Verification checks.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +listType=atomic
	VerificationChecks []VerificationCheck `json:"verificationChecks,omitempty"`

	// FollowUpTriage names the Follow-up Triage started after a NotRecovered outcome.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	FollowUpTriage string `json:"followUpTriage,omitempty"`
}

// Triage is one investigation of one failing Workload, from Trigger to verified outcome.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=trg
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.workload.namespace`
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workload.name`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.status.proposal.action`
// +kubebuilder:printcolumn:name="Triggers",type=integer,priority=1,JSONPath=`.status.triggerCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Triage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TriageSpec `json:"spec"`

	// +optional
	Status TriageStatus `json:"status,omitzero"`
}

// TriageList is a list of Triages.
// +kubebuilder:object:root=true
type TriageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Triage `json:"items"`
}
