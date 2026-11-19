// Package publish turns an approved fixed-Action Proposal into a pull request in
// the target repository. It commits the reviewed patch to a branch and opens
// the pull request; it never merges it, because merging is a human act
// (ADR-0041). The v2 file-Proposal writer lives in internal/github/pr.
package publish

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/github/pr"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

const (
	// branchPrefix namespaces the branches Pagerless opens in the target repository.
	branchPrefix = "pagerless/"
	authorName   = "pagerless-server"
	authorEmail  = "pagerless-server@pagerless.dev"
)

// ErrClosed means that the pull request was closed without merging.
var ErrClosed = errors.New("the pull request was closed without merging")

// Config configures a Publisher.
type Config struct {
	// Sandbox is the target repository the change is committed to.
	Sandbox sandbox.Repository
	// GitHub is the API client for the target repository's host.
	GitHub *pr.Client
	// Token returns the write-scoped token that pushes the branch. It is read for each push, so that a rotated
	// Secret takes effect.
	Token func() (string, error)
}

// Publisher publishes Proposals as pull requests. It is safe for concurrent use.
type Publisher struct {
	cfg Config
}

// New returns a Publisher.
func New(cfg Config) *Publisher { return &Publisher{cfg: cfg} }

// Result is the state of the pull request that a Proposal's branch carries.
type Result struct {
	// Branch is the branch the Proposal was committed to.
	Branch string
	// PullRequestURL is the address of the pull request page.
	// PullRequestNumber is the provider's number for the pull request.
	PullRequestNumber int
	PullRequestURL    string
	// Merged reports whether the pull request has merged, and MergeCommit is the commit the merge produced.
	Merged      bool
	MergeCommit string
	// Closed reports whether the pull request was closed without merging.
	Closed bool
}

// Branch returns the branch that carries the Page's Proposal. The name is deterministic, so that a repeated
// publication updates the same branch rather than opening a second pull request.
func Branch(tr *v1alpha1.Page) (string, error) {
	proposal := tr.Status.Proposal
	if proposal == nil || proposal.Hash == "" {
		return "", errors.New("the Page has no Proposal hash")
	}
	hash := proposal.Hash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return branchPrefix + tr.Name + "-" + hash, nil
}

