// Package pr is the isolated GitHub pull request writer. It is the only code
// that reads the write-scoped GitHub token, and it is the only code that
// changes a Target Repository (ADR-0043). Its entry point accepts a value that
// the server has already validated: a Page identity, an approved Proposal hash,
// a base commit, and whole-file contents. It never accepts model text.
//
// Every step is idempotent. A rerun after a crash reuses the branch and the
// existing Pull Request; a branch whose tree differs fails with ErrBranchConflict
// instead of overwriting a human's work; a "422 already exists" resolves by
// lookup. Pull Requests are never auto-merged: the package contains no merge
// call of any kind.
package pr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// BranchPrefix namespaces every branch that Pagerless opens.
	BranchPrefix = "pagerless/"

	// authorName and authorEmail identity the commits that Pagerless creates.
	authorName  = "pagerless-server"
	authorEmail = "pagerless-server@pagerless.dev"

	// maxFiles is the number of files a Proposal may change.
	maxFiles = 5

	requestTimeout = 30 * time.Second
	maxResponse    = 8 << 20
)

// ErrBranchConflict means the branch already exists with a different tree, so
// the writer refuses to overwrite it.
var ErrBranchConflict = errors.New("the branch already exists with a different tree")

// ErrBaseMoved means the base branch no longer points at the recorded base
// commit, so the Proposal is stale and must be approved again.
var ErrBaseMoved = errors.New("the base branch moved since the Proposal was rendered")

// ErrPermissionDenied means GitHub refused the token.
var ErrPermissionDenied = errors.New("GitHub refused the write token")

var (
	// branchPrefix and pageName guard the branch name that the writer creates.
	validBranch = regexp.MustCompile(`^pagerless/[a-z0-9]([-a-z0-9]*[a-z0-9])?-[0-9a-f]{8}$`)
	// hexDigest matches a lowercase hexadecimal SHA-256 digest.
	hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// hexSHA matches a Git object SHA.
	hexSHA = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
)

// PageRef identifies the Page whose Proposal the writer carries.
type PageRef struct {
	// Namespace is the Page's namespace.
	Namespace string
	// Name is the Page's name.
	Name string
	// UID is the Page's UID. A recreated Page is a different Page.
	UID string
}

// File is one whole-file change that a human approved.
type File struct {
	// Path is the Target Repository path, relative to the repository root.
	Path string
	// Content is the complete new content of the file.
	Content []byte
	// Digest is the lowercase hexadecimal SHA-256 digest of Content.
	Digest string
}

// Approved is the server-validated value that the writer acts on. Every field
// is derived from the Page and its Validation, never from model text.
type Approved struct {
	// Page is the Page the Proposal belongs to.
	Page PageRef
	// Hash is the approved Proposal hash.
	Hash string
	// BaseBranch is the branch the Pull Request targets.
	BaseBranch string
	// BaseCommit is the commit the Proposal was rendered against.
	BaseCommit string
	// Files are the approved whole-file changes.
	Files []File
	// Title is the Pull Request title.
	Title string
	// Body is the Pull Request body.
	Body string
	// Draft opens the Pull Request as a draft. Marking it ready and merging
	// remain human acts.
	Draft bool
	// Supersedes is the number of the Pull Request that this Proposal replaces,
	// or zero. The writer closes it with a comment that links the new one.
	Supersedes int
}

// Result is the state of the branch and Pull Request that carries an Approved Proposal.
type Result struct {
	// Branch is the branch that carries the Proposal.
	Branch string
	// Number is the Pull Request number.
	Number int
	// URL is the address of the Pull Request page.
	URL string
	// HeadSHA is the commit the branch points at.
	HeadSHA string
	// State is the Pull Request state: "open" or "closed".
	State string
	// Created reports that this call opened the Pull Request, rather than
	// reusing one that already existed.
	Created bool
}

