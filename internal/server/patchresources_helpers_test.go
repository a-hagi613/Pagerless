package server_test

import (
	"bytes"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// withPatchResources starts the harness with Validation accepting PatchResources for the sandbox namespace.
func withPatchResources() testenv.Option {
	return testenv.WithValidators(func(e *testenv.Env) map[v1alpha1.Action]proposal.ActionValidator {
		return e.PatchResources(sandboxNamespace)
	})
}

// withSandboxActions starts the harness with Validation accepting the Actions
// that change the Sandbox through a pull request in Phase 2.
func withSandboxActions() testenv.Option {
	return testenv.WithValidators(func(e *testenv.Env) map[v1alpha1.Action]proposal.ActionValidator {
		return e.SandboxActions(sandboxNamespace)
	})
}

// workload describes one Workload in the cluster and its manifest in the Sandbox.
type workload struct {
	name string
	// annotation overrides the source path annotation. noAnnotation leaves it out.
	annotation   string
	noAnnotation bool
	// dependsOn are the dependency paths declared in the Workload's annotations.
	dependsOn []string
	// requests and limits replace the manifest's container resources when set.
	requests, limits map[string]string
	// noLive leaves the Workload out of the cluster. noManifest leaves it out of the Sandbox.
	noLive, noManifest bool
	// mutateLive changes the Deployment in the cluster, so that it differs from its manifest.
	mutateLive func(*appsv1.Deployment)
}

// sourcePath is where the Workload's manifest lives in the Sandbox.
func (w workload) sourcePath() string { return "apps/" + w.name + "/deployment.yaml" }

func (w workload) manifest() string {
	annotations := ""
	if !w.noAnnotation {
		path := w.annotation
		if path == "" {
			path = w.sourcePath()
		}
		annotations = fmt.Sprintf("  annotations:\n    %s: %s\n", v1alpha1.AnnotationSourcePath, path)
		if len(w.dependsOn) > 0 {
			annotations += fmt.Sprintf("    %s: %s\n", v1alpha1.AnnotationDependsOnPaths, strings.Join(w.dependsOn, ","))
		}
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: sandbox
%[2]sspec:
  replicas: 1
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      containers:
        - name: app
          image: registry.invalid/%[1]s:1.0
%[3]s`, w.name, annotations, w.resources())
}

// const requests that every Workload in the harness declares.
const (
	requestCPU     = "100m"
	memoryResource = "memory"
	requestMemory  = "128Mi"
	limitMemory    = "256Mi"
)

// resources renders the container's resources, defaulting to the values that
// every Workload in the harness starts with.
func (w workload) resources() string {
	requests := w.requests
	if requests == nil {
		requests = map[string]string{"cpu": requestCPU, memoryResource: requestMemory}
	}
	limits := w.limits
	if limits == nil {
		limits = map[string]string{memoryResource: limitMemory}
	}
	var b strings.Builder
	b.WriteString("          resources:\n")
	for _, section := range []struct {
		name   string
		values map[string]string
	}{{"requests", requests}, {"limits", limits}} {
		b.WriteString("            " + section.name + ":\n")
		names := make([]string, 0, len(section.values))
		for name := range section.values {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			fmt.Fprintf(&b, "              %s: %s\n", name, section.values[name])
		}
	}
	return b.String()
}

// deploy commits the Workload's manifest to the Sandbox and creates the Workload in the cluster.
// It must not run concurrently with another call, because the Sandbox's working copy is shared.
func deploy(t *testing.T, env *testenv.Env, w workload) {
	t.Helper()
	ns := &corev1.Namespace{Name: sandboxNamespace}
	if err := env.Client.Create(t.Context(), ns); err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	if !w.noManifest {
		env.Sandbox.Commit(w.sourcePath(), w.manifest(), "Add "+w.name)
	}
	if w.noLive {
		return
	}
	var d appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(w.manifest()), &d); err != nil {
		t.Fatal(err)
	}
	if w.mutateLive != nil {
		w.mutateLive(&d)
	}
	if err := env.Client.Create(t.Context(), &d); err != nil {
		t.Fatalf("create Deployment %s: %v", w.name, err)
	}
}

func liveDeployment(t *testing.T, env *testenv.Env, name string) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: sandboxNamespace, Name: name}, &d); err != nil {
		t.Fatalf("get Deployment %s: %v", name, err)
	}
	return &d
}

func quantities(pairs ...string) corev1.ResourceList {
	list := corev1.ResourceList{}
	for i := 0; i < len(pairs); i += 2 {
		list[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return list
}

// patchReport is a report whose Proposal patches the app container's resources.
func patchReport(container string, requests, limits corev1.ResourceList) agentapi.Report {
	return agentapi.Report{
		Diagnosis: "The container is OOMKilled because its memory limit is too low.",
		Findings:  []v1alpha1.Finding{logFinding},
		Proposal: v1alpha1.ProposedAction{
			Action: v1alpha1.ActionPatchResources,
			Parameters: &v1alpha1.ActionParameters{PatchResources: &v1alpha1.PatchResourcesParameters{
				Container: container, Requests: requests, Limits: limits,
			}},
		},
	}
}

// startTriage creates a Triage for the Workload and returns its stand-in agent.
func startTriage(t *testing.T, env *testenv.Env, name string) (string, *testenv.StandInAgent) {
	t.Helper()
	created := env.CreateTriage(t, testenv.NewTriage(name))
	return created.Name, env.StartAgent(t, created.Name)
}

// rollbackReport is a report whose Proposal reverts one Sandbox commit.
func rollbackReport(commit string) agentapi.Report {
	return agentapi.Report{
		Diagnosis: "The last change to the file the Workload reads broke it, so that change should be reverted.",
		Findings:  []v1alpha1.Finding{logFinding},
		Proposal: v1alpha1.ProposedAction{
			Action:     v1alpha1.ActionRollback,
			Parameters: &v1alpha1.ActionParameters{Rollback: &v1alpha1.RollbackParameters{Commit: commit}},
		},
	}
}

// gitApplyCheck reports whether the patch applies cleanly to a fresh clone of the Sandbox.
func gitApplyCheck(t *testing.T, env *testenv.Env, patch string) error {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "apply", "--check")
	cmd.Dir = env.Sandbox.Clone()
	cmd.Stdin = strings.NewReader(patch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	return nil
}
