package testenv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// BareRepo is a local bare git repository that stands in for the Sandbox. Its
// URL is a file path, which git clones, fetches from, and pushes to like any
// remote.
type BareRepo struct {
	// URL is the repository's location, usable as a git remote.
	URL string

	work string
	t    testing.TB
}

// NewBareRepo creates an empty bare repository with a main branch and one
// initial commit, and removes it when the test ends.
func NewBareRepo(t testing.TB) *BareRepo {
	t.Helper()
	dir := t.TempDir()
	r := &BareRepo{URL: filepath.Join(dir, "sandbox.git"), work: filepath.Join(dir, "work"), t: t}
	git(t, dir, "init", "--bare", "--initial-branch=main", r.URL)
	git(t, dir, "clone", r.URL, r.work)
	r.Commit("README.md", "sandbox\n", "Initial commit")
	return r
}

// Commit writes the file, commits it on main, pushes, and returns the new commit.
func (r *BareRepo) Commit(path, content, message string) string {
	r.t.Helper()
	full := filepath.Join(r.work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		r.t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		r.t.Fatalf("write %s: %v", path, err)
	}
	git(r.t, r.work, "add", path)
	git(r.t, r.work, "commit", "--message", message)
	git(r.t, r.work, "push", "origin", "HEAD:main")
	return r.Head()
}

// Symlink commits a symbolic link on main, pushes, and returns the new commit.
func (r *BareRepo) Symlink(path, target, message string) string {
	r.t.Helper()
	full := filepath.Join(r.work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		r.t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.Symlink(target, full); err != nil {
		r.t.Fatalf("link %s: %v", path, err)
	}
	git(r.t, r.work, "add", path)
	git(r.t, r.work, "commit", "--message", message)
	git(r.t, r.work, "push", "origin", "HEAD:main")
	return r.Head()
}

// Head returns the commit at the tip of main in the bare repository.
func (r *BareRepo) Head() string {
	r.t.Helper()
	return strings.TrimSpace(git(r.t, r.URL, "rev-parse", "main"))
}

// Clone checks the repository out into a new temporary directory and returns its path.
func (r *BareRepo) Clone() string {
	r.t.Helper()
	dir := filepath.Join(r.t.TempDir(), "clone")
	git(r.t, filepath.Dir(dir), "clone", r.URL, dir)
	return dir
}

// git runs git with a fixed identity and no user or system configuration.
func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gitOutput is git without the test: it returns the failure, so that code
// running outside the test's goroutine, such as the fake GitHub's handlers,
// can report it.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Pagerless Test", "GIT_AUTHOR_EMAIL=test@pagerless.dev",
		"GIT_COMMITTER_NAME=Pagerless Test", "GIT_COMMITTER_EMAIL=test@pagerless.dev",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}