// Branch returns the branch that carries an Approved Proposal:
// pagerless/<page>-<hash8>. The name is deterministic, so a repeated
// publication reuses the same branch and Pull Request.
func Branch(page PageRef, hash string) (string, error) {
	if !hexDigest.MatchString(hash) {
		return "", fmt.Errorf("the Proposal hash is not a lowercase hexadecimal SHA-256 digest")
	}
	name := strings.ToLower(page.Name)
	if name == "" {
		return "", errors.New("the Page has no name")
	}
	branch := BranchPrefix + name + "-" + hash[:8]
	if !validBranch.MatchString(branch) {
		return "", fmt.Errorf("the Page name %q does not make a branch name", page.Name)
	}
	return branch, nil
}

// validate checks an Approved value before any request is sent.
func (a Approved) validate() error {
	switch {
	case !hexDigest.MatchString(a.Hash):
		return errors.New("the Proposal hash is not a lowercase hexadecimal SHA-256 digest")
	case !hexSHA.MatchString(a.BaseCommit):
		return errors.New("the base commit is not a Git SHA")
	case a.BaseBranch == "":
		return errors.New("the base branch is empty")
	case a.Page.Name == "":
		return errors.New("the Page has no name")
	case strings.TrimSpace(a.Title) == "":
		return errors.New("the Pull Request title is empty")
	case strings.TrimSpace(a.Body) == "":
		return errors.New("the Pull Request body is empty")
	case len(a.Files) == 0:
		return errors.New("the Proposal changes no files")
	case len(a.Files) > maxFiles:
		return fmt.Errorf("the Proposal changes %d files, above the limit of %d", len(a.Files), maxFiles)
	}
	seen := make(map[string]struct{}, len(a.Files))
	for _, file := range a.Files {
		if file.Path == "" {
			return errors.New("a file has no path")
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return fmt.Errorf("%s appears twice", file.Path)
		}
		seen[file.Path] = struct{}{}
		if !hexDigest.MatchString(file.Digest) {
			return fmt.Errorf("the digest of %s is not a lowercase hexadecimal SHA-256 digest", file.Path)
		}
		sum := sha256.Sum256(file.Content)
		if hex.EncodeToString(sum[:]) != file.Digest {
			return fmt.Errorf("the content of %s does not match its digest", file.Path)
		}
	}
	return nil
}

// Ensure creates the blobs, tree, commit, branch, and Pull Request for an
// Approved Proposal, reusing whatever already exists. It is safe to call again
// after a crash at any point.
func (c *Client) Ensure(ctx context.Context, approved Approved) (Result, error) {
	if err := approved.validate(); err != nil {
		return Result{}, err
	}
	branch, err := Branch(approved.Page, approved.Hash)
	if err != nil {
		return Result{}, err
	}
	head, err := c.branchHead(ctx, approved.BaseBranch)
	if err != nil {
		return Result{}, fmt.Errorf("read the base branch %s: %w", approved.BaseBranch, err)
	}
	if head != approved.BaseCommit {
		return Result{}, fmt.Errorf("%w: %s is at %s, not %s",
			ErrBaseMoved, approved.BaseBranch, head, approved.BaseCommit)
	}
	tree, err := c.writeTree(ctx, approved)
	if err != nil {
		return Result{}, err
	}
	headSHA, err := c.ensureCommit(ctx, branch, approved, tree)
	if err != nil {
		return Result{}, err
	}
	pullRequest, created, err := c.ensurePullRequest(ctx, branch, approved, headSHA)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Branch: branch, Number: pullRequest.Number, URL: pullRequest.HTMLURL,
		HeadSHA: headSHA, State: pullRequest.State, Created: created,
	}
	if approved.Supersedes > 0 && approved.Supersedes != pullRequest.Number {
		if err := c.supersede(ctx, approved.Supersedes, result.URL); err != nil {
			return result, err
		}
	}
	return result, nil
}

