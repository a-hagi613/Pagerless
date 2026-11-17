package testenv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/a-hagi613/pagerless/internal/testenv"
)

func TestBareRepoCanBeClonedAfterCommits(t *testing.T) {
	repo := testenv.NewBareRepo(t)
	first := repo.Head()

	second := repo.Commit("sandbox/checkout/deployment.yaml", "kind: Deployment\n", "Add checkout")
	if second == first {
		t.Fatal("Commit() did not advance the branch")
	}
	if got := repo.Head(); got != second {
		t.Errorf("Head() = %s, want %s", got, second)
	}

	clone := repo.Clone()
	content, err := os.ReadFile(filepath.Join(clone, "sandbox", "checkout", "deployment.yaml"))
	if err != nil || string(content) != "kind: Deployment\n" {
		t.Errorf("cloned file = %q, error %v", content, err)
	}
}
