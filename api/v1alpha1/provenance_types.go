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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Confidence ranks how well a Resolution is grounded in the Target Repository.
// It decides whether an unattended draft pull request may be opened and never
// changes the Proposal Validation. A guess is never presented as exact.
// +kubebuilder:validation:Enum=exact;configured;identity;name-only;none
type Confidence string

const (
	// ConfidenceExact means the Target's own declaration names the file that
	// defines it, and the repository agrees.
	ConfidenceExact Confidence = "exact"
	// ConfidenceConfigured means a GitOps declaration or a configured mapping
	// names the repository and path, without an exact file declaration.
	ConfidenceConfigured Confidence = "configured"
	// ConfidenceIdentity means the file was found by matching the Target's
	// group, version, kind, namespace, and name against repository content.
	ConfidenceIdentity Confidence = "identity"
	// ConfidenceNameOnly means only the Target's name matched, so the file may
	// belong to a different object.
	ConfidenceNameOnly Confidence = "name-only"
	// ConfidenceNone means no repository grounding was found.
	ConfidenceNone Confidence = "none"
)

// PermitsUnattendedDraft reports whether a Resolution of this Confidence is
// strong enough to open a draft pull request without an Approval. Only exact,
// and configured under an explicit allowAutoDraft rule, qualify; the rule
// itself is enforced by the caller that knows it.
func (c Confidence) PermitsUnattendedDraft() bool {
	return c == ConfidenceExact || c == ConfidenceConfigured
}

// ResolutionSource names the rung of the provenance ladder that produced a
// Resolution.
// +kubebuilder:validation:Enum=Hint;ArgoCDTrackingID;ArgoCDInstance;FluxKustomization;FluxHelmRelease;HelmMetadata;ConfiguredMapping;IdentitySearch;None
type ResolutionSource string

const (
	// ResolutionSourceHint is an explicit pagerless.dev/* annotation, verified
	// against the repository before use.
	ResolutionSourceHint ResolutionSource = "Hint"
	// ResolutionSourceArgoCDTrackingID is Argo CD's tracking-id annotation.
	ResolutionSourceArgoCDTrackingID ResolutionSource = "ArgoCDTrackingID"
	// ResolutionSourceArgoCDInstance is the app.kubernetes.io/instance label,
	// verified against the named Argo CD Application.
	ResolutionSourceArgoCDInstance ResolutionSource = "ArgoCDInstance"
	// ResolutionSourceFluxKustomization is Flux's Kustomization labels.
	ResolutionSourceFluxKustomization ResolutionSource = "FluxKustomization"
	// ResolutionSourceFluxHelmRelease is Flux's HelmRelease labels.
	ResolutionSourceFluxHelmRelease ResolutionSource = "FluxHelmRelease"
	// ResolutionSourceHelmMetadata is Helm's release metadata labels.
	ResolutionSourceHelmMetadata ResolutionSource = "HelmMetadata"
	// ResolutionSourceConfiguredMapping is a repository rule that the Operator configured.
	ResolutionSourceConfiguredMapping ResolutionSource = "ConfiguredMapping"
	// ResolutionSourceIdentitySearch is the repository tree searched for a
	// document with the Target's identity.
	ResolutionSourceIdentitySearch ResolutionSource = "IdentitySearch"
	// ResolutionSourceNone means the ladder found nothing.
	ResolutionSourceNone ResolutionSource = "None"
)

// ResolutionAttemptOutcome is what one rung of the ladder did.
// +kubebuilder:validation:Enum=Resolved;Failed;Skipped
type ResolutionAttemptOutcome string

const (
	// ResolutionAttemptResolved means the rung produced the Resolution.
	ResolutionAttemptResolved ResolutionAttemptOutcome = "Resolved"
	// ResolutionAttemptFailed means the rung ran and found nothing usable.
	ResolutionAttemptFailed ResolutionAttemptOutcome = "Failed"
	// ResolutionAttemptSkipped means the rung did not apply to the Target.
	ResolutionAttemptSkipped ResolutionAttemptOutcome = "Skipped"
)

// ResolutionAttempt records one rung's outcome, so that an operator can see why
// a weaker rung produced the answer.
type ResolutionAttempt struct {
	// Source is the rung.
	// +required
	Source ResolutionSource `json:"source"`

	// Outcome is what the rung did.
	// +required
	Outcome ResolutionAttemptOutcome `json:"outcome"`

	// Reason explains a failure or a skip in one sentence.
	// +optional
	// +kubebuilder:validation:MaxLength=200
	Reason string `json:"reason,omitempty"`
}

// EditTargetKind is the kind of file that a change should go to. The declared
// order is the fixed precedence: an overlay patch that targets the object wins
// over the base manifest, and chart templates and Kustomize wiring are never
// edit targets.
// +kubebuilder:validation:Enum=OverlayPatch;OverlayResource;FluxPatch;ArgoValues;BaseManifest;HelmValues
type EditTargetKind string

