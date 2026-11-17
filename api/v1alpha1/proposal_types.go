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

// Action is one kind of remediation on the fixed menu (ADR-0002).
// +kubebuilder:validation:Enum=RestartWorkload;Scale;PatchResources;Rollback;NoAction
type Action string

const (
	// ActionRestartWorkload deletes the Workload's pods one at a time. It executes live.
	ActionRestartWorkload Action = "RestartWorkload"
	// ActionScale changes the Workload's replica count through a pull request.
	ActionScale Action = "Scale"
	// ActionPatchResources changes a container's requests and limits through a pull request.
	ActionPatchResources Action = "PatchResources"
	// ActionRollback reverts one Sandbox commit through a pull request.
	ActionRollback Action = "Rollback"
	// ActionNoAction records a Diagnosis without a remediation.
	ActionNoAction Action = "NoAction"
)

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
	// Commit is the Sandbox commit to revert. It must be among the last 20 commits touching the Workload's path or a declared dependency path.
	// +required
	// +kubebuilder:validation:MinLength=7
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]+$`
	Commit string `json:"commit"`
}

// ActionParameters holds the typed parameters of an Action. At most one field is set, and it must match the Action.
// RestartWorkload and NoAction have no parameters.
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

// RenderedChange is the exact change that Validation rendered, which the approver reviews and the Executor applies.
type RenderedChange struct {
	// Path is the Sandbox path that the patch changes.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Path string `json:"path,omitempty"`

	// Patch is the git patch against the recorded base commit, for pull request Actions.
	// +optional
	// +kubebuilder:validation:MaxLength=32768
	Patch string `json:"patch,omitempty"`
}

// ProposalBase records the state a Proposal was rendered against. A Proposal whose base changed is a Stale Proposal.
type ProposalBase struct {
	// Commit is the Sandbox commit the patch applies to.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Commit string `json:"commit,omitempty"`

	// WorkloadGeneration is the Workload's metadata.generation when the Proposal was rendered.
	// +optional
	WorkloadGeneration int64 `json:"workloadGeneration,omitempty"`
}

// Proposal is the single typed remediation recommended for a Triage. It is the unit that is validated, hashed, and approved.
type Proposal struct {
	// ProposedAction is the Action and its parameters, as supplied by the agent.
	ProposedAction `json:",inline"`

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
