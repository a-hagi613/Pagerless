package server_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/internal/testenv"
)

const deployDir = "../../deploy/pagerless-server"

func TestKustomizationListsEveryManifest(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(deployDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, pattern := range []string{"*.yaml", "crds/*.yaml"} {
		matches, err := filepath.Glob(filepath.Join(deployDir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(deployDir, m)
			if err != nil {
				t.Fatal(err)
			}
			if rel != "kustomization.yaml" {
				onDisk = append(onDisk, filepath.ToSlash(rel))
			}
		}
	}
	slices.Sort(onDisk)
	listed := slices.Clone(kustomization.Resources)
	slices.Sort(listed)
	if !slices.Equal(onDisk, listed) {
		t.Errorf("kustomization resources = %v, manifests on disk = %v", listed, onDisk)
	}
}

// TestServerPodSatisfiesThePodSecurityStandardOfItsNamespace creates the pod
// that the Deployment describes, as a dry run, in the namespace that enforces
// the restricted standard.
func TestServerPodSatisfiesThePodSecurityStandardOfItsNamespace(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	raw, err := os.ReadFile(filepath.Join(deployDir, "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.UnmarshalStrict(raw, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		Name:      "pagerless-server-dry-run",
		Namespace: deployment.Namespace,
		Labels:    deployment.Spec.Template.Labels,
		Spec:      deployment.Spec.Template.Spec,
	}
	if err := env.Client.Create(t.Context(), pod, client.DryRunAll); err != nil {
		t.Errorf("the API server refuses the server's pod: %v", err)
	}
}
