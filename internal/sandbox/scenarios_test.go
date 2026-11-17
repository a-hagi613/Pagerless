package sandbox_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestScenarioPatchesChangeOneSandboxManifest checks that every Scenario in
// scenarios/ injects its failure as one change to one Sandbox manifest, which
// is the shape that `make break` applies the patch in. It names no Workload
// path, because a Scenario may break a Workload's own manifest or a shared file
// that its Workload depends on.
func TestScenarioPatchesChangeOneSandboxManifest(t *testing.T) {
	t.Parallel()
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
		if errors.Is(err, os.ErrNotExist) {
			// Scenario 9 is applied outside Git, so it has no inject.patch.
			continue
		}
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
		target := patchTarget(t, entry.Name(), text)
		if !strings.HasPrefix(target, "workloads/") || !strings.HasSuffix(target, ".yaml") {
			t.Errorf("Scenario %s: the patch targets %q, want a YAML manifest under workloads/", entry.Name(), target)
		}
		for _, want := range []string{"--- a/" + target, "+++ b/" + target, "\n+++ b/"} {
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
	for _, want := range []string{"oom", "injection", "bad-config", "bad-image-tag", "unschedulable"} {
		if !slices.Contains(scenarios, want) {
			t.Errorf("scenarios/ defines %v, want it to include %q", scenarios, want)
		}
	}
}

// patchTarget returns the one Sandbox path that the patch changes, and fails
// the test when its two diff headers name different paths.
func patchTarget(t *testing.T, scenario, patch string) string {
	t.Helper()
	var before, after string
	for line := range strings.SplitSeq(patch, "\n") {
		if !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			t.Fatalf("Scenario %s: the diff header %q does not name two paths", scenario, line)
		}
		before, after = strings.TrimPrefix(fields[2], "a/"), strings.TrimPrefix(fields[3], "b/")
	}
	if before == "" || before != after {
		t.Fatalf("Scenario %s: the patch changes %q and %q, want one path", scenario, before, after)
	}
	return before
}
