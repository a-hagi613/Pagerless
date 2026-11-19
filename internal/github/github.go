package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultGitHubAPI is the base URL of GitHub's REST and GraphQL APIs.
	DefaultGitHubAPI = "https://api.github.com"

	// MergeMethod is how GitHub merges a pull request once its check passes. A squash merge leaves one commit on
	// the target repository's branch for the whole change.
	MergeMethod = "SQUASH"

	githubTimeout         = 30 * time.Second
	githubMaxResponseSize = 4 << 20
)

// ErrPullRequestExists means GitHub refused to open a pull request because one already exists for the branch.
var ErrPullRequestExists = errors.New("a pull request already exists for the branch")

// repositoryName matches the owner/name form of a GitHub repository.
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// RepositoryFromURL returns the owner/name of the GitHub repository that a
// clone URL such as https://github.com/octocat/pagerless-sandbox.git names.
func RepositoryFromURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a repository URL", raw)
	}
	name := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if !repositoryName.MatchString(name) {
		return "", fmt.Errorf("%q does not name a repository as <owner>/<name>", raw)
	}
	return name, nil
}

// GitHub is the part of GitHub's API that publishes a change: it finds,
// opens, and enables auto-merge on pull requests in the target repository. It
// authenticates with the write-scoped token (ADR-0015).
type GitHub struct {
	// APIURL is the base URL of the API. It defaults to DefaultGitHubAPI.
	APIURL string
	// Repository is the target repository's owner/name.
	Repository string
	// Token returns the write-scoped token. It is read for each request, so that a rotated Secret takes effect.
	Token func() (string, error)
	// HTTPClient performs the requests. It defaults to a client with a timeout.
	HTTPClient *http.Client
}

// PullRequest is the part of a pull request that Pagerless reads.
type PullRequest struct {
	// Number is the pull request number.
	Number int `json:"number"`
	// NodeID is the identifier that GraphQL mutations take.
	NodeID string `json:"node_id"`
	// HTMLURL is the address of the pull request page.
	HTMLURL string `json:"html_url"`
	// State is "open" or "closed".
	State string `json:"state"`
	// MergedAt is when the pull request merged, if it did.
	MergedAt *time.Time `json:"merged_at"`
	// MergeCommitSHA is the commit that the merge produced on the base branch, once merged.
	MergeCommitSHA string `json:"merge_commit_sha"`
	// AutoMerge describes the auto-merge request, when one is enabled.
	AutoMerge *struct {
		MergeMethod string `json:"merge_method"`
	} `json:"auto_merge"`
}

func (g *GitHub) baseURL() string {
	if g.APIURL == "" {
		return DefaultGitHubAPI
	}
	return strings.TrimRight(g.APIURL, "/")
}

func (g *GitHub) owner() string {
	owner, _, _ := strings.Cut(g.Repository, "/")
	return owner
}

// FindPullRequest returns the pull request, in any state, whose head is the
// branch in the target repository, or nil when there is none.
func (g *GitHub) FindPullRequest(ctx context.Context, branch string) (*PullRequest, error) {
	query := url.Values{"head": {g.owner() + ":" + branch}, "state": {"all"}, "per_page": {"1"}}
	var found []PullRequest
	if _, err := g.do(ctx, http.MethodGet, "/repos/"+g.Repository+"/pulls?"+query.Encode(), nil, &found); err != nil {
		return nil, fmt.Errorf("look up the pull request for %s: %w", branch, err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

// OpenPullRequest opens a pull request from the branch into the base branch.
// It returns ErrPullRequestExists when one already exists for the branch.
func (g *GitHub) OpenPullRequest(ctx context.Context, branch, base, title, body string) (*PullRequest, error) {
	request := map[string]string{"title": title, "head": branch, "base": base, "body": body}
	var opened PullRequest
	status, err := g.do(ctx, http.MethodPost, "/repos/"+g.Repository+"/pulls", request, &opened)
	switch {
	case status == http.StatusUnprocessableEntity && strings.Contains(fmt.Sprint(err), "already exists"):
		return nil, ErrPullRequestExists
	case err != nil:
		return nil, fmt.Errorf("open a pull request for %s: %w", branch, err)
	}
	return &opened, nil
}

// autoMergeMutation turns on auto-merge, which only the GraphQL API offers.
const autoMergeMutation = `mutation($id: ID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method}) {
    pullRequest { number }
  }
}`

// EnableAutoMerge turns on auto-merge for the pull request, so that GitHub
// merges it as soon as its required check passes.
func (g *GitHub) EnableAutoMerge(ctx context.Context, pr *PullRequest) error {
	request := map[string]any{
		"query":     autoMergeMutation,
		"variables": map[string]string{"id": pr.NodeID, "method": MergeMethod},
	}
	var answer struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := g.do(ctx, http.MethodPost, "/graphql", request, &answer); err != nil {
		return fmt.Errorf("enable auto-merge on pull request %d: %w", pr.Number, err)
	}
	if len(answer.Errors) > 0 {
		return fmt.Errorf("enable auto-merge on pull request %d: %s", pr.Number, answer.Errors[0].Message)
	}
	return nil
}

// do sends one request and decodes a JSON answer into out. It returns the
// HTTP status alongside any error, so that callers can tell refusals apart.
func (g *GitHub) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL()+path, reader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	token, err := g.Token()
	if err != nil {
		return 0, fmt.Errorf("read the GitHub token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := g.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: githubTimeout}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, githubMaxResponseSize))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("GitHub answered %d: %s", resp.StatusCode, summarize(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return resp.StatusCode, nil
}

// summarize shortens an error body for a log line or status message.
func summarize(raw []byte) string {
	const limit = 300
	text := strings.TrimSpace(string(raw))
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
