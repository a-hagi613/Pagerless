package sandbox_test

import (
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/internal/sandbox"
)

// TestHistoryReadsTheKnownGoodTagFromAFreshClone proves the production reader
// path: the Unschedulable bound asks a fresh clone of the Sandbox's branch for
// the file at the known-good tag, and a normal clone must carry a tag that is
// not at the branch tip.
func TestHistoryReadsTheKnownGoodTagFromAFreshClone(t *testing.T) {
	t.Parallel()
	repo, sb := repoWith(t)
	knownGood := repo.Tag(sandbox.KnownGoodTag)
	repo.Commit(manifest, "kind: Deployment\nspec:\n  replicas: 3\n", "Raise replicas")
	if repo.Head() == knownGood {
		t.Fatal("the branch did not move past the known-good tag")
	}

	history, err := sb.History(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = history.Close() })
	content, err := history.FileAtTag(t.Context(), sandbox.KnownGoodTag, manifest)
	if err != nil {
		t.Fatalf("the fresh clone cannot read the known-good tag: %v", err)
	}
	text := string(content)
	if !strings.Contains(text, "replicas: 1") || strings.Contains(text, "replicas: 3") {
		t.Errorf("known-good %s = %q, want the file at the tag, not at the branch tip", manifest, text)
	}
}
