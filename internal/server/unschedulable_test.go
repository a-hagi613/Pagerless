package server_test

import (
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// unschedulableMemory is the request that the Unschedulable Scenario commits:
// larger than any node in the cluster can satisfy.
const unschedulableMemory = "256Gi"

// unschedulableRequests and unschedulableLimits are the container resources
// that the Unschedulable Scenario leaves in the Sandbox and in the cluster.
var (
	unschedulableRequests = map[string]string{"cpu": requestCPU, memoryResource: unschedulableMemory}
	unschedulableLimits   = map[string]string{memoryResource: unschedulableMemory}
)

// inject commits the Workload as a Scenario left it and moves the live
// Workload to match, as Argo CD's sync of the injected commit would.
func inject(t *testing.T, env *testenv.Env, w workload) string {
	t.Helper()
	commit := env.Sandbox.Commit(w.sourcePath(), w.manifest(), "Inject "+w.name)
	var manifest appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(w.manifest()), &manifest); err != nil {
		t.Fatal(err)
	}
	live := liveDeployment(t, env, w.name)
	live.Spec.Template.Spec.Containers[0].Resources = manifest.Spec.Template.Spec.Containers[0].Resources
	if err := env.Client.Update(t.Context(), live); err != nil {
		t.Fatalf("update Deployment %s to the injected state: %v", w.name, err)
	}
	return commit
}

func TestUnschedulableWorkloadPatchMustFitANodeAndTheKnownGoodValue(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	env.CreateNode(t, "worker-1", corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("4Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	})

	// The Workloads whose patches are tested against the Unschedulable bound. The
	// Sandbox's working copy is shared, so every commit is made before the
	// parallel subtests report their Proposals.
	pending := []string{
		"pending-at-known-good", "pending-above-known-good", "pending-beyond-the-node", "pending-left-alone",
	}
	for _, name := range pending {
		deploy(t, env, workload{name: name})
	}
	deploy(t, env, workload{name: "schedulable"})
	env.Sandbox.Tag(sandbox.KnownGoodTag)
	for _, name := range pending {
		inject(t, env, workload{name: name, requests: unschedulableRequests, limits: unschedulableLimits})
	}

	type attempt struct {
		name             string
		workload         string
		requests, limits corev1.ResourceList
		wantStatus       int
		wantParts        []string
	}
	attempts := []attempt{
		{name: "a request at the known-good value is accepted", workload: "pending-at-known-good",
			requests: quantities("memory", "128Mi"), limits: quantities("memory", "256Mi"), wantStatus: http.StatusOK},
		{name: "a request above the known-good value is refused", workload: "pending-above-known-good",
			requests: quantities("memory", "1Gi"), limits: quantities("memory", "2Gi"),
			wantStatus: http.StatusUnprocessableEntity, wantParts: []string{"known-good"}},
		{name: "a request no node can fit is refused", workload: "pending-beyond-the-node",
			requests: quantities("memory", "8Gi"), limits: quantities("memory", "8Gi"),
			wantStatus: http.StatusUnprocessableEntity, wantParts: []string{"node", "allocatable"}},
		{name: "a patch that leaves the impossible request is refused", workload: "pending-left-alone",
			limits:     quantities("cpu", "1"),
			wantStatus: http.StatusUnprocessableEntity, wantParts: []string{"unschedulable", "allocatable"}},
		{name: "a schedulable Workload may raise its request inside the node", workload: "schedulable",
			requests: quantities("memory", "1Gi"), limits: quantities("memory", "2Gi"), wantStatus: http.StatusOK},
	}

	for _, a := range attempts {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			name, agent := startTriage(t, env, a.workload)
			result, err := agent.API.PostReport(t.Context(), patchReport("app", a.requests, a.limits))
			if err != nil {
				t.Fatal(err)
			}
			if result.StatusCode != a.wantStatus {
				t.Fatalf("answer = %+v, want %d", result, a.wantStatus)
			}
			if a.wantStatus == http.StatusOK {
				tr := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
				if tr.Status.Proposal.Action != v1alpha1.ActionPatchResources {
					t.Errorf("proposal = %+v, want PatchResources", tr.Status.Proposal)
				}
				return
			}
			found := false
			for _, e := range result.Report.Errors {
				matched := true
				for _, part := range a.wantParts {
					matched = matched && strings.Contains(e.Field+e.Message, part)
				}
				found = found || matched
			}
			if !found {
				t.Errorf("errors = %+v, want one containing %v", result.Report.Errors, a.wantParts)
			}
		})
	}
}
