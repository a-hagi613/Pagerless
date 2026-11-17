package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitForTest runs git in directory with a fixed identity, so the test does not
// depend on the developer's global Git configuration.
func gitForTest(t *testing.T, directory string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.name=Pagerless Test", "-c", "user.email=test@pagerless.dev",
		"-c", "init.defaultBranch=main",
	}, args...)
	command := exec.CommandContext(t.Context(), "git", full...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func TestEnsureRepositoryPeelsAnAnnotatedTagAndRepairsAPartialRepository(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	source := t.TempDir()
	gitForTest(t, source, "init", "--bare", "--initial-branch=main", ".")
	work := t.TempDir()
	gitForTest(t, work, "init", "--initial-branch=main")
	if err := os.MkdirAll(filepath.Join(work, "workloads"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(work, "workloads", "checkout.yaml"),
		[]byte("kind: Deployment\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, work, "add", ".")
	gitForTest(t, work, "commit", "--message", "Initial Sandbox")
	gitForTest(t, work, "tag", "--annotate", "known-good", "--message", "known-good")
	gitForTest(t, work, "push", "--quiet", source, "main", "known-good")
	tag := gitForTest(t, work, "rev-parse", "known-good")
	commit := gitForTest(t, work, "rev-parse", "known-good^{commit}")
	if tag == commit {
		t.Fatal("the annotated tag must be a distinct object from its commit")
	}

	directory := filepath.Join(t.TempDir(), "pagerless-test", "sandbox.git")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ensureRepository(t.Context(), directory, source, tag)
	if err != nil {
		t.Fatalf("ensureRepository: %v", err)
	}
	if got != commit {
		t.Fatalf("base commit = %s, want the tag's commit %s", got, commit)
	}
	repeated, err := ensureRepository(t.Context(), directory, source, tag)
	if err != nil || repeated != commit {
		t.Fatalf("second call = %q, %v; want the existing commit %s", repeated, err, commit)
	}
	head := gitForTest(t, directory, "--git-dir", directory, "rev-parse", "refs/heads/main")
	if head != commit {
		t.Fatalf("refs/heads/main = %s, want %s", head, commit)
	}
	tagCommit := gitForTest(t, directory, "--git-dir", directory, "rev-parse", "refs/tags/known-good^{commit}")
	if tagCommit != commit {
		t.Fatalf("the known-good tag peels to %s, want %s", tagCommit, commit)
	}
}

func TestEnsureRepositoryLeavesNoRepositoryWhenTheFetchFails(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	directory := filepath.Join(t.TempDir(), "pagerless-test", "sandbox.git")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.git")
	if _, err := ensureRepository(t.Context(), directory, missing, strings.Repeat("a", 40)); err == nil {
		t.Fatal("a missing source repository was accepted")
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("a failed initialization left a repository behind: %v", err)
	}
}
