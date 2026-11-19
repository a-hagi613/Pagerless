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
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase is the stage a Page is in.
// +kubebuilder:validation:Enum=Pending;Investigating;Proposed;Approved;PullRequestOpen;Verifying;Recovered;NotRecovered;Diagnosed;Rejected;Expired;Invalid;Superseded;Failed
type Phase string

const (
	// PhasePending means the Page is accepted and queued; nothing has been read yet.
	PhasePending Phase = "Pending"
	// PhaseInvestigating means the agent Job is running.
	PhaseInvestigating Phase = "Investigating"
	// PhaseProposed means a Proposal passed Validation and awaits a human decision.
	PhaseProposed Phase = "Proposed"
	// PhaseApproved means the operator approved the current Proposal; the pull request is next.
	PhaseApproved Phase = "Approved"
	// PhasePullRequestOpen means the pull request exists and Pagerless is following its Checks.
	PhasePullRequestOpen Phase = "PullRequestOpen"
	// PhaseVerifying means the pull request merged and Verification is observing the Target.
	PhaseVerifying Phase = "Verifying"
	// PhaseRecovered is terminal: Verification found the Target recovered.
	PhaseRecovered Phase = "Recovered"
	// PhaseNotRecovered is terminal: Verification found the Target not recovered.
	PhaseNotRecovered Phase = "NotRecovered"
	// PhaseDiagnosed is terminal: the operator acknowledged a no-change recommendation.
	PhaseDiagnosed Phase = "Diagnosed"
	// PhaseRejected is terminal: the operator rejected the Proposal.
	PhaseRejected Phase = "Rejected"
	// PhaseExpired is terminal: no decision arrived inside the approval window.
	PhaseExpired Phase = "Expired"
	// PhaseInvalid is terminal: two Proposals failed Validation.
	PhaseInvalid Phase = "Invalid"
	// PhaseSuperseded is terminal: another Page is canonical for the Fingerprint.
	PhaseSuperseded Phase = "Superseded"
	// PhaseFailed is terminal: the agent failed, a budget was exhausted, or the pull request was abandoned.
	PhaseFailed Phase = "Failed"
)

// IsTerminal reports whether the phase is a terminal phase.
func (p Phase) IsTerminal() bool {
	switch p {
	case PhaseRecovered, PhaseNotRecovered, PhaseDiagnosed, PhaseRejected,
		PhaseExpired, PhaseInvalid, PhaseSuperseded, PhaseFailed:
		return true
	default:
		return false
	}
}

// IsActive reports whether a Page in this phase is an Active Page. A Page that
// has not yet been given a phase is active.
func (p Phase) IsActive() bool {
	return !p.IsTerminal()
}

// Severity ranks how urgently a Page needs a human. It is a hint for the
// operator and never changes the lifecycle.
// +kubebuilder:validation:Enum=info;warning;critical
type Severity string

const (
	// SeverityInfo marks a Page that can wait.
	SeverityInfo Severity = "info"
	// SeverityWarning marks a Page that needs attention.
	SeverityWarning Severity = "warning"
	// SeverityCritical marks a Page that is losing availability now.
	SeverityCritical Severity = "critical"
)

// Condition types describe a Page in machine-readable form. A Page sets each
// one once and never removes it.
const (
	// ConditionAccepted is True when this Page is the canonical Active Page for its Fingerprint.
	ConditionAccepted = "Accepted"
	// ConditionInvestigated is True when the agent produced a Diagnosis and a Proposal or a no-change
	// recommendation.
	ConditionInvestigated = "Investigated"
	// ConditionDecided is True when an Approval, a Rejection, or an acknowledgement is recorded.
	ConditionDecided = "Decided"
	// ConditionDelivered is True when a pull request exists and its Checks pass.
	ConditionDelivered = "Delivered"
	// ConditionVerified is True when Verification reached a decision.
	ConditionVerified = "Verified"
	// ConditionThrottled is True on the Page that absorbed Triggers which did not open a Page.
	ConditionThrottled = "Throttled"
	// ConditionFailed is True when the Page cannot progress.
	ConditionFailed = "Failed"
)