const (
	// EditTargetOverlayPatch is a Kustomize overlay patch that targets the object.
	EditTargetOverlayPatch EditTargetKind = "OverlayPatch"
	// EditTargetOverlayResource is a Kustomize overlay resource that replaces the base.
	EditTargetOverlayResource EditTargetKind = "OverlayResource"
	// EditTargetFluxPatch is a patch that a Flux Kustomization applies.
	EditTargetFluxPatch EditTargetKind = "FluxPatch"
	// EditTargetArgoValues is a values file that an Argo CD Helm Application uses.
	EditTargetArgoValues EditTargetKind = "ArgoValues"
	// EditTargetBaseManifest is the base manifest that defines the object.
	EditTargetBaseManifest EditTargetKind = "BaseManifest"
	// EditTargetHelmValues is a Helm values file.
	EditTargetHelmValues EditTargetKind = "HelmValues"
)

// Rank returns the precedence of the kind, from highest to lowest. A caller
// that has several edit targets keeps the one with the highest rank.
func (k EditTargetKind) Rank() int {
	switch k {
	case EditTargetOverlayPatch:
		return 5
	case EditTargetOverlayResource:
		return 4
	case EditTargetFluxPatch:
		return 3
	case EditTargetArgoValues:
		return 2
	case EditTargetBaseManifest:
		return 1
	case EditTargetHelmValues:
		return 0
	default:
		return -1
	}
}

// IsEditable reports whether a file of this kind may ever be an edit target.
// Chart templates, Kustomize wiring, repository automation, and the other
// files that Validation refuses are not represented here at all, so every
// declared kind is editable.
func (k EditTargetKind) IsEditable() bool { return k.Rank() >= 0 }

// ResolutionCandidate is one legitimate file pair for an ambiguous Resolution.
// The model chooses among candidates and Validation checks the choice.
type ResolutionCandidate struct {
	// Path is the file that defines the failing object.
	// +required
	// +kubebuilder:validation:MaxLength=500
	Path string `json:"path"`

	// EditTarget is the file that a change should go to.
	// +required
	// +kubebuilder:validation:MaxLength=500
	EditTarget string `json:"editTarget"`

	// EditTargetKind is the kind of EditTarget.
	// +required
	EditTargetKind EditTargetKind `json:"editTargetKind"`
}

// Resolution records how the Target Repository was found: the four-rung
// provenance ladder's result, the file that defines the Target, and the file
// that a change should go to. A weak resolution is visible to the Operator and
// blocks unattended pull requests.
type Resolution struct {
	// URL is the resolved repository URL.
	// +optional
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=500
	URL string `json:"url,omitempty"`

	// Branch is the resolved branch.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Branch string `json:"branch,omitempty"`

	// Path is the file that defines the Target, relative to the repository root.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Path string `json:"path,omitempty"`

	// EditTarget is the file that a change should go to, relative to the repository root.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	EditTarget string `json:"editTarget,omitempty"`

	// EditTargetKind is the kind of EditTarget.
	// +optional
	EditTargetKind EditTargetKind `json:"editTargetKind,omitempty"`

	// Source is the rung that produced the Resolution.
	// +required
	Source ResolutionSource `json:"source"`

	// Confidence is the strength of the Resolution.
	// +required
	Confidence Confidence `json:"confidence"`

	// BaseCommit is the commit the Resolution was computed at and a Proposal is
	// rendered against.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	BaseCommit string `json:"baseCommit,omitempty"`

	// Ambiguous reports that two or more candidates are rank-equal, so the model
	// must choose one and Validation must check the choice.
	// +optional
	Ambiguous bool `json:"ambiguous,omitempty"`

	// Candidates are the rank-equal candidates of an ambiguous Resolution.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	// +listType=atomic
	Candidates []ResolutionCandidate `json:"candidates,omitempty"`

	// Attempts records what every rung did, in ladder order. Nine rungs can run, and a
	// final attempt records that none of them resolved the Target.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	// +listType=atomic
	Attempts []ResolutionAttempt `json:"attempts,omitempty"`

	// Reason explains a Resolution that found nothing, or an ambiguity, in one sentence.
	// +optional
	// +kubebuilder:validation:MaxLength=300
	Reason string `json:"reason,omitempty"`

	// ResolvedAt is when resolution ran.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`
}

// ExcludesProposalHash reports whether a Page must not propose the hash again.
// A Recurrence excludes its predecessor's Proposal hash so that the same change
// cannot be proposed forever.
func (s PageSpec) ExcludesProposalHash(hash string) bool {
	if hash == "" {
		return false
	}
	return slices.Contains(s.ExcludedProposalHashes, hash)
}