// Publish commits the Proposal to its branch, opens the pull request if it is not open yet, and reports the pull
// request's state. A second call for the same Proposal observes the first call's result instead of acting again.
func (p *Publisher) Publish(ctx context.Context, tr *v1alpha1.Page) (Result, error) {
	branch, err := Branch(tr)
	if err != nil {
		return Result{}, err
	}
	pullRequest, err := p.cfg.GitHub.FindPullRequest(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if pullRequest == nil {
		if err := p.push(ctx, tr, branch); err != nil {
			return Result{}, err
		}
		pullRequest, err = p.cfg.GitHub.OpenPullRequest(ctx, branch, p.cfg.Sandbox.Branch, title(tr), body(tr))
		if errors.Is(err, pr.ErrPullRequestExists) {
			// A request that this one raced with, or an earlier attempt, opened it first.
			pullRequest, err = p.cfg.GitHub.FindPullRequest(ctx, branch)
			if err == nil && pullRequest == nil {
				err = errors.New("GitHub reports a Pull Request for the branch but does not return it")
			}
		}
		if err != nil {
			return Result{}, err
		}
	}
	result := Result{Branch: branch, PullRequestURL: pullRequest.HTMLURL, PullRequestNumber: pullRequest.Number}
	switch {
	case pullRequest.MergedAt != nil || pullRequest.MergeCommitSHA != "":
		result.Merged, result.MergeCommit = true, pullRequest.MergeCommitSHA
	case pullRequest.State == "closed":
		result.Closed = true
	}
	return result, nil
}

// push clones the target repository, applies the stored patch on the Proposal's base commit, commits it, and
// pushes the branch.
func (p *Publisher) push(ctx context.Context, tr *v1alpha1.Page, branch string) error {
	change := tr.Status.Proposal.RenderedChange
	if change == nil {
		return errors.New("the Page's Proposal holds no rendered change")
	}
	token, err := p.cfg.Token()
	if err != nil {
		return fmt.Errorf("read the GitHub token: %w", err)
	}
	dir, err := os.MkdirTemp("", "pagerless-publish-")
	if err != nil {
		return fmt.Errorf("create a working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	repo := filepath.Join(dir, "target")
	env := gitEnv(dir, p.cfg.Sandbox.URL, token, tr.Status.Decision.DecidedAt.Time)
	git := func(stdin []byte, args ...string) (string, error) {
		return sandbox.Git(ctx, repo, env, stdin, args...)
	}
	if _, err := sandbox.Git(ctx, "", env, nil, "clone", "--quiet", "--no-checkout", "--single-branch",
		"--branch", p.cfg.Sandbox.Branch, "--", p.cfg.Sandbox.URL, repo); err != nil {
		return fmt.Errorf("clone the target repository: %w", err)
	}
	base := tr.Status.Proposal.Base.Commit
	if _, err := git(nil, "checkout", "--quiet", "--detach", base); err != nil {
		return fmt.Errorf("the base commit %s is not in the %s branch: %w", base, p.cfg.Sandbox.Branch, err)
	}
	if _, err := git([]byte(change.Patch), "apply", "--index", "--whitespace=nowarn"); err != nil {
		return fmt.Errorf("the stored patch does not apply to %s: %w", base, err)
	}
	message := fmt.Sprintf("%s\n\nPagerless-Proposal-Hash: %s\nApproved-By: %s\n",
		title(tr), tr.Status.Proposal.Hash, tr.Status.Decision.DecidedBy)
	if _, err := git(nil, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--no-verify", "--message", message); err != nil {
		return fmt.Errorf("commit the change: %w", err)
	}
	if _, err := git(nil, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}

func title(tr *v1alpha1.Page) string {
	w := tr.Spec.Target
	return fmt.Sprintf("Pagerless %s: %s %s/%s", tr.Status.Proposal.Action, w.Kind, w.Namespace, w.Name)
}

func body(tr *v1alpha1.Page) string {
	current, decision, w := tr.Status.Proposal, tr.Status.Decision, tr.Spec.Target
	var b strings.Builder
	fmt.Fprintf(&b, "Opened by pagerless-server for Page `%s`.\n\n", tr.Name)
	fmt.Fprintf(&b, "- Action: %s\n", current.Action)
	fmt.Fprintf(&b, "- Target: %s %s/%s\n", w.Kind, w.Namespace, w.Name)
	fmt.Fprintf(&b, "- Base commit: `%s`\n", current.Base.Commit)
	fmt.Fprintf(&b, "- Proposal hash: `%s`\n", current.Hash)
	fmt.Fprintf(&b, "- Approved by: %s at %s\n", decision.DecidedBy, decision.DecidedAt.UTC().Format(time.RFC3339))
	if decision.Reason != "" {
		fmt.Fprintf(&b, "- Reason: %s\n", decision.Reason)
	}
	if tr.Status.Diagnosis != "" {
		fmt.Fprintf(&b, "\nDiagnosis:\n\n```\n%s\n```\n", strings.ReplaceAll(tr.Status.Diagnosis, "```", "'''"))
	}
	b.WriteString("\nThe change is exactly the patch the approver reviewed. It merges only when the " +
		"target repository's Checks pass and the Operator merges it.\n")
	return b.String()
}

// gitEnv is the environment of every git command: no inherited configuration, a fixed identity and the decision's
// date, and the write token as an HTTP header for the repository's host only.
func gitEnv(home, repositoryURL, token string, decidedAt time.Time) []string {
	when := strconv.FormatInt(decidedAt.Unix(), 10) + " +0000"
	env := []string{
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=" + authorName, "GIT_AUTHOR_EMAIL=" + authorEmail, "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=" + authorName, "GIT_COMMITTER_EMAIL=" + authorEmail, "GIT_COMMITTER_DATE=" + when,
	}
	if u, err := url.Parse(repositoryURL); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+u.Scheme+"://"+u.Host+"/.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+credentials,
		)
	}
	return env
}