// writeTree creates the blobs for the Proposal's files and the tree that
// applies them to the base commit's tree. Trees and blobs are content
// addressed, so creating them again is harmless.
func (c *Client) writeTree(ctx context.Context, approved Approved) (string, error) {
	base, err := c.commit(ctx, approved.BaseCommit)
	if err != nil {
		return "", fmt.Errorf("read the base commit %s: %w", approved.BaseCommit, err)
	}
	entries := make([]treeEntry, 0, len(approved.Files))
	for _, file := range approved.Files {
		blob, err := c.createBlob(ctx, file.Content)
		if err != nil {
			return "", fmt.Errorf("create the blob for %s: %w", file.Path, err)
		}
		entries = append(entries, treeEntry{Path: file.Path, Mode: "100644", Type: "blob", SHA: blob})
	}
	tree, err := c.createTree(ctx, base.Tree.SHA, entries)
	if err != nil {
		return "", fmt.Errorf("create the tree: %w", err)
	}
	return tree, nil
}

// ensureCommit returns the branch head, creating the commit and the branch when
// they do not exist. An existing branch is reused only when its tree is the
// tree this Proposal produces; otherwise the writer refuses to overwrite it.
func (c *Client) ensureCommit(ctx context.Context, branch string, approved Approved, tree string) (string, error) {
	existing, err := c.branchHead(ctx, branch)
	switch {
	case errors.Is(err, errNotFound):
	case err != nil:
		return "", fmt.Errorf("read the branch %s: %w", branch, err)
	default:
		current, err := c.commit(ctx, existing)
		if err != nil {
			return "", fmt.Errorf("read the branch head %s: %w", existing, err)
		}
		if current.Tree.SHA != tree {
			return "", fmt.Errorf("%w: %s carries tree %s, the Proposal produces %s",
				ErrBranchConflict, branch, current.Tree.SHA, tree)
		}
		return existing, nil
	}
	sha, err := c.createCommit(ctx, approved, tree)
	if err != nil {
		return "", fmt.Errorf("create the commit: %w", err)
	}
	if err := c.createRef(ctx, branch, sha); err != nil {
		if errors.Is(err, errReferenceExists) {
			// A concurrent writer created the branch first. Reuse it only when it
			// carries the same tree.
			return c.ensureCommit(ctx, branch, approved, tree)
		}
		return "", fmt.Errorf("create the branch %s: %w", branch, err)
	}
	return sha, nil
}

// ensurePullRequest returns the Pull Request for the branch, opening it when it
// does not exist.
func (c *Client) ensurePullRequest(
	ctx context.Context, branch string, approved Approved, headSHA string,
) (pullRequest, bool, error) {
	found, err := c.findPullRequest(ctx, branch)
	if err != nil {
		return pullRequest{}, false, fmt.Errorf("look up the Pull Request for %s: %w", branch, err)
	}
	if found != nil {
		return *found, false, nil
	}
	opened, err := c.openPullRequest(ctx, branch, approved)
	switch {
	case errors.Is(err, ErrPullRequestExists):
		// Another attempt, or a concurrent one, opened it first.
		found, err := c.findPullRequest(ctx, branch)
		if err != nil {
			return pullRequest{}, false, fmt.Errorf("look up the Pull Request for %s: %w", branch, err)
		}
		if found == nil {
			return pullRequest{}, false, fmt.Errorf("GitHub reports a Pull Request for %s but does not return it", branch)
		}
		return *found, false, nil
	case err != nil:
		return pullRequest{}, false, fmt.Errorf("open the Pull Request for %s: %w", branch, err)
	}
	if opened.Head.SHA != "" {
		headSHA = opened.Head.SHA
	}
	return *opened, true, nil
}

// supersede closes a Pull Request that a revised Proposal replaces, and links
// the new one in its thread. It is idempotent: an already closed Pull Request
// stays closed.
func (c *Client) supersede(ctx context.Context, number int, newURL string) error {
	current, err := c.pullRequest(ctx, number)
	if err != nil {
		return fmt.Errorf("read the superseded Pull Request %d: %w", number, err)
	}
	if err := c.comment(ctx, number,
		fmt.Sprintf("Superseded by %s, which carries the revised Proposal.", newURL)); err != nil {
		return fmt.Errorf("comment on the superseded Pull Request %d: %w", number, err)
	}
	if current.State == "closed" {
		return nil
	}
	if err := c.closePullRequest(ctx, number); err != nil {
		return fmt.Errorf("close the superseded Pull Request %d: %w", number, err)
	}
	return nil
}
