package proposal

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// maxDescribeRunes bounds the text that Describe returns, so that one refused
// Proposal leaves room for its errors and for the other attempt in a condition
// message.
const maxDescribeRunes = 300

// Describe renders a ProposedAction as one line for a condition message, such
// as "PatchResources container=memory-holder limits[memory]=64Mi". It never
// returns an empty string: an Action outside the menu is named as it was sent,
// so a refusal can always be attributed.
func Describe(action v1alpha1.ProposedAction) string {
	name := string(action.Action)
	if name == "" {
		name = "an Action with no name"
	}
	if action.Parameters == nil {
		return truncate(name)
	}
	switch {
	case action.Parameters.Scale != nil:
		return truncate(fmt.Sprintf("%s replicas=%d", name, action.Parameters.Scale.Replicas))
	case action.Parameters.PatchResources != nil:
		p := action.Parameters.PatchResources
		parts := []string{"container=" + p.Container}
		if len(p.Requests) > 0 {
			parts = append(parts, "requests"+resourceListLabel(p.Requests))
		}
		if len(p.Limits) > 0 {
			parts = append(parts, "limits"+resourceListLabel(p.Limits))
		}
		return truncate(name + " " + strings.Join(parts, " "))
	case action.Parameters.Rollback != nil:
		return truncate(name + " commit=" + action.Parameters.Rollback.Commit)
	default:
		return truncate(name)
	}
}

// resourceListLabel renders a ResourceList as "[cpu]=500m,[memory]=64Mi" in a
// stable order.
func resourceListLabel(list corev1.ResourceList) string {
	names := slices.Sorted(maps.Keys(list))
	parts := make([]string, 0, len(names))
	for _, name := range names {
		quantity := list[name]
		parts = append(parts, fmt.Sprintf("[%s]=%s", name, quantity.String()))
	}
	return strings.Join(parts, ",")
}

func truncate(s string) string {
	if runes := []rune(s); len(runes) > maxDescribeRunes {
		return string(runes[:maxDescribeRunes])
	}
	return s
}
