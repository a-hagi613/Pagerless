// Package sandbox reads the Sandbox, the separate Git repository that holds the
// Targets' manifests, and renders changes to it as git patches (ADR-0003,
// ADR-0008). It runs the git command line against a temporary shallow clone,
// and never writes to the Sandbox itself.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MaxFileSize bounds the size of a Sandbox file that Validation reads.
const MaxFileSize = 1 << 20

// MaxPathLength is the longest Sandbox path that Validation accepts. It matches the rendered change's schema.
const MaxPathLength = 500

// commandTimeout bounds one git invocation.
const commandTimeout = 60 * time.Second

// ErrNotFound means that the path does not name a regular file at the commit.
var ErrNotFound = errors.New("the path does not name a regular file in the Sandbox")

// ErrInvalidPath means that the path cannot name a manifest in the Sandbox.
var ErrInvalidPath = errors.New("invalid Sandbox path")

// safePath admits only the characters that git prints without quoting.
var safePath = regexp.MustCompile(`^[A-Za-z0-9._/@+=,-]+$`)

// Repository identifies the Sandbox.
type Repository struct {
	// URL is where git clones the Sandbox from.
	URL string
	// Branch is the branch that Argo CD deploys and the Executor's pull requests target.
	Branch string
	// ReadToken returns the read-only Sandbox token. It is read once per Head or Checkout so rotation is observed.
	// When set, URL must be HTTPS without embedded credentials, a query, or a fragment.
	ReadToken func() (string, error)
}

// CheckPath reports whether p is a path that Validation may read: a relative,
// clean path to a YAML file that stays inside the repository and avoids git's
// own directory.
func CheckPath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: the path is empty", ErrInvalidPath)
	case len(p) > MaxPathLength:
		return fmt.Errorf("%w: the path exceeds %d characters", ErrInvalidPath, MaxPathLength)
	case !safePath.MatchString(p):
		return fmt.Errorf("%w: the path may contain only letters, digits, and ._/@+=,-", ErrInvalidPath)
	case path.IsAbs(p) || path.Clean(p) != p:
		return fmt.Errorf("%w: the path must be relative and clean", ErrInvalidPath)
	case p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("%w: the path must stay inside the Sandbox", ErrInvalidPath)
	case !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml"):
		return fmt.Errorf("%w: the path must name a .yaml or .yml file", ErrInvalidPath)
	}
	for segment := range strings.SplitSeq(p, "/") {
		if strings.EqualFold(segment, ".git") {
			return fmt.Errorf("%w: the path must not enter git's metadata", ErrInvalidPath)
		}
	}
	return nil
}

// Head returns the commit at the tip of the Sandbox's branch without cloning.
func (r Repository) Head(ctx context.Context) (string, error) {
	if err := r.check(); err != nil {
		return "", err
	}
	env, err := r.readEnvironment()
	if err != nil {
		return "", err
	}
	out, err := run(ctx, "", env, nil, "ls-remote", "--exit-code", "--", r.URL, "refs/heads/"+r.Branch)
	if err != nil {
		return "", fmt.Errorf("read the Sandbox head: %w", err)
	}
	commit, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
	if !isObjectID(commit) {
		return "", fmt.Errorf("read the Sandbox head: unexpected answer %q", out)
	}
	return commit, nil
}

// Checkout is a snapshot of the Sandbox at one commit. Close removes it.
type Checkout struct {
	// Commit is the commit at the tip of the Sandbox's branch when the snapshot was taken.
	Commit string

	dir string
}

// Checkout clones the tip of the Sandbox's branch into a temporary directory.
func (r Repository) Checkout(ctx context.Context) (*Checkout, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	env, err := r.readEnvironment()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "pagerless-sandbox-")
	if err != nil {
		return nil, fmt.Errorf("create a directory for the Sandbox: %w", err)
	}
	c := &Checkout{dir: dir}
	if _, err := run(ctx, "", env, nil, "clone", "--bare", "--depth", "1", "--single-branch",
		"--branch", r.Branch, "--", r.URL, dir); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("clone the Sandbox: %w", err)
	}
	head, err := c.git(ctx, nil, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	c.Commit = strings.TrimSpace(head)
	return c, nil
}

// Close removes the snapshot.
func (c *Checkout) Close() error {
	if err := os.RemoveAll(c.dir); err != nil {
		return fmt.Errorf("remove the Sandbox snapshot: %w", err)
	}
	return nil
}