// Reasons recorded on the conditions.
const (
	// ReasonCanonical means this Page is the canonical Active Page for its Fingerprint.
	ReasonCanonical = "Canonical"
	// ReasonDuplicate means another Page is canonical for the same Fingerprint.
	ReasonDuplicate = "Duplicate"
	// ReasonAgentReported means the agent submitted a report that Validation accepted.
	ReasonAgentReported = "AgentReported"
	// ReasonAgentDeadlineExceeded means the agent Job exceeded its active deadline without a report.
	ReasonAgentDeadlineExceeded = "AgentDeadlineExceeded"
	// ReasonAgentEndedWithoutReport means the agent Job ended without submitting a report.
	ReasonAgentEndedWithoutReport = "AgentEndedWithoutReport"
	// ReasonAgentJobMissing means the agent Job disappeared before the agent reported.
	ReasonAgentJobMissing = "AgentJobMissing"
	// ReasonValidationFailedTwice means the agent's second Proposal also failed Validation.
	ReasonValidationFailedTwice = "ValidationFailedTwice"
	// ReasonStaleProposal means the Proposal's base moved and it could not be re-rendered.
	ReasonStaleProposal = "StaleProposal"
	// ReasonApproved means the operator approved the current Proposal.
	ReasonApproved = "Approved"
	// ReasonRejected means the operator rejected the Proposal.
	ReasonRejected = "Rejected"
	// ReasonAcknowledged means the operator acknowledged a no-change recommendation.
	ReasonAcknowledged = "Acknowledged"
	// ReasonExpired means no decision arrived inside the approval window.
	ReasonExpired = "Expired"
	// ReasonPullRequestOpen means the pull request exists.
	ReasonPullRequestOpen = "PullRequestOpen"
	// ReasonMerged means the pull request merged.
	ReasonMerged = "Merged"
	// ReasonChecksFailing means the Target Repository's Checks failed.
	ReasonChecksFailing = "ChecksFailing"
	// ReasonPullRequestClosed means the pull request was closed without being merged.
	ReasonPullRequestClosed = "PullRequestClosed"
	// ReasonRecovered means Verification found the Target recovered.
	ReasonRecovered = "Recovered"
	// ReasonNotRecovered means Verification found the Target not recovered.
	ReasonNotRecovered = "NotRecovered"
	// ReasonPullRequestPermissionDenied means GitHub refused the branch push or the pull request.
	ReasonPullRequestPermissionDenied = "PullRequestPermissionDenied"
	// ReasonTargetDeleted means the Target disappeared before the Page reached a terminal phase.
	ReasonTargetDeleted = "TargetDeleted"
	// ReasonValidationRefused means a Proposal failed Validation and the agent may submit one revision.
	ReasonValidationRefused = "ValidationRefused"
	// ReasonTargetGenerationChanged means the Target's generation changed after the Proposal was rendered.
	ReasonTargetGenerationChanged = "TargetGenerationChanged"
	// ReasonSourcePathChanged means the Target's pagerless.dev/source-path annotation changed.
	ReasonSourcePathChanged = "SourcePathChanged"
	// ReasonTargetRepositoryFileChanged means a Target Repository file that the patch changes was modified.
	ReasonTargetRepositoryFileChanged = "TargetRepositoryFileChanged"
	// ReasonRollbackCommitChanged means the commit a Rollback Proposal reverts is no longer among the allowed commits.
	ReasonRollbackCommitChanged = "RollbackCommitChanged"
	// ReasonExecutionRefused means the publisher refused to publish the approved Proposal.
	ReasonExecutionRefused = "ExecutionRefused"
)

// MaxValidationAttempts is the number of Proposals an agent may submit for one
// Page. The second Proposal that fails Validation ends the Page as Invalid.
const MaxValidationAttempts = 2

// MaxReasonLength is the longest reason a human may give for a decision.
const MaxReasonLength = 1000

// AnnotationSourcePath is the annotation on a Target that names the Target
// Repository file holding its manifest, relative to the repository root.
const AnnotationSourcePath = "pagerless.dev/source-path"

