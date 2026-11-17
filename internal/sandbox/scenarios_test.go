package sandbox_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestScenarioPatchesChangeOneSandboxManifest checks that every Scenario in
// scenarios/ injects its failure as one change to the checkout Workload's
// manifest, which is the layout that `make break` applies the patch to.
func TestScenarioPatchesChangeOneSandboxManifest(t *testing.T) {
	t.Parallel()
	const checkoutManifest = "workloads/checkout/deployment.yaml"
	root := filepath.Join("..", "..", "scenarios")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		scenarios = append(scenarios, entry.Name())
		patch, err := os.ReadFile(filepath.Join(root, entry.Name(), "inject.patch"))
		if err != nil {
			t.Fatalf("Scenario %s: %v", entry.Name(), err)
		}
		text := string(patch)
		if !strings.HasSuffix(text, "\n") {
			t.Errorf("Scenario %s: the patch does not end with a newline", entry.Name())
		}
		if got := strings.Count(text, "diff --git "); got != 1 {
			t.Errorf("Scenario %s: the patch changes %d files, want exactly one", entry.Name(), got)
		}
		for _, want := range []string{"a/" + checkoutManifest, "b/" + checkoutManifest, "\n+++ b/"} {
			if !strings.Contains(text, want) {
				t.Errorf("Scenario %s: the patch lacks %q", entry.Name(), want)
			}
		}
		for _, unwanted := range []string{"new file mode", "deleted file mode", "rename from", "rename to"} {
			if strings.Contains(text, unwanted) {
				t.Errorf("Scenario %s: the patch contains %q, which a Scenario injection does not", entry.Name(), unwanted)
			}
		}
	}
	for _, want := range []string{"oom", "bad-config", "bad-image-tag", "unschedulable"} {
		if !slices.Contains(scenarios, want) {
			t.Errorf("scenarios/ defines %v, want it to include %q", scenarios, want)
		}
	}
}
