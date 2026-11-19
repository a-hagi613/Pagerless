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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Action is one kind of remediation that a Proposal can recommend. Every
// remaining Action is delivered as a pull request: Pagerless never mutates the
// cluster.
// +kubebuilder:validation:Enum=Scale;PatchResources;Rollback;NoAction
type Action string

const (
	// ActionScale changes the Target's replica count through a pull request.
	ActionScale Action = "Scale"
	// ActionPatchResources changes a container's requests and limits through a pull request.
	ActionPatchResources Action = "PatchResources"
	// ActionRollback reverts one Target Repository commit through a pull request.
	ActionRollback Action = "Rollback"
	// ActionNoAction records a Diagnosis without a remediation.
	ActionNoAction Action = "NoAction"
)

// DeliveredByPullRequest reports whether the remediation reaches the cluster as
// a pull request against the Target Repository. NoAction changes nothing.
func (a Action) DeliveredByPullRequest() bool {
	switch a {
	case ActionScale, ActionPatchResources, ActionRollback:
		return true
	default:
		return false
	}
}

// ScaleParameters are the parameters of the Scale Action.
type ScaleParameters struct {
	// Replicas is the desired replica count.
	// +required
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
}

// PatchResourcesParameters are the parameters of the PatchResources Action.
type PatchResourcesParameters struct {
	// Container names the container to patch.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Container string `json:"container"`

	// Requests are the desired resource requests.
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`

	// Limits are the desired resource limits.
	// +optional
	Limits corev1.ResourceList `json:"limits,omitempty"`
}

// RollbackParameters are the parameters of the Rollback Action.
type RollbackParameters struct {
	// Commit is the Target Repository commit to revert. It must be among the last
	// 20 commits touching the Target's path or a declared dependency path.
	// +required
	// +kubebuilder:validation:MinLength=7
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]+$`
	Commit string `json:"commit"`
}

// ActionParameters holds the typed parameters of an Action. At most one field is set, and it must match the Action.
// NoAction has no parameters.
// +kubebuilder:validation:MaxProperties=1
type ActionParameters struct {
	// Scale holds the parameters of the Scale Action.
	// +optional
	Scale *ScaleParameters `json:"scale,omitempty"`

	// PatchResources holds the parameters of the PatchResources Action.
	// +optional
	PatchResources *PatchResourcesParameters `json:"patchResources,omitempty"`

	// Rollback holds the parameters of the Rollback Action.
	// +optional
	Rollback *RollbackParameters `json:"rollback,omitempty"`
}

// ProposedAction is the part of a Proposal that the agent supplies: one Action with its parameters.
// +kubebuilder:validation:XValidation:rule="self.action == 'Scale' ? (has(self.parameters) && has(self.parameters.scale)) : (!has(self.parameters) || !has(self.parameters.scale))",message="parameters.scale must be set exactly when the action is Scale"
// +kubebuilder:validation:XValidation:rule="self.action == 'PatchResources' ? (has(self.parameters) && has(self.parameters.patchResources)) : (!has(self.parameters) || !has(self.parameters.patchResources))",message="parameters.patchResources must be set exactly when the action is PatchResources"
// +kubebuilder:validation:XValidation:rule="self.action == 'Rollback' ? (has(self.parameters) && has(self.parameters.rollback)) : (!has(self.parameters) || !has(self.parameters.rollback))",message="parameters.rollback must be set exactly when the action is Rollback"
type ProposedAction struct {
	// Action is the remediation.
	// +required
	Action Action `json:"action"`

	// Parameters are the typed parameters of the Action.
	// +optional
	Parameters *ActionParameters `json:"parameters,omitempty"`
}

// RenderedChange is the exact change that Validation rendered, which the approver reviews and the publisher
// commits.
type RenderedChange struct {
	// Path is the Target Repository path that the patch changes.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Path string `json:"path,omitempty"`

	// Patch is the git patch against the recorded base commit.
	// +optional
	// +kubebuilder:validation:MaxLength=32768
	Patch string `json:"patch,omitempty"`
}

// ProposalBase records the state a Proposal was rendered against. A Proposal
// whose base changed is a Stale Proposal.
type ProposalBase struct {
	// Commit is the Target Repository commit the patch applies to.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Commit string `json:"commit,omitempty"`

	// TargetGeneration is the Target's metadata.generation when the Proposal was rendered.
	// +optional
	TargetGeneration int64 `json:"targetGeneration,omitempty"`
}

// ProposalKind is the form of a Proposal.
// +kubebuilder:validation:Enum=FilePatch;DiagnosisOnly;Action
type ProposalKind string

const (
	// ProposalKindFilePatch changes one to five whole files in the Target Repository.
	ProposalKindFilePatch ProposalKind = "FilePatch"
	// ProposalKindDiagnosisOnly recommends no change and ends the Page as Diagnosed.
	ProposalKindDiagnosisOnly ProposalKind = "DiagnosisOnly"
	// ProposalKindAction is the v1 fixed-Action form. It remains accepted while the report API still submits
	// Actions; the v2 agent submits FilePatch.
	ProposalKindAction ProposalKind = "Action"
)

// MaxProposalFiles is the largest number of files a Proposal may change.
const MaxProposalFiles = 5

// MaxFileContentBytes is the largest UpdatedContent a Proposal may carry for one file.
const MaxFileContentBytes = 64 << 10

