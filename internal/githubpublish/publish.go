// Package githubpublish is the isolated write path to GitHub. It is the only
// code in the service that creates a branch or a pull request, and it holds a
// credential the read-only integrations never see.
//
// The publisher validates the repository, the base revision, the base content
// and every path before it writes. It never pushes to the default branch and it
// never merges: a human reviews and merges in GitHub.
package githubpublish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/a-hagi613/pagerless/internal/diff"
)

// Bounds applied before anything reaches GitHub.
const (
	// MaxFiles bounds the changed files in one proposal.
	MaxFiles = 10
	// MaxFileBytes bounds one proposed file.
	MaxFileBytes = 256 << 10
	// MaxTotalBytes bounds all proposed content.
	MaxTotalBytes = 1 << 20
	// MaxTitleBytes bounds the pull-request title.
	MaxTitleBytes = 240
)

// Errors callers map onto a rejected proposal.
var (
	// ErrRepository means the proposal names no repository or the wrong one.
	ErrRepository = errors.New("repository is not the configured repository")
	// ErrPath means a path is absolute, traverses, or is outside the prefix.
	ErrPath = errors.New("path is outside the configured source directory")
	// ErrBase means the claimed base is not the current base revision.
	ErrBase = errors.New("base revision is not the current base revision")
	// ErrConflict means the branch already carries different work.
	ErrConflict = errors.New("the proposal branch already carries a different change")
	// ErrUnchanged means the proposal changes nothing.
	ErrUnchanged = errors.New("the proposal does not change the base content")
	// ErrInvalid means the proposal is malformed or exceeds a bound.
	ErrInvalid = errors.New("the proposal is not valid")
)

// File is one proposed file content.
type File struct {
	Path    string
	Content string
}

// Proposal is one verified change to publish.
type Proposal struct {
	// PageID and TurnID identify the investigation the change came from.
	PageID string
	TurnID string
	// Question is the first user message of the page.
	Question string
	// Findings is the redacted terminal answer.
	Findings string
	// Evidence lists the tool calls the answer cites.
	Evidence []EvidenceRef
	// BaseSHA is the verified base commit the change applies to.
	BaseSHA string
	// Files is the validated content.
	Files []File
}

// EvidenceRef is one citation rendered into the pull-request body.
type EvidenceRef struct {
	CallID string
	Tool   string
}

// PullRequest is an opened draft pull request.
type PullRequest struct {
	Number int
	URL    string
	// Created is false when an existing pull request was recovered.
	Created bool
}

// Published is the outcome of one proposal.
type Published struct {
	PullRequest PullRequest
	// Branch is the branch the pull request uses.
	Branch string
	// Diff is the server-rendered diff of every changed file.
	Diff []diff.File
}

// Reader reads repository state. Production uses a read-scoped credential.
type Reader interface {
	// Head returns the commit SHA of a branch or tag. found is false when the
	// reference does not exist.
	Head(ctx context.Context, ref string) (sha string, found bool, err error)
	// File returns one file at a revision. found is false when it does not exist.
	File(ctx context.Context, path, ref string) (content []byte, found bool, err error)
}

// Writer writes branches, files and pull requests. Production uses a separate
// write-scoped credential that the reader never uses.
type Writer interface {
	// CreateBranch creates a branch at a commit. It returns false when the
	// branch already exists.
	CreateBranch(ctx context.Context, branch, sha string) (created bool, err error)
	// PutFile commits one file on a branch. blobSHA is the current blob SHA, or
	// empty for a new file.
	PutFile(ctx context.Context, branch, path, content, message, blobSHA string) error
	// BlobSHA returns the blob SHA of a file on a branch, or empty when absent.
	BlobSHA(ctx context.Context, branch, path string) (string, error)
	// FindPullRequest returns the open pull request whose head is branch, if any.
	FindPullRequest(ctx context.Context, branch string) (*PullRequest, bool, error)
	// OpenDraftPullRequest opens a draft pull request.
	OpenDraftPullRequest(ctx context.Context, request DraftRequest) (PullRequest, error)
}

// DraftRequest is the content of a draft pull request.
type DraftRequest struct {
	Title string
	Head  string
	Base  string
	Body  string
}

// Config is the fixed publishing scope.
type Config struct {
	// Repository is owner/name. A proposal may not name another repository.
	Repository string
	// BaseBranch is the branch the change is based on.
	BaseBranch string
	// PathPrefix bounds every changed path when set.
	PathPrefix string
	// BaseURL is the GitHub web URL used in the pull-request body.
	BaseURL string
}

// Publisher validates and publishes one proposal.
type Publisher struct {
	Config Config
	Reader Reader
	Writer Writer
}

