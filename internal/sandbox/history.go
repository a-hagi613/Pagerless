package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

// KnownGoodTag is the Sandbox tag that marks the last state in which every
// Target was healthy. `make reset` points the branch at it, and the
// Unschedulable bound of PatchResources reads the requests it declares.
const KnownGoodTag = "known-good"

// ErrNoCommit means that the name does not identify a commit in the Sandbox.
var ErrNoCommit = errors.New("the commit is not in the Sandbox")

// ErrRevertConflict means that the commit does not revert cleanly against the
// current tip of the Sandbox's branch.
var ErrRevertConflict = errors.New("the commit does not revert cleanly against the current Sandbox commit")

// ErrRevertNotOneFile means that reverting the commit would change something
// other than exactly one existing regular file.
var ErrRevertNotOneFile = errors.New("reverting the commit would change more than one file")

// ErrAlreadyReverted means that the commit's change is not present at the tip,
// so reverting it would change nothing.
var ErrAlreadyReverted = errors.New("the commit is already reverted in the Sandbox")

// commitID matches a commit parameter: an abbreviated or full object
// identifier. It keeps a parameter from reaching git as an option or a revision
// expression.
var commitID = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// refName admits the characters that a tag that Validation reads may contain.
var refName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// History is a snapshot of the Sandbox that carries its commit history, which
// Checkout does not, and a working tree that a revert can be rendered in.
type History struct {
	// Commit is the commit at the tip of the Sandbox's branch when the snapshot was taken.
	Commit string

	dir string
}

// History clones the history of the Sandbox's branch into a temporary
// directory. A Rollback Validation and a revert's staleness check need the
// commits that touched a path, so they cannot use the shallow Checkout.
func (r Repository) History(ctx context.Context) (*History, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	env, err := r.readEnvironment()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "pagerless-history-")
	if err != nil {
		return nil, fmt.Errorf("create a directory for the Sandbox history: %w", err)
	}
	h := &History{dir: dir}
	if _, err := run(ctx, "", env, nil, "clone", "--quiet", "--single-branch", "--branch", r.Branch,
		"--", r.URL, dir); err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("clone the Sandbox history: %w", err)
	}
	head, err := h.git(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		_ = h.Close()
		return nil, err
	}
	h.Commit = strings.TrimSpace(head)
	return h, nil
}

// Close removes the snapshot.
func (h *History) Close() error {
	if err := os.RemoveAll(h.dir); err != nil {
		return fmt.Errorf("remove the Sandbox history snapshot: %w", err)
	}
	return nil
}

// Resolve returns the full identifier of the commit that the parameter names.
// It reports ErrNoCommit when the parameter is not a commit identifier or
// names no commit.
func (h *History) Resolve(ctx context.Context, parameter string) (string, error) {
	if !commitID.MatchString(parameter) {
		return "", ErrNoCommit
	}
	out, err := h.git(ctx, "rev-parse", "--verify", "--quiet", parameter+"^{commit}")
	if err != nil {
		return "", ErrNoCommit
	}
	id := strings.TrimSpace(out)
	if !isObjectID(id) {
		return "", ErrNoCommit
	}
	return id, nil
}

