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
// the Triage identifier, the Action, its parameters, the target Workload, and
// the rendered change (ADR-0008). Field order is fixed by the struct, and
// every map is encoded as a sorted slice, so equal Proposals encode to
// identical bytes.
type hashInput struct {
	Triage   triageIdentity             `json:"triage"`
	Workload v1alpha1.WorkloadReference `json:"workload"`
	Action   v1alpha1.Action            `json:"action"`
	Scale    *int32                     `json:"scale,omitempty"`
	Patch    *patchInput                `json:"patchResources,omitempty"`
	Rollback *string                    `json:"rollback,omitempty"`
	Change   *v1alpha1.RenderedChange   `json:"renderedChange,omitempty"`
}

type triageIdentity struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
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
// SHA-256 digest over the Triage identifier, the Action and its parameters,
// the target Workload, and the rendered change.
func Hash(triage *v1alpha1.Triage, action v1alpha1.ProposedAction, change *v1alpha1.RenderedChange) (string, error) {
	input := hashInput{
		Triage:   triageIdentity{Namespace: triage.Namespace, Name: triage.Name, UID: triage.UID},
		Workload: triage.Spec.Workload,
		Action:   action.Action,
		Change:   change,
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
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode proposal for hashing: %w", err)
	}
	sum := sha256.Sum256(append([]byte(hashDomain), encoded...))
	return hex.EncodeToString(sum[:]), nil
}