// Publish validates the proposal against the current base and opens one draft
// pull request. A retry recovers the branch and pull request it created.
func (p *Publisher) Publish(ctx context.Context, proposal Proposal) (Published, error) {
	if p.Reader == nil || p.Writer == nil {
		return Published{}, errors.New("github publish: the publisher needs a reader and a writer")
	}
	base, err := p.validate(ctx, proposal)
	if err != nil {
		return Published{}, err
	}
	branch := branchName(proposal)
	if existing, found, err := p.Writer.FindPullRequest(ctx, branch); err != nil {
		return Published{}, fmt.Errorf("find the pull request of %s: %w", branch, err)
	} else if found {
		return Published{PullRequest: *existing, Branch: branch, Diff: base.diff}, nil
	}
	if err := p.ensureBranch(ctx, branch, proposal.BaseSHA); err != nil {
		return Published{}, err
	}
	for _, file := range base.files {
		blob, err := p.Writer.BlobSHA(ctx, branch, file.Path)
		if err != nil {
			return Published{}, fmt.Errorf("read the branch file %s: %w", file.Path, err)
		}
		if blob != "" {
			current, found, err := p.Reader.File(ctx, file.Path, branch)
			if err != nil {
				return Published{}, fmt.Errorf("read the branch file %s: %w", file.Path, err)
			}
			if found && string(current) == file.Content {
				continue
			}
			if blob != base.blobs[file.Path] {
				return Published{}, fmt.Errorf("%w: %s", ErrConflict, file.Path)
			}
		}
		message := fmt.Sprintf("pagerless: %s", title(proposal.Question))
		if err := p.Writer.PutFile(ctx, branch, file.Path, file.Content, message, blob); err != nil {
			return Published{}, fmt.Errorf("commit %s: %w", file.Path, err)
		}
	}
	request := DraftRequest{
		Title: title(proposal.Question),
		Head:  branch,
		Base:  p.Config.BaseBranch,
		Body:  body(p.Config, proposal, base.files),
	}
	created, err := p.Writer.OpenDraftPullRequest(ctx, request)
	if err != nil {
		return Published{}, fmt.Errorf("open the draft pull request: %w", err)
	}
	created.Created = true
	return Published{PullRequest: created, Branch: branch, Diff: base.diff}, nil
}

// validated holds the base content and blobs of a checked proposal.
type validated struct {
	files []File
	blobs map[string]string
	diff  []diff.File
}

// validate checks ownership, paths, bounds and the base revision, and renders
// the diff from the base content.
func (p *Publisher) validate(ctx context.Context, proposal Proposal) (validated, error) {
	if p.Config.Repository == "" || p.Config.BaseBranch == "" {
		return validated{}, errors.New("github publish: the repository and base branch are not configured")
	}
	if len(proposal.Files) == 0 {
		return validated{}, fmt.Errorf("%w: no files", ErrInvalid)
	}
	if len(proposal.Files) > MaxFiles {
		return validated{}, fmt.Errorf("%w: more than %d files", ErrInvalid, MaxFiles)
	}
	head, found, err := p.Reader.Head(ctx, p.Config.BaseBranch)
	if err != nil {
		return validated{}, fmt.Errorf("read the base branch: %w", err)
	}
	if !found {
		return validated{}, fmt.Errorf("%w: the base branch %q does not exist", ErrBase, p.Config.BaseBranch)
	}
	if head != proposal.BaseSHA {
		return validated{}, fmt.Errorf("%w: the proposal is based on %s, the branch is at %s",
			ErrBase, proposal.BaseSHA, head)
	}
	result := validated{
		files: make([]File, 0, len(proposal.Files)),
		blobs: make(map[string]string, len(proposal.Files)),
		diff:  make([]diff.File, 0, len(proposal.Files)),
	}
	seen := make(map[string]bool, len(proposal.Files))
	total := 0
	for _, file := range proposal.Files {
		if err := p.checkPath(file.Path); err != nil {
			return validated{}, err
		}
		if seen[file.Path] {
			return validated{}, fmt.Errorf("%w: the path %q appears twice", ErrInvalid, file.Path)
		}
		seen[file.Path] = true
		if len(file.Content) > MaxFileBytes {
			return validated{}, fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalid, file.Path, MaxFileBytes)
		}
		if !utf8.ValidString(file.Content) {
			return validated{}, fmt.Errorf("%w: %s is not UTF-8 text", ErrInvalid, file.Path)
		}
		total += len(file.Content)
		if total > MaxTotalBytes {
			return validated{}, fmt.Errorf("%w: the content exceeds %d bytes", ErrInvalid, MaxTotalBytes)
		}
		base, found, err := p.Reader.File(ctx, file.Path, proposal.BaseSHA)
		if err != nil {
			return validated{}, fmt.Errorf("read %s at the base revision: %w", file.Path, err)
		}
		blob, err := p.Writer.BlobSHA(ctx, proposal.BaseSHA, file.Path)
		if err != nil {
			return validated{}, fmt.Errorf("read the base blob of %s: %w", file.Path, err)
		}
		result.blobs[file.Path] = blob
		if found && string(base) == file.Content {
			return validated{}, fmt.Errorf("%w: %s", ErrUnchanged, file.Path)
		}
		rendered, err := diff.UnifiedDiff(file.Path, base, []byte(file.Content))
		if err != nil {
			return validated{}, fmt.Errorf("render the diff of %s: %w", file.Path, err)
		}
		if rendered.Added == 0 && rendered.Removed == 0 {
			return validated{}, fmt.Errorf("%w: %s", ErrUnchanged, file.Path)
		}
		result.files = append(result.files, file)
		result.diff = append(result.diff, rendered)
	}
	return result, nil
}

