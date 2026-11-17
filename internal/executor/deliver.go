package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

const (
	authorName  = "pagerless-executor"
	authorEmail = "pagerless-executor@pagerless.dev"

	// maxGitMessage bounds the git error text that a refusal carries.
	maxGitMessage = 400
)

var objectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func isObjectID(s string) bool { return objectID.MatchString(s) }

// deliver commits the stored patch to a new branch on its base commit,
// pushes the branch, and opens the pull request. It refuses, before anything
// leaves the Executor, when the patch does not apply cleanly or changes
// anything other than the one file the Proposal names.
func (e *Executor) deliver(
	ctx context.Context, tr *v1alpha1.Triage, change *v1alpha1.RenderedChange, branch string,
) (*PullRequest, error) {
	if err := e.push(ctx, tr, change, branch); err != nil {
		return nil, err
	}
	pr, err := e.cfg.GitHub.OpenPullRequest(ctx, branch, e.cfg.Sandbox.Branch, title(tr), body(tr))
	if errors.Is(err, ErrPullRequestExists) {
		// A request that this one raced with, or an earlier attempt, opened it first.
		pr, err = e.cfg.GitHub.FindPullRequest(ctx, branch)
		if err == nil && pr == nil {
			err = errors.New("GitHub reports a pull request for the branch but does not return it")
		}
	}
	return pr, err
}

func title(tr *v1alpha1.Triage) string {
	w := tr.Spec.Workload
	return fmt.Sprintf("Pagerless %s: %s %s/%s", tr.Status.Proposal.Action, w.Kind, w.Namespace, w.Name)
}

func body(tr *v1alpha1.Triage) string {
	current, decision, w := tr.Status.Proposal, tr.Status.Decision, tr.Spec.Workload
	var b strings.Builder
	fmt.Fprintf(&b, "Opened by pagerless-executor for Triage `%s`.\n\n", tr.Name)
	fmt.Fprintf(&b, "- Action: %s\n", current.Action)
	fmt.Fprintf(&b, "- Workload: %s %s/%s\n", w.Kind, w.Namespace, w.Name)
	fmt.Fprintf(&b, "- Base commit: `%s`\n", current.Base.Commit)
	fmt.Fprintf(&b, "- Proposal hash: `%s`\n", current.Hash)
	fmt.Fprintf(&b, "- Approved by: %s at %s\n", decision.Approver, decision.DecidedAt.UTC().Format(time.RFC3339))
	if decision.Reason != "" {
		fmt.Fprintf(&b, "- Reason: %s\n", decision.Reason)
	}
	if tr.Status.Diagnosis != "" {
		fmt.Fprintf(&b, "\nDiagnosis:\n\n```\n%s\n```\n", strings.ReplaceAll(tr.Status.Diagnosis, "```", "'''"))
	}
	b.WriteString("\nThe change is exactly the patch the approver reviewed. It merges when the Sandbox's check passes.\n")
	return b.String()
}

// push clones the Sandbox, applies the stored patch byte-for-byte on its
// base commit, commits it, and pushes the branch.
func (e *Executor) push(
	ctx context.Context, tr *v1alpha1.Triage, change *v1alpha1.RenderedChange, branch string,
) error {
	token, err := e.cfg.Token()
	if err != nil {
		return fmt.Errorf("read the GitHub token: %w", err)
	}
	dir, err := os.MkdirTemp("", "pagerless-executor-")
	if err != nil {
		return fmt.Errorf("create a working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	repo := filepath.Join(dir, "sandbox")
	g := gitSession{ctx: ctx, dir: repo, env: gitEnv(dir, e.cfg.Sandbox.URL, token, tr)}

	if _, err := sandbox.Git(ctx, "", g.env, nil, "clone", "--no-checkout", "--single-branch",
		"--branch", e.cfg.Sandbox.Branch, "--", e.cfg.Sandbox.URL, repo); err != nil {
		return fmt.Errorf("clone the Sandbox: %w", err)
	}
	base := tr.Status.Proposal.Base.Commit
	if _, err := g.run(nil, "checkout", "--quiet", "--detach", base); err != nil {
		return refuse(http.StatusUnprocessableEntity, executorapi.ReasonPatchDoesNotApply,
			"the base commit %s is not in the Sandbox's %s branch", base, e.cfg.Sandbox.Branch)
	}
	if _, err := g.run([]byte(change.Patch), "apply", "--index", "--whitespace=nowarn"); err != nil {
		return refuse(http.StatusUnprocessableEntity, executorapi.ReasonPatchDoesNotApply,
			"the stored patch does not apply cleanly to %s: %s", base, clip(err.Error()))
	}
	if err := g.checkOnlyChanged(change.Path); err != nil {
		return err
	}
	message := fmt.Sprintf("%s\n\nPagerless-Triage: %s/%s\nPagerless-Proposal-Hash: %s\nApproved-By: %s\n",
		title(tr), tr.Namespace, tr.Name, tr.Status.Proposal.Hash, tr.Status.Decision.Approver)
	if _, err := g.run(nil, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--no-verify", "--message", message); err != nil {
		return fmt.Errorf("commit the change: %w", err)
	}
	if _, err := g.run(nil, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}

// gitSession runs git in one working directory with one environment.
type gitSession struct {
	ctx context.Context
	dir string
	env []string
}

func (g gitSession) run(stdin []byte, args ...string) (string, error) {
	return sandbox.Git(g.ctx, g.dir, g.env, stdin, args...)
}

// checkOnlyChanged confirms that the applied patch changed exactly one
// existing regular file, the one the Proposal names, and nothing else: no
// other path, no creation or deletion, no rename, and no change of mode.
func (g gitSession) checkOnlyChanged(path string) *executorapi.Error {
	raw, err := g.run(nil, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-abbrev", "--no-ext-diff")
	if err != nil {
		return &executorapi.Error{
			StatusCode: http.StatusInternalServerError, Message: "inspect the applied patch: " + err.Error(),
		}
	}
	// With -z the output is ":<old mode> <new mode> <old id> <new id> <status>", NUL, the path, NUL.
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	unsafe := refuse(http.StatusConflict, executorapi.ReasonUnsafeChange,
		"the stored patch must modify only %s, and no other path or mode", path)
	if len(fields) != 2 || fields[1] != path {
		return unsafe
	}
	meta := strings.Fields(fields[0])
	if len(meta) != 5 || meta[4] != "M" {
		return unsafe
	}
	oldMode, newMode := strings.TrimPrefix(meta[0], ":"), meta[1]
	if oldMode != newMode || (newMode != "100644" && newMode != "100755") {
		return unsafe
	}
	return nil
}

// gitEnv is the environment of every git command the Executor runs: no user
// configuration, a fixed identity and a date taken from the decision (so that
// repeating a request recreates the same commit), and the write token as an
// HTTP header for the Sandbox's host only.
func gitEnv(home, sandboxURL, token string, tr *v1alpha1.Triage) []string {
	when := strconv.FormatInt(tr.Status.Decision.DecidedAt.Unix(), 10) + " +0000"
	env := []string{
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=" + authorName, "GIT_AUTHOR_EMAIL=" + authorEmail, "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=" + authorName, "GIT_COMMITTER_EMAIL=" + authorEmail, "GIT_COMMITTER_DATE=" + when,
	}
	if u, err := url.Parse(sandboxURL); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+u.Scheme+"://"+u.Host+"/.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+credentials,
		)
	}
	return env
}

// clip shortens git's error text for a refusal.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxGitMessage {
		return s[:maxGitMessage] + "..."
	}
	return s
}