// Blob returns the object identifier of the file at the path, or ErrNotFound
// when the commit has no regular file there.
func (c *Checkout) Blob(ctx context.Context, p string) (id, mode string, err error) {
	if err := CheckPath(p); err != nil {
		return "", "", err
	}
	out, err := c.git(ctx, nil, nil, "ls-tree", "-z", "HEAD", "--", p)
	if err != nil {
		return "", "", err
	}
	// A line is "<mode> blob <id>\t<path>" followed by NUL.
	meta, name, found := strings.Cut(strings.TrimSuffix(out, "\x00"), "\t")
	fields := strings.Fields(meta)
	if !found || name != p || len(fields) != 3 || fields[1] != "blob" || !isRegularMode(fields[0]) {
		return "", "", ErrNotFound
	}
	return fields[2], fields[0], nil
}

// ReadFile returns the content of the file at the path.
func (c *Checkout) ReadFile(ctx context.Context, p string) ([]byte, error) {
	id, _, err := c.Blob(ctx, p)
	if err != nil {
		return nil, err
	}
	size, err := c.git(ctx, nil, nil, "cat-file", "-s", id)
	if err != nil {
		return nil, err
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(size), "%d", &n); err != nil {
		return nil, fmt.Errorf("read the size of %s: %w", p, err)
	}
	if n > MaxFileSize {
		return nil, fmt.Errorf("%w: %s is %d bytes, over the %d byte limit", ErrInvalidPath, p, n, MaxFileSize)
	}
	content, err := c.git(ctx, nil, nil, "cat-file", "blob", id)
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

// Diff returns the git patch that changes the file at the path from its
// content at the commit to the new content. The patch is deterministic: it
// names full object identifiers and uses fixed prefixes, so equal inputs give
// equal bytes in any clone. It is empty when the content is unchanged.
func (c *Checkout) Diff(ctx context.Context, p string, content []byte) (string, error) {
	_, mode, err := c.Blob(ctx, p)
	if err != nil {
		return "", err
	}
	index, err := os.CreateTemp(c.dir, "index-")
	if err != nil {
		return "", fmt.Errorf("create a scratch index: %w", err)
	}
	indexPath := index.Name()
	_ = index.Close()
	// git refuses an empty file as an index, so remove it and let git create it.
	if err := os.Remove(indexPath); err != nil {
		return "", fmt.Errorf("prepare a scratch index: %w", err)
	}
	env := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, err := c.git(ctx, env, nil, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	blob, err := c.git(ctx, env, content, "hash-object", "-w", "--no-filters", "--stdin")
	if err != nil {
		return "", err
	}
	if _, err := c.git(ctx, env, nil, "update-index", "--cacheinfo", mode+","+strings.TrimSpace(blob)+","+p); err != nil {
		return "", err
	}
	tree, err := c.git(ctx, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return c.git(ctx, nil, nil, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--full-index", "--src-prefix=a/", "--dst-prefix=b/", "HEAD^{tree}", strings.TrimSpace(tree))
}

func (c *Checkout) git(ctx context.Context, env []string, stdin []byte, args ...string) (string, error) {
	return run(ctx, c.dir, env, stdin, args...)
}

func (r Repository) check() error {
	switch {
	case r.URL == "" || strings.HasPrefix(r.URL, "-"):
		return errors.New("the Sandbox URL is not usable")
	case r.Branch == "" || strings.HasPrefix(r.Branch, "-") || strings.ContainsAny(r.Branch, " \t\n:~^?*[\\"):
		return errors.New("the Sandbox branch is not usable")
	}
	if r.ReadToken != nil {
		_, err := readURL(r.URL)
		return err
	}
	return nil
}

func isRegularMode(mode string) bool { return mode == "100644" || mode == "100755" }

var objectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func isObjectID(s string) bool { return objectID.MatchString(s) }

// run runs git with a fixed configuration: no user or system configuration,
// no prompts, and only the protocols a Sandbox can use.
func run(ctx context.Context, dir string, env []string, stdin []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if dir == "" {
		// Neither the working directory nor the temporary root is guaranteed to be outside a repository.
		// A private directory and a discovery ceiling prevent their local Git configuration from being read.
		outside, err := os.MkdirTemp("", "pagerless-git-")
		if err != nil {
			return "", fmt.Errorf("create an isolated Git directory: %w", err)
		}
		defer func() { _ = os.RemoveAll(outside) }()
		cmd.Dir = outside
	}
	cmd.Env = append(gitEnvironment(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(cmd.Dir),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=file:https:ssh",
		"LC_ALL=C",
	)
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(redactGitOutput(stderr.String(), env)))
	}
	return redactGitOutput(stdout.String(), env), nil
}