// AnnotationDependsOnPaths is the annotation on a Target that names the Target
// Repository files it depends on but does not own, such as a shared
// configuration file. The value is a comma-separated list of paths relative to
// the repository root. Rollback may revert a commit that touches one of them.
const AnnotationDependsOnPaths = "pagerless.dev/depends-on-paths"

// Decision annotations. A human decision is recorded by patching the Page's
// annotations, because the standalone chart installs no admission webhook to
// stamp an identity. The server reconciles the annotations into status.decision
// (ADR-0031). Any caller that can patch Pages can record a decision, which is
// the intended single-operator authority model.
const (
	// AnnotationDecision carries "approve" or "reject".
	AnnotationDecision = "pagerless.dev/decision"
	// AnnotationDecisionProposal binds the decision to one Proposal hash.
	AnnotationDecisionProposal = "pagerless.dev/decision-proposal"
	// AnnotationDecisionReason carries the operator's optional reason.
	AnnotationDecisionReason = "pagerless.dev/decision-reason"
	// AnnotationDecidedBy carries the acting identity the caller declares. Its
	// trust is exactly the caller's RBAC on Pages.
	AnnotationDecidedBy = "pagerless.dev/decided-by"
)

// DecisionAnnotationApprove and DecisionAnnotationReject are the values of
// AnnotationDecision.
const (
	DecisionAnnotationApprove = "approve"
	DecisionAnnotationReject  = "reject"
)