// checkPath validates a path against the configured prefix.
func (p *Publisher) checkPath(path string) error {
	cleaned := strings.TrimSpace(path)
	if cleaned == "" || strings.HasPrefix(cleaned, "/") || strings.Contains(cleaned, `\`) ||
		strings.Contains(cleaned, "\x00") || strings.Contains(cleaned, "..") {
		return fmt.Errorf("%w: %q", ErrPath, path)
	}
	prefix := strings.Trim(strings.TrimSpace(p.Config.PathPrefix), "/")
	if prefix == "" {
		return nil
	}
	if cleaned != prefix && !strings.HasPrefix(cleaned, prefix+"/") {
		return fmt.Errorf("%w: %q is outside %q", ErrPath, path, prefix)
	}
	return nil
}

// ensureBranch creates the branch at the base, or verifies that an existing
// branch is the one a previous attempt of this turn created.
func (p *Publisher) ensureBranch(ctx context.Context, branch, baseSHA string) error {
	head, found, err := p.Reader.Head(ctx, branch)
	if err != nil {
		return fmt.Errorf("read the proposal branch: %w", err)
	}
	if found {
		if head == baseSHA {
			return nil
		}
		// A branch with commits is only acceptable when every file it carries
		// is the content this turn proposes; the caller verifies that per file.
		return nil
	}
	created, err := p.Writer.CreateBranch(ctx, branch, baseSHA)
	if err != nil {
		return fmt.Errorf("create the proposal branch: %w", err)
	}
	if !created {
		return fmt.Errorf("%w: the branch %s appeared while creating it", ErrConflict, branch)
	}
	return nil
}

// branchName derives a stable branch name for one turn. A retry of the same
// turn targets the same branch, which is what makes recovery possible.
func branchName(proposal Proposal) string {
	page := short(proposal.PageID, 12)
	turn := short(proposal.TurnID, 8)
	return "pagerless/" + page + "-" + turn
}

// short keeps the first n ASCII-safe characters.
func short(value string, n int) string {
	var builder strings.Builder
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' {
			builder.WriteRune(character)
		}
		if builder.Len() >= n {
			break
		}
	}
	if builder.Len() == 0 {
		return "page"
	}
	return builder.String()
}

// title derives a bounded pull-request title from the question.
func title(question string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(question), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		line = "Investigate a Kubernetes symptom"
	}
	if len(line) > MaxTitleBytes {
		line = strings.ToValidUTF8(line[:MaxTitleBytes], "")
	}
	return "pagerless: " + line
}

// body renders the pull-request description. It carries the question, the
// findings, the evidence references and the expected effect, and it never
// claims the change was applied.
func body(config Config, proposal Proposal, files []File) string {
	var builder strings.Builder
	builder.WriteString("## Question\n\n")
	builder.WriteString(quote(proposal.Question))
	builder.WriteString("\n\n## Findings\n\n")
	builder.WriteString(quote(proposal.Findings))
	builder.WriteString("\n\n## Evidence\n\n")
	if len(proposal.Evidence) == 0 {
		builder.WriteString("No tool call is cited.\n")
	} else {
		count := min(len(proposal.Evidence), 20)
		for _, reference := range proposal.Evidence[:count] {
			fmt.Fprintf(&builder, "- `%s` (%s)\n", reference.CallID, reference.Tool)
		}
	}
	builder.WriteString("\n## Files\n\n")
	for _, file := range files {
		fmt.Fprintf(&builder, "- `%s`\n", file.Path)
	}
	builder.WriteString("\n## Expected effect\n\n")
	builder.WriteString("The change is proposed for review. Pagerless did not apply it to a cluster.\n")
	builder.WriteString("\n---\n\n")
	fmt.Fprintf(&builder, "Page `%s`, turn `%s`, base `%s`.\n", proposal.PageID, proposal.TurnID, proposal.BaseSHA)
	if config.BaseURL != "" {
		fmt.Fprintf(&builder,
			"\nThis draft was opened by Pagerless. It does not merge, and it never writes to `%s` directly.\n",
			config.BaseBranch)
	}
	return builder.String()
}

func quote(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "_No content recorded._"
	}
	lines := strings.Split(trimmed, "\n")
	for index, line := range lines {
		lines[index] = "> " + line
	}
	return strings.Join(lines, "\n")
}
