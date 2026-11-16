package sandbox_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	manifest   = "apps/checkout/deployment.yaml"
	mainBranch = "main"
)

func repoWith(t *testing.T) (*testenv.BareRepo, sandbox.Repository) {
	t.Helper()
	repo := testenv.NewBareRepo(t)
	repo.Commit(manifest, "kind: Deployment\nspec:\n  replicas: 1\n", "Add checkout")
	return repo, sandbox.Repository{URL: repo.URL, Branch: mainBranch}
}

func TestDiffAppliesCleanlyAndIsReproducible(t *testing.T) {
	t.Parallel()
	repo, sb := repoWith(t)
	updated := []byte("kind: Deployment\nspec:\n  replicas: 3\n")

	first, err := sb.Checkout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if first.Commit != repo.Head() {
		t.Errorf("checkout commit = %s, want the head %s", first.Commit, repo.Head())
	}
	patch, err := first.Diff(t.Context(), manifest, updated)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sb.Checkout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	again, err := second.Diff(t.Context(), manifest, updated)
	if err != nil {
		t.Fatal(err)
	}
	if patch != again {
		t.Errorf("two clones rendered different patches:\n%s\n%s", patch, again)
	}
	for _, want := range []string{"--- a/" + manifest, "+++ b/" + manifest, "-  replicas: 1", "+  replicas: 3"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, patch)
		}
	}

	// The patch applies byte-for-byte to a fresh clone and yields the new content.
	work := repo.Clone()
	apply := exec.CommandContext(t.Context(), "git", "apply")
	apply.Dir = work
	apply.Stdin = strings.NewReader(patch)
	var stderr bytes.Buffer
	apply.Stderr = &stderr
	if err := apply.Run(); err != nil {
		t.Fatalf("git apply: %v: %s", err, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(work, manifest))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, updated) {
		t.Errorf("applied content = %q, want %q", got, updated)
	}

	unchanged := []byte("kind: Deployment\nspec:\n  replicas: 1\n")
	if same, err := first.Diff(t.Context(), manifest, unchanged); err != nil || same != "" {
		t.Errorf("an unchanged file gave patch %q, error %v, want an empty patch", same, err)
	}
}

func TestBlobTracksTheFileAcrossCommits(t *testing.T) {
	t.Parallel()
	repo, sb := repoWith(t)
	before, err := sb.Checkout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = before.Close() })
	blobBefore, _, err := before.Blob(t.Context(), manifest)
	if err != nil {
		t.Fatal(err)
	}

	repo.Commit("README.md", "unrelated\n", "Change another file")
	head, err := sb.Head(t.Context())
	if err != nil || head != repo.Head() {
		t.Fatalf("head = %q, %v, want %s", head, err, repo.Head())
	}
	other, err := sb.Checkout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if blob, _, err := other.Blob(t.Context(), manifest); err != nil || blob != blobBefore {
		t.Errorf("blob after an unrelated commit = %s, %v, want %s", blob, err, blobBefore)
	}

	repo.Commit(manifest, "kind: Deployment\nspec:\n  replicas: 9\n", "Change the manifest")
	changed, err := sb.Checkout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = changed.Close() })
	if blob, _, err := changed.Blob(t.Context(), manifest); err != nil || blob == blobBefore {
		t.Errorf("blob after changing the file = %s, %v, want a new blob", blob, err)
	}
}

func TestCheckoutRefusesPathsThatAreNotRegularFiles(t *testing.T) {
	t.Parallel()
	repo, sb := repoWith(t)
	repo.Symlink("apps/link.yaml", "/etc/passwd", "Add a symbolic link")
	checkout, err := sb.Checkout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkout.Close() })

	for _, p := range []string{"apps/link.yaml", "apps/missing.yaml"} {
		if _, err := checkout.ReadFile(t.Context(), p); !errors.Is(err, sandbox.ErrNotFound) {
			t.Errorf("ReadFile(%q) error = %v, want ErrNotFound", p, err)
		}
	}
}

func TestCheckPathRejectsPathsThatCouldEscapeTheSandbox(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"", "/etc/passwd.yaml", "../x.yaml", "apps/../../x.yaml", "apps//x.yaml", "./x.yaml", "apps/x.yaml/",
		".git/config.yaml", "apps/.GIT/x.yaml", "apps/x.txt", "apps/x y.yaml", "apps/x\n.yaml", "apps/x\\y.yaml",
		"apps/" + strings.Repeat("a", sandbox.MaxPathLength) + ".yaml", "-x.yaml/..", "..",
	} {
		if err := sandbox.CheckPath(p); !errors.Is(err, sandbox.ErrInvalidPath) {
			t.Errorf("CheckPath(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
	for _, p := range []string{manifest, "a.yml", "apps/.github/x.yaml", "apps/team-a_1/svc@2.yaml"} {
		if err := sandbox.CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v, want it accepted", p, err)
		}
	}
}

func TestRepositoryRefusesOptionLookalikes(t *testing.T) {
	t.Parallel()
	for _, r := range []sandbox.Repository{
		{URL: "--upload-pack=touch /tmp/x", Branch: "main"},
		{URL: "/tmp/repo", Branch: "--evil"},
		{URL: "", Branch: "main"},
		{URL: "/tmp/repo", Branch: ""},
	} {
		if _, err := r.Checkout(t.Context()); err == nil {
			t.Errorf("Checkout of %+v succeeded, want an error", r)
		}
		if _, err := r.Head(t.Context()); err == nil {
			t.Errorf("Head of %+v succeeded, want an error", r)
		}
	}
}
