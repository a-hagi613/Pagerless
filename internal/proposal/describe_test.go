package proposal

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

func TestDescribeNamesTheActionAndItsParameters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		action v1alpha1.ProposedAction
		want   string
	}{
		{
			name: "patch resources",
			action: v1alpha1.ProposedAction{
				Action: v1alpha1.ActionPatchResources,
				Parameters: &v1alpha1.ActionParameters{PatchResources: &v1alpha1.PatchResourcesParameters{
					Container: "memory-holder",
					Requests:  corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}},
			},
			want: "PatchResources container=memory-holder requests[memory]=64Mi limits[cpu]=500m,[memory]=128Mi",
		},
		{
			name: "scale",
			action: v1alpha1.ProposedAction{
				Action:     v1alpha1.ActionScale,
				Parameters: &v1alpha1.ActionParameters{Scale: &v1alpha1.ScaleParameters{Replicas: 0}},
			},
			want: "Scale replicas=0",
		},
		{
			name: "rollback",
			action: v1alpha1.ProposedAction{
				Action:     v1alpha1.ActionRollback,
				Parameters: &v1alpha1.ActionParameters{Rollback: &v1alpha1.RollbackParameters{Commit: "9eff68b"}},
			},
			want: "Rollback commit=9eff68b",
		},
		{
			name:   "an action without parameters",
			action: v1alpha1.ProposedAction{Action: v1alpha1.ActionScale},
			want:   "Scale",
		},
		{
			name:   "an action outside the menu",
			action: v1alpha1.ProposedAction{Action: v1alpha1.Action("Explode")},
			want:   "Explode",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Describe(tc.action); got != tc.want {
				t.Errorf("Describe() = %q, want %q", got, tc.want)
			}
		})
	}
}
