package patchresources

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/remediation"
)

// failingWriter fails every Update with a fixed error, so DryRun's
// classification of an API-server answer is tested without a cluster.
type failingWriter struct {
	client.Client
	err error
}

// Update returns the fixed error.
func (w failingWriter) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return w.err
}

// TestDryRunClassifiesAnAPIServerAnswer keeps the boundary between a refused
// Proposal and a Validation that could not run: an admission refusal is the
// Proposal's fault, while an unavailable API server is not.
func TestDryRunClassifiesAnAPIServerAnswer(t *testing.T) {
	t.Parallel()
	group := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := []struct {
		name       string
		answer     error
		validation bool
	}{
		{
			name: "an invalid object",
			answer: apierrors.NewInvalid(
				schema.GroupKind{Group: "apps", Kind: "Deployment"}, "checkout", nil),
			validation: true,
		},
		{
			name:       "a forbidden update",
			answer:     apierrors.NewForbidden(group, "checkout", errors.New("no")),
			validation: true,
		},
		{
			name:       "a bad request",
			answer:     apierrors.NewBadRequest("no"),
			validation: true,
		},
		{
			name:       "an unavailable API server",
			answer:     apierrors.NewServiceUnavailable("no"),
			validation: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live := liveWorkload()
			r := &renderer{
				params: v1alpha1.PatchResourcesParameters{Container: "app"},
				writer: failingWriter{err: tc.answer},
			}
			errs, err := r.DryRun(t.Context(), live)
			switch {
			case tc.validation && err != nil:
				t.Fatalf("DryRun() returned %v, want a Validation error, not a failed Validation", err)
			case tc.validation && len(errs) == 0:
				t.Fatal("DryRun() returned no Validation error for a refused update")
			case tc.validation && (errs[0].Field != "workload" || !strings.Contains(errs[0].Message, "dry run")):
				t.Errorf("error = %+v, want the workload field and the dry run refusal", errs[0])
			case !tc.validation && err == nil:
				t.Error("DryRun() hid an API-server failure as a refused Proposal")
			}
		})
	}
}

// liveWorkload is a Deployment the Proposal can patch without changing it.
func liveWorkload() *remediation.LiveWorkload {
	deployment := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "app",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
						},
					}},
				},
			},
		},
	}
	return &remediation.LiveWorkload{Object: deployment, PodSpec: &deployment.Spec.Template.Spec}
}