// FileEdit is one whole-file change against the recorded base commit. The Page
// records the file's identity and the digest of its new content; the content
// itself is held by the server under that digest, so that Page status stays
// small and never carries repository bytes. The server derives the Diff from
// the content, so a model-supplied patch cannot be inconsistent with the base.
type FileEdit struct {
	// Path is the Target Repository path, relative to the repository root.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=500
	Path string `json:"path"`

	// BaseBlob is the Git blob SHA of the file at the recorded base commit.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40,64}$`
	BaseBlob string `json:"baseBlob"`

	// ContentDigest is the lowercase hexadecimal SHA-256 digest of the complete
	// new content of the file.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`

	// Rationale states why this file is the edit target for the Diagnosis.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=300
	Rationale string `json:"rationale"`
}

// ProposalWarningClass classifies a permitted change that the Operator must review.
// +kubebuilder:validation:Enum=Image;Replicas;ReplicasZero;NodePlacement;ResourceRemoval;Degraded
type ProposalWarningClass string

const (
	// WarningImage means a container image changed.
	WarningImage ProposalWarningClass = "Image"
	// WarningReplicas means the replica count changed.
	WarningReplicas ProposalWarningClass = "Replicas"
	// WarningReplicasZero means the replica count was set to zero.
	WarningReplicasZero ProposalWarningClass = "ReplicasZero"
	// WarningNodePlacement means node placement changed.
	WarningNodePlacement ProposalWarningClass = "NodePlacement"
	// WarningResourceRemoval means a resource request or limit was removed.
	WarningResourceRemoval ProposalWarningClass = "ResourceRemoval"
	// WarningDegraded means OpenAPI schema validation could not run.
	WarningDegraded ProposalWarningClass = "Degraded"
)

// ProposalWarning is a permitted change that is recorded on the Page and carried into the Pull Request body.
type ProposalWarning struct {
	// Class classifies the warning.
	// +required
	Class ProposalWarningClass `json:"class"`

	// Field names the document field the warning is about.
	// +required
	// +kubebuilder:validation:MaxLength=200
	Field string `json:"field"`

	// Message explains the warning.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=300
	Message string `json:"message"`
}

// ProposalValidationError is one reason Validation refused a Proposal.
type ProposalValidationError struct {
	// Field names the offending part of the Proposal.
	// +required
	// +kubebuilder:validation:MaxLength=200
	Field string `json:"field"`

	// Message explains the refusal.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=300
	Message string `json:"message"`
}

// ProposalValidation is the deterministic, read-only Validation report for a Proposal. Admission, quota, and
// defaulting are never evaluated: the repository's own Checks are the validating authority.
type ProposalValidation struct {
	// Passed reports whether Validation accepted the Proposal.
	// +required
	Passed bool `json:"passed"`

	// Degraded reports that OpenAPI schema validation could not run, so the Proposal is approvable but not
	// eligible for an unattended draft.
	// +optional
	Degraded bool `json:"degraded,omitempty"`

	// Errors are the refusals.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	// +listType=atomic
	Errors []ProposalValidationError `json:"errors,omitempty"`

	// Warnings are permitted changes that the Operator must review.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	// +listType=atomic
	Warnings []ProposalWarning `json:"warnings,omitempty"`

	// Admission states the one line that the Page shows about admission, quota, and defaulting.
	// +optional
	// +kubebuilder:validation:MaxLength=120
	Admission string `json:"admission,omitempty"`
}

// AdmissionNotEvaluated is the one line that every Validation report carries about what it does not check.
const AdmissionNotEvaluated = "admission: not evaluated — CI is authoritative"

// Proposal is the single typed remediation recommended for a Page. It is the unit that is validated, hashed,
// and approved.
type Proposal struct {
	// ProposedAction is the Action and its parameters, as supplied by the agent.
	ProposedAction `json:",inline"`

	// Kind is the form of the Proposal. It is FilePatch for the v2 agent, DiagnosisOnly when no change is
	// recommended, and Action for a v1 fixed-Action Proposal.
	// +optional
	Kind ProposalKind `json:"kind,omitempty"`

	// Files are the whole-file changes of a FilePatch Proposal.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	// +listType=atomic
	Files []FileEdit `json:"files,omitempty"`

	// DiagnosisOnlyReason explains why no change is recommended.
	// +optional
	// +kubebuilder:validation:MaxLength=1000
	DiagnosisOnlyReason string `json:"diagnosisOnlyReason,omitempty"`

	// Validation is the deterministic Validation report for this Proposal.
	// +optional
	Validation *ProposalValidation `json:"validation,omitempty"`

	// RenderedChange is the exact change that Validation rendered.
	// +optional
	RenderedChange *RenderedChange `json:"renderedChange,omitempty"`

	// Base is the state the change was rendered against.
	// +optional
	Base *ProposalBase `json:"base,omitempty"`

	// Hash binds an Approval to this exact Proposal. It is a lowercase hexadecimal SHA-256 digest.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	Hash string `json:"hash"`

	// Attempt is 1 for the first Proposal the agent submitted and 2 for its revision.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2
	Attempt int32 `json:"attempt"`

	// ExpiresAt is when an unapproved Proposal expires.
	// +required
	ExpiresAt metav1.Time `json:"expiresAt"`
}
