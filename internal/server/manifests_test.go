package server_test

import (
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/internal/testenv"
)

// TestServerPodSatisfiesThePodSecurityStandardOfItsNamespace renders the chart,
// finds the server's Deployment, and creates its pod as a dry run in the
// namespace that enforces the restricted standard.
func TestServerPodSatisfiesThePodSecurityStandardOfItsNamespace(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	raw, err := os.ReadFile(testenv.ChartObjects(t))
	if err != nil {
		t.Fatalf("read the rendered chart: %v", err)
	}
	for document := range strings.SplitSeq(string(raw), "\n---") {
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal([]byte(document), &deployment); err != nil {
			t.Fatalf("decode a rendered document: %v", err)
		}
		if deployment.Kind != "Deployment" || deployment.Name != "pagerless-server" {
			continue
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
		return
	}
	t.Fatal("the chart renders no pagerless-server Deployment")
}
