// Package proposal turns the Proposal that an agent submits into the Proposal
// that a human approves: it runs Validation for the Action, then binds the
// result to a hash (ADR-0008).
package proposal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// hashDomain separates Proposal hashes from every other SHA-256 use and
// versions the encoding below.
const hashDomain = "pagerless.dev/proposal/v1\n"

// hashInput is the canonical encoding of everything a Proposal hash covers:
// the Page identifier, the Action, its parameters, the target Target, and
// the rendered change with the base it was rendered against (ADR-0008). Field
// order is fixed by the struct, and every map is encoded as a sorted slice, so
// equal Proposals encode to identical bytes.
type hashInput struct {
	Page     pageIdentity             `json:"page"`
	Target   v1alpha1.TargetReference `json:"target"`
	Action   v1alpha1.Action          `json:"action"`
	Scale    *int32                   `json:"scale,omitempty"`
	Patch    *patchInput              `json:"patchResources,omitempty"`
	Rollback *string                  `json:"rollback,omitempty"`
	Files    []fileInput              `json:"files,omitempty"`
	Change   *v1alpha1.RenderedChange `json:"renderedChange,omitempty"`
	Base     *v1alpha1.ProposalBase   `json:"base,omitempty"`
}

type pageIdentity struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
}

// fileInput covers one whole-file edit. The content is hashed rather than
// embedded, so a hash is a fixed-size identity for any Proposal size.
type fileInput struct {
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Rationale  string `json:"rationale"`
	Confidence string `json:"confidence,omitempty"`
}

// fileInputs encodes a FilePatch's files in the order the model proposed them.
func fileInputs(parameters *v1alpha1.FilePatchParameters) []fileInput {
	if parameters == nil {
		return nil
	}
	inputs := make([]fileInput, 0, len(parameters.Files))
	for _, file := range parameters.Files {
		sum := sha256.Sum256([]byte(file.UpdatedContent))
		inputs = append(inputs, fileInput{
			Path: file.Path, Digest: hex.EncodeToString(sum[:]), Rationale: file.Rationale,
			Confidence: parameters.Confidence,
		})
	}
	return inputs
}

type patchInput struct {
	Container string         `json:"container"`
	Requests  []resourceItem `json:"requests"`
	Limits    []resourceItem `json:"limits"`
}

// resourceItem encodes a quantity by its exact value in milli-units, so that
// "1Gi" and "1024Mi" hash identically.
type resourceItem struct {
	Name       corev1.ResourceName `json:"name"`
	MilliValue int64               `json:"milliValue"`
}

func resourceItems(list corev1.ResourceList) []resourceItem {
	items := make([]resourceItem, 0, len(list))
	for name, quantity := range list {
		items = append(items, resourceItem{Name: name, MilliValue: quantity.MilliValue()})
	}
	slices.SortFunc(items, func(a, b resourceItem) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})
	return items
}

// Hash returns the hash that an Approval binds to: a lowercase hexadecimal
// SHA-256 digest over the Page identifier, the Action and its parameters,
// the target Target, and the rendered change with its base.
func Hash(
	page *v1alpha1.Page, action v1alpha1.ProposedAction, change *v1alpha1.RenderedChange, base *v1alpha1.ProposalBase,
) (string, error) {
	input := hashInput{
		Page:   pageIdentity{Namespace: page.Namespace, Name: page.Name, UID: page.UID},
		Target: page.Spec.Target,
		Action: action.Action,
		Change: change,
		Base:   base,
	}
	if p := action.Parameters; p != nil {
		if p.Scale != nil {
			replicas := p.Scale.Replicas
			input.Scale = &replicas
		}
		if p.PatchResources != nil {
			input.Patch = &patchInput{
				Container: p.PatchResources.Container,
				Requests:  resourceItems(p.PatchResources.Requests),
				Limits:    resourceItems(p.PatchResources.Limits),
			}
		}
		if p.Rollback != nil {
			commit := p.Rollback.Commit
			input.Rollback = &commit
		}
		if p.FilePatch != nil {
			input.Files = fileInputs(p.FilePatch)
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode proposal for hashing: %w", err)
	}
	sum := sha256.Sum256(append([]byte(hashDomain), encoded...))
	return hex.EncodeToString(sum[:]), nil
}
