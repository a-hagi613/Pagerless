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

// MaxReasonLength is the longest reason a human may give for a decision.
const MaxReasonLength = 1000

// TriageApprovalSpec is a human decision on one Proposal. It is immutable after creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type TriageApprovalSpec struct {
	// Triage names the Triage, in the same namespace, whose Proposal this decision is about.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Triage string `json:"triage"`

	// ProposalHash is the hash of the one Proposal that this decision binds to.
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	ProposalHash string `json:"proposalHash"`

	// Decision is the human decision.
	// +required
	Decision DecisionOutcome `json:"decision"`

	// Reason is the optional reason the human gave.
	// +optional
	// +kubebuilder:validation:MaxLength=1000
	Reason string `json:"reason,omitempty"`

	// Approver is the identity of the requester. A mutating admission webhook sets it from the admission
	// request and overwrites any value the client supplied, so it cannot be forged.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Approver string `json:"approver,omitempty"`
}

// TriageApproval is a human decision to approve or reject one specific Proposal, bound to its hash (ADR-0004).
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=tra
// +kubebuilder:printcolumn:name="Triage",type=string,JSONPath=`.spec.triage`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="Approver",type=string,JSONPath=`.spec.approver`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TriageApproval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec TriageApprovalSpec `json:"spec"`
}

// TriageApprovalList is a list of TriageApprovals.
// +kubebuilder:object:root=true
type TriageApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TriageApproval `json:"items"`
}