// DependsOnPaths returns the Target Repository paths declared in a Target's
// annotations, without duplicates and in sorted order.
func DependsOnPaths(annotations map[string]string) []string {
	var paths []string
	seen := make(map[string]struct{})
	for entry := range strings.SplitSeq(annotations[AnnotationDependsOnPaths], ",") {
		p := strings.TrimSpace(entry)
		if p == "" {
			continue
		}
		if _, duplicate := seen[p]; duplicate {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths
}

// TargetKind is the kind of a Target that a Page can investigate.
// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet;Job;CronJob;Pod;PersistentVolumeClaim;Node
type TargetKind string

const (
	// TargetKindDeployment is a Deployment.
	TargetKindDeployment TargetKind = "Deployment"
	// TargetKindStatefulSet is a StatefulSet.
	TargetKindStatefulSet TargetKind = "StatefulSet"
	// TargetKindDaemonSet is a DaemonSet.
	TargetKindDaemonSet TargetKind = "DaemonSet"
	// TargetKindJob is a Job.
	TargetKindJob TargetKind = "Job"
	// TargetKindCronJob is a CronJob.
	TargetKindCronJob TargetKind = "CronJob"
	// TargetKindPod is a bare Pod.
	TargetKindPod TargetKind = "Pod"
	// TargetKindPersistentVolumeClaim is a PersistentVolumeClaim.
	TargetKindPersistentVolumeClaim TargetKind = "PersistentVolumeClaim"
	// TargetKindNode is a Node. It is the only cluster-scoped kind, so it is the
	// only kind whose Target may have an empty namespace.
	TargetKindNode TargetKind = "Node"
)

// IsClusterScoped reports whether the kind lives outside a namespace.
func (k TargetKind) IsClusterScoped() bool { return k == TargetKindNode }

// TargetReference names the failing object. The UID makes a recreated object a
// new failure rather than a continuation of the old one.
// +kubebuilder:validation:XValidation:rule="self.kind == 'Node' ? (!has(self.namespace) || size(self.namespace) == 0) : (has(self.namespace) && size(self.namespace) > 0)",message="namespace must be set for a namespaced kind and empty for Node"
type TargetReference struct {
	// APIVersion is the Target's API version, such as apps/v1.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	APIVersion string `json:"apiVersion,omitempty"`

	// Kind is the Target's kind.
	// +required
	Kind TargetKind `json:"kind"`

	// Namespace is the Target's namespace. It is empty only for a Node.
	// +required
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// Name is the Target's name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UID is the Target's UID. It distinguishes a recreated object from the
	// object it replaced.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F-]{36}$`
	UID string `json:"uid,omitempty"`
}

// TriggerSource identifies what opened or joined a Page.
// +kubebuilder:validation:Enum=Event;Condition;Alertmanager;Grafana;Datadog;SNS;CLI;Portal;API
type TriggerSource string

const (
	// TriggerSourceEvent is a Kubernetes Warning Event.
	TriggerSourceEvent TriggerSource = "Event"
	// TriggerSourceCondition is a state transition in a Target's status.
	TriggerSourceCondition TriggerSource = "Condition"
	// TriggerSourceAlertmanager is an opted-in Alertmanager alert.
	TriggerSourceAlertmanager TriggerSource = "Alertmanager"
	// TriggerSourceGrafana is a Grafana Alerting alert.
	TriggerSourceGrafana TriggerSource = "Grafana"
	// TriggerSourceDatadog is a Datadog alert.
	TriggerSourceDatadog TriggerSource = "Datadog"
	// TriggerSourceSNS is an AWS SNS or CloudWatch alert.
	TriggerSourceSNS TriggerSource = "SNS"
	// TriggerSourceCLI is a human request through kubectl page.
	TriggerSourceCLI TriggerSource = "CLI"
	// TriggerSourcePortal is a human request through the Portal.
	TriggerSourcePortal TriggerSource = "Portal"
	// TriggerSourceAPI is a request through the server's HTTP API.
	TriggerSourceAPI TriggerSource = "API"
)

// Trigger records the observation that opened or joined a Page. Later Triggers
// accumulate in status.triggerCount, so the spec keeps the first one verbatim.
type Trigger struct {
	// Source is what opened or joined the Page.
	// +required
	Source TriggerSource `json:"source"`

	// Reason is the machine-readable cause, such as CrashLoopBackOff.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Reason string `json:"reason,omitempty"`

	// Message is the human-readable text at first observation.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`

	// Pod is the pod that raised the Trigger. It is evidence, not the Target.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Pod string `json:"pod,omitempty"`

	// AlertFingerprint is the upstream alert fingerprint, when one exists.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	AlertFingerprint string `json:"alertFingerprint,omitempty"`
}

// RepositoryReference is the declared Target Repository. Resolution fills
// status.repository.
type RepositoryReference struct {
	// URL is the HTTPS Git URL, for example https://github.com/acme/gitops.
	// +optional
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=500
	URL string `json:"url,omitempty"`

	// Branch is the branch a Proposal targets. It defaults to main.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Branch string `json:"branch,omitempty"`

	// Path is the Target's manifest path inside the repository, relative to its root.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Path string `json:"path,omitempty"`
}

// PageSpec describes what to investigate. It is immutable after creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type PageSpec struct {
	// Target is the failing object.
	// +required
	Target TargetReference `json:"target"`

	// Fingerprint is the stable failure identity. It deliberately excludes
	// messages, timestamps, and repetition counts, so that one crash loop is
	// one failure rather than hundreds.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	Fingerprint string `json:"fingerprint"`

	// Trigger is the observation that opened the Page.
	// +required
	Trigger Trigger `json:"trigger"`

	// Hint is optional guidance from the operator, passed to the agent as human input.
	// +optional
	// +kubebuilder:validation:MaxLength=1000
	Hint string `json:"hint,omitempty"`

	// Repository is the declared Target Repository. Empty lets resolution decide.
	// +optional
	Repository *RepositoryReference `json:"repository,omitempty"`

	// PreviousPage links a Recurrence to the Page that recorded the previous outcome.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PreviousPage string `json:"previousPage,omitempty"`
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

// MaxSteps is the number of most recent steps kept in a Page's status.
const MaxSteps = 50

// MaxFindings is the number of Findings kept in a Page's status.
const MaxFindings = 20

// MaxNarrativeLength is the longest Narrative that a Page stores.
const MaxNarrativeLength = 4000

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
	// FindingKindCommit is a Target Repository commit.
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

// Decision is the recorded human decision on a Proposal. Only pagerless-server
// writes it, from the decision annotations (ADR-0031).
type Decision struct {
	// Outcome is the decision.
	// +required
	Outcome DecisionOutcome `json:"outcome"`

	// ProposalHash binds the decision to one Proposal.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	ProposalHash string `json:"proposalHash"`

	// DecidedBy is the identity the caller declared. Its trust is the caller's
	// RBAC on Pages, because the standalone chart installs no admission webhook
	// to stamp an authentic identity.
	// +required
	// +kubebuilder:validation:MaxLength=253
	DecidedBy string `json:"decidedBy"`

	// Reason is the optional reason the human gave.
	// +optional
	// +kubebuilder:validation:MaxLength=1000
	Reason string `json:"reason,omitempty"`

	// DecidedAt is when the decision was made.
	// +required
	DecidedAt metav1.Time `json:"decidedAt"`
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

// Usage is the model usage of one Page's investigation, as the agent reported
// it. Every field is bounded so that a compromised agent cannot inflate Page
// status or the exported metrics.
type Usage struct {
	// InputTokens counts every input token the investigation's model calls read, including cached tokens.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10000000
	InputTokens int64 `json:"inputTokens"`

	// OutputTokens counts every output token the investigation's model calls wrote.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	OutputTokens int64 `json:"outputTokens"`

	// CostUSD is the estimated cost of the model calls in United States dollars.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	CostUSD float64 `json:"costUSD"`
}

// PullRequestState is the provider's view of the pull request.
// +kubebuilder:validation:Enum=Draft;Open;Merged;Closed
type PullRequestState string

const (
	// PullRequestDraft is an open draft pull request.
	PullRequestDraft PullRequestState = "Draft"
	// PullRequestOpen is an open pull request awaiting review and merge.
	PullRequestOpen PullRequestState = "Open"
	// PullRequestMerged is a merged pull request.
	PullRequestMerged PullRequestState = "Merged"
	// PullRequestClosed is a pull request closed without merging.
	PullRequestClosed PullRequestState = "Closed"
)

// ChecksState is the aggregate state of the Target Repository's CI on the pull request.
// +kubebuilder:validation:Enum=Pending;Passing;Failing;Unknown
type ChecksState string

const (
	// ChecksPending means the repository's Checks have not finished.
	ChecksPending ChecksState = "Pending"
	// ChecksPassing means every Check passed.
	ChecksPassing ChecksState = "Passing"
	// ChecksFailing means at least one Check failed.
	ChecksFailing ChecksState = "Failing"
	// ChecksUnknown means no Check state is available.
	ChecksUnknown ChecksState = "Unknown"
)

// PullRequest records the pull request that carries an approved Proposal.
type PullRequest struct {
	// Provider is the code host. It is GitHub in v2.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Provider string `json:"provider,omitempty"`

	// Number is the pull request number.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Number int32 `json:"number,omitempty"`

	// URL is the address of the pull request page.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	URL string `json:"url,omitempty"`

	// Branch is the branch that carries the Proposal.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Branch string `json:"branch,omitempty"`

	// HeadSHA is the commit the branch points at.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	HeadSHA string `json:"headSHA,omitempty"`

	// State is the provider's view of the pull request.
	// +optional
	State PullRequestState `json:"state,omitempty"`

	// Checks is the aggregate state of the repository's CI.
	// +optional
	Checks ChecksState `json:"checks,omitempty"`

	// MergeCommit is the commit the merge produced.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	MergeCommit string `json:"mergeCommit,omitempty"`

	// OpenedAt is when the pull request was opened.
	// +optional
	OpenedAt *metav1.Time `json:"openedAt,omitempty"`

	// MergedAt is when the pull request merged.
	// +optional
	MergedAt *metav1.Time `json:"mergedAt,omitempty"`
}

// RepositoryResolution records how the Target Repository was found. A weak
// method is visible to the operator and blocks unattended pull requests.
type RepositoryResolution struct {
	// URL is the resolved repository URL.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	URL string `json:"url,omitempty"`

	// Branch is the resolved branch.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Branch string `json:"branch,omitempty"`

	// Path is the Target's manifest path inside the repository.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Path string `json:"path,omitempty"`

	// Method names how the repository was found, such as TrackingID or NameMatch.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Method string `json:"method,omitempty"`

	// Confidence is exact for a provenance method and heuristic otherwise.
	// +optional
	// +kubebuilder:validation:Enum=Exact;Heuristic
	Confidence string `json:"confidence,omitempty"`

	// BaseCommit is the commit a Proposal was rendered against.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	BaseCommit string `json:"baseCommit,omitempty"`

	// ResolvedAt is when resolution ran.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`
}

// CooldownStatus is the recurrence bookkeeping that outlives the investigation.
type CooldownStatus struct {
	// LastPagedAt is when this Page opened.
	// +optional
	LastPagedAt *metav1.Time `json:"lastPagedAt,omitempty"`

	// NextEligibleAt is when the same Fingerprint may open a new Page.
	// +optional
	NextEligibleAt *metav1.Time `json:"nextEligibleAt,omitempty"`

	// RecurrenceCount is the number of Pages recorded for this Fingerprint, including this one.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RecurrenceCount int32 `json:"recurrenceCount,omitempty"`
}

// PageStatus is the observed state of a Page. Only pagerless-server writes it (ADR-0001).
type PageStatus struct {
	// Phase is the stage the Page is in.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// ObservedGeneration is the Page generation that this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Severity ranks how urgently the Page needs a human.
	// +optional
	Severity Severity `json:"severity,omitempty"`

	// StartedAt is when pagerless-server put the Page in the Pending phase. It is
	// the start of the time to Recovered that Pagerless exports.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// TerminalAt is when the Page reached its terminal phase. The retention
	// controller deletes the Page after the configured retention period.
	// +optional
	TerminalAt *metav1.Time `json:"terminalAt,omitempty"`

	// Conditions describe the Page's state in machine-readable form.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// TriggerCount is the number of Triggers that opened or joined the Page.
	// +optional
	TriggerCount int32 `json:"triggerCount,omitempty"`

	// LastTriggerAt is when the most recent Trigger joined the Page.
	// +optional
	LastTriggerAt *metav1.Time `json:"lastTriggerAt,omitempty"`

	// SuppressedCount is the number of Triggers that did not open a Page because
	// this Page already covered their Fingerprint.
	// +optional
	SuppressedCount int32 `json:"suppressedCount,omitempty"`

	// Cooldown records when the same Fingerprint may open a new Page.
	// +optional
	Cooldown *CooldownStatus `json:"cooldown,omitempty"`

	// Repository is the resolved Target Repository.
	// +optional
	Repository *RepositoryResolution `json:"repository,omitempty"`

	// AgentJobName is the name of the agent Job.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	AgentJobName string `json:"agentJobName,omitempty"`

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

	// Usage is the model usage of the investigation, as the agent reported it in its accepted report.
	// +optional
	Usage *Usage `json:"usage,omitempty"`

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

	// PullRequest is the pull request that carries the approved Proposal.
	// +optional
	PullRequest *PullRequest `json:"pullRequest,omitempty"`

	// VerificationStartedAt is when the observation window began: the moment
	// Pagerless saw the pull request merge.
	// +optional
	VerificationStartedAt *metav1.Time `json:"verificationStartedAt,omitempty"`

	// VerificationChecks are the results of the deterministic Verification checks.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +listType=atomic
	VerificationChecks []VerificationCheck `json:"verificationChecks,omitempty"`

	// NarrativeJobName is the name of the Job that writes the Narrative after Verification.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	NarrativeJobName string `json:"narrativeJobName,omitempty"`

	// Narrative is the short, purely descriptive account of the Page written after Verification. It never
	// affects the outcome.
	// +optional
	// +kubebuilder:validation:MaxLength=4000
	Narrative string `json:"narrative,omitempty"`
}

// Page is one investigation of one failing Target, from Trigger to verified outcome.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pg,categories=pagerless
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.spec.trigger.source`
// +kubebuilder:printcolumn:name="PR",type=integer,JSONPath=`.status.pullRequest.number`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Kind",type=string,priority=1,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Namespace",type=string,priority=1,JSONPath=`.spec.target.namespace`
type Page struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec PageSpec `json:"spec"`

	// +optional
	Status PageStatus `json:"status,omitzero"`
}

// PageList is a list of Pages.
// +kubebuilder:object:root=true
type PageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Page `json:"items"`
}
