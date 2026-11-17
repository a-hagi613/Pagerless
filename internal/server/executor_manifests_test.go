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

	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const executorDeployDir = "../../deploy/pagerless-executor"

func readDeployment(t *testing.T, dir string) appsv1.Deployment {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.UnmarshalStrict(raw, &deployment); err != nil {
		t.Fatal(err)
	}
	return deployment
}

func TestExecutorKustomizationListsEveryManifest(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(executorDeployDir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(executorDeployDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, m := range matches {
		if name := filepath.Base(m); name != "kustomization.yaml" {
			onDisk = append(onDisk, name)
		}
	}
	slices.Sort(onDisk)
	listed := slices.Clone(kustomization.Resources)
	slices.Sort(listed)
	if !slices.Equal(onDisk, listed) {
		t.Errorf("kustomization resources = %v, manifests on disk = %v", listed, onDisk)
	}
}

// TestExecutorPodSatisfiesThePodSecurityStandardOfItsNamespace creates the
// pod that the Deployment describes, as a dry run, in the namespace that
// enforces the restricted standard.
func TestExecutorPodSatisfiesThePodSecurityStandardOfItsNamespace(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	deployment := readDeployment(t, executorDeployDir)
	pod := &corev1.Pod{
		Name:      "pagerless-executor-dry-run",
		Namespace: deployment.Namespace,
		Labels:    deployment.Spec.Template.Labels,
		Spec:      deployment.Spec.Template.Spec,
	}
	if err := env.Client.Create(t.Context(), pod, client.DryRunAll); err != nil {
		t.Errorf("the API server refuses the Executor's pod: %v", err)
	}
}

func TestExecutorRunsAsItsOwnServiceAccountWithTheWriteToken(t *testing.T) {
	t.Parallel()
	executorSpec := readDeployment(t, executorDeployDir).Spec.Template.Spec
	if executorSpec.ServiceAccountName != "pagerless-executor" {
		t.Errorf("the Executor runs as %q, want its own service account", executorSpec.ServiceAccountName)
	}
	serverSpec := readDeployment(t, deployDir).Spec.Template.Spec
	if serverSpec.ServiceAccountName == executorSpec.ServiceAccountName {
		t.Errorf("the server and the Executor share a service account")
	}
	hasWriteToken := func(spec corev1.PodSpec) bool {
		for _, volume := range spec.Volumes {
			if volume.Secret != nil && volume.Secret.SecretName == "github-write" {
				return true
			}
		}
		return false
	}
	if !hasWriteToken(executorSpec) {
		t.Errorf("the Executor does not mount the write-scoped GitHub token")
	}
	if hasWriteToken(serverSpec) {
		t.Errorf("the server mounts the write-scoped GitHub token, which only the Executor may hold")
	}
}

func TestServerProjectsATokenForTheExecutorsAudience(t *testing.T) {
	t.Parallel()
	var audiences []string
	for _, volume := range readDeployment(t, deployDir).Spec.Template.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				audiences = append(audiences, source.ServiceAccountToken.Audience)
			}
		}
	}
	if !slices.Equal(audiences, []string{executorapi.Audience}) {
		t.Errorf("projected token audiences = %v, want [%s]", audiences, executorapi.Audience)
	}
}