// Commits returns the commits that touched any of the paths, newest first, up
// to limit. A commit that touches several of the paths appears once.
func (h *History) Commits(ctx context.Context, paths []string, limit int) ([]string, error) {
	paths = historyPaths(paths)
	if len(paths) == 0 || limit < 1 {
		return nil, nil
	}
	args := make([]string, 0, 4+len(paths))
	args = append(args, "log", "--format=%H", fmt.Sprintf("--max-count=%d", limit), "--")
	args = append(args, paths...)
	out, err := h.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// Revert is the rendered revert of one commit: the single file it changes and
// the patch that changes it against the tip.
type Revert struct {
	// Path is the Sandbox path the revert changes.
	Path string
	// Patch is the git patch from the tip to the reverted state.
	Patch string
}

// Revert renders the revert of the commit against the tip of the branch. It
// refuses with ErrRevertConflict when the revert does not apply cleanly, and
// with ErrRevertNotOneFile when it would change anything other than exactly one
// existing regular file, which is all that an approved change may touch.
func (h *History) Revert(ctx context.Context, commit string) (Revert, error) {
	var none Revert
	if _, err := h.git(ctx, "checkout", "--quiet", "--force", "--detach", h.Commit); err != nil {
		return none, err
	}
	defer func() {
		_, _ = h.git(context.WithoutCancel(ctx), "revert", "--abort")
		_, _ = h.git(context.WithoutCancel(ctx), "reset", "--hard", "--quiet")
	}()
	if _, err := h.git(ctx, "revert", "--no-commit", "--no-edit", commit); err != nil {
		return none, fmt.Errorf("%w: %s", ErrRevertConflict, strings.TrimSpace(err.Error()))
	}
	path, err := h.revertedPath(ctx)
	if err != nil {
		return none, err
	}
	patch, err := h.git(ctx, "diff", "--cached", "--no-color", "--no-ext-diff", "--no-textconv",
		"--no-renames", "--full-index", "--src-prefix=a/", "--dst-prefix=b/")
	if err != nil {
		return none, err
	}
	if patch == "" {
		return none, ErrAlreadyReverted
	}
	return Revert{Path: path, Patch: patch}, nil
}

// revertedPath returns the one path the staged revert modifies.
func (h *History) revertedPath(ctx context.Context) (string, error) {
	raw, err := h.git(ctx, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	// With -z the output is ":<old mode> <new mode> <old id> <new id> <status>", NUL, the path, NUL.
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	if len(fields) != 2 {
		return "", ErrRevertNotOneFile
	}
	meta := strings.Fields(fields[0])
	if len(meta) != 5 || meta[4] != "M" {
		return "", ErrRevertNotOneFile
	}
	if oldMode, newMode := strings.TrimPrefix(meta[0], ":"), meta[1]; oldMode != newMode || !isRegularMode(newMode) {
		return "", ErrRevertNotOneFile
	}
	return fields[1], nil
}

// FileAtTag returns the content of the file at the path in the commit that the
// tag names. It reports ErrNotFound when the commit has no regular file there.
func (h *History) FileAtTag(ctx context.Context, tag, p string) ([]byte, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	if !refName.MatchString(tag) {
		return nil, fmt.Errorf("%w: %q is not a tag name", ErrNotFound, tag)
	}
	out, err := h.git(ctx, "rev-parse", "--verify", "--quiet", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return nil, ErrNotFound
	}
	commit := strings.TrimSpace(out)
	if !isObjectID(commit) {
		return nil, ErrNotFound
	}
	tree, err := h.git(ctx, "ls-tree", "-z", commit, "--", p)
	if err != nil {
		return nil, err
	}
	// A line is "<mode> blob <id>\t<path>" followed by NUL.
	head, name, found := strings.Cut(strings.TrimSuffix(tree, "\x00"), "\t")
	fields := strings.Fields(head)
	if !found || name != p || len(fields) != 3 || fields[1] != "blob" || !isRegularMode(fields[0]) {
		return nil, ErrNotFound
	}
	return h.readBlob(ctx, fields[2], p)
}

// readBlob returns the content of the blob, bounded by MaxFileSize.
func (h *History) readBlob(ctx context.Context, id, p string) ([]byte, error) {
	size, err := h.git(ctx, "cat-file", "-s", id)
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
	content, err := h.git(ctx, "cat-file", "blob", id)
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

// paths sorts and de-duplicates the paths that a commit window covers, so that
// the git invocation is deterministic.
func historyPaths(paths []string) []string {
	clean := slices.Clone(paths)
	slices.Sort(clean)
	return slices.Compact(clean)
}

func (h *History) git(ctx context.Context, args ...string) (string, error) {
	return run(ctx, h.dir, nil, nil, args...)
}
