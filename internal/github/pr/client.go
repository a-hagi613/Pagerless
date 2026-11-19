package pr

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
	"strconv"
	"strings"

	"github.com/a-hagi613/pagerless/internal/github"
)

// Client writes to one Target Repository. It holds its own token function, so
// that the write capability is configured in exactly one place.
type Client struct {
	// APIURL is the base URL of the API. It defaults to github.DefaultGitHubAPI.
	APIURL string
	// Repository is the target repository's owner/name.
	Repository string
	// Token returns the write-scoped token. It is read for each request, so that
	// a rotated Secret takes effect.
	Token func() (string, error)
	// HTTPClient performs the requests. It defaults to a client with a timeout.
	HTTPClient *http.Client
}

var (
	// repositoryName matches the owner/name form of a GitHub repository.
	repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

	// errNotFound means the object does not exist.
	errNotFound = errors.New("not found")
	// errReferenceExists means the branch was created concurrently.
	errReferenceExists = errors.New("the reference already exists")
	// ErrPullRequestExists means a Pull Request already exists for the branch.
	ErrPullRequestExists = errors.New("a Pull Request already exists for the branch")
)

// treeEntry is one entry of a Git tree request.
type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// commit is the part of a Git commit that the writer reads.
type commit struct {
	SHA  string `json:"sha"`
	Tree struct {
		SHA string `json:"sha"`
	} `json:"tree"`
}

// pullRequest is the part of a Pull Request that the writer reads.
type pullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// baseURL returns the API base URL without a trailing slash.
func (c *Client) baseURL() string {
	if c.APIURL == "" {
		return github.DefaultGitHubAPI
	}
	return strings.TrimRight(c.APIURL, "/")
}

// owner returns the repository's owner.
func (c *Client) owner() string {
	owner, _, _ := strings.Cut(c.Repository, "/")
	return owner
}

// validate checks the client configuration.
func (c *Client) validate() error {
	if !repositoryName.MatchString(c.Repository) {
		return fmt.Errorf("%q does not name a repository as <owner>/<name>", c.Repository)
	}
	if c.Token == nil {
		return errors.New("the write token function is nil")
	}
	return nil
}

// createBlob creates a blob and returns its Git SHA.
func (c *Client) createBlob(ctx context.Context, content []byte) (string, error) {
	var answer struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/repos/"+c.Repository+"/git/blobs",
		map[string]string{"content": string(content), "encoding": "utf-8"}, &answer); err != nil {
		return "", err
	}
	if answer.SHA == "" {
		return "", errors.New("GitHub returned no blob SHA")
	}
	return answer.SHA, nil
}

// createTree creates a tree on top of the base tree and returns its SHA.
func (c *Client) createTree(ctx context.Context, base string, entries []treeEntry) (string, error) {
	var answer struct {
		SHA string `json:"sha"`
	}
	request := map[string]any{"base_tree": base, "tree": entries}
	if _, err := c.do(ctx, http.MethodPost, "/repos/"+c.Repository+"/git/trees", request, &answer); err != nil {
		return "", err
	}
	if answer.SHA == "" {
		return "", errors.New("GitHub returned no tree SHA")
	}
	return answer.SHA, nil
}

// createCommit creates a commit whose parent is the base commit and returns its SHA.
func (c *Client) createCommit(ctx context.Context, approved Approved, tree string) (string, error) {
	var answer struct {
		SHA string `json:"sha"`
	}
	message := fmt.Sprintf("%s\n\nPagerless-Proposal-Hash: %s\n", approved.Title, approved.Hash)
	request := map[string]any{
		"message":   message,
		"tree":      tree,
		"parents":   []string{approved.BaseCommit},
		"author":    map[string]string{"name": authorName, "email": authorEmail},
		"committer": map[string]string{"name": authorName, "email": authorEmail},
	}
	if _, err := c.do(ctx, http.MethodPost, "/repos/"+c.Repository+"/git/commits", request, &answer); err != nil {
		return "", err
	}
	if answer.SHA == "" {
		return "", errors.New("GitHub returned no commit SHA")
	}
	return answer.SHA, nil
}

// createRef creates the branch at the commit.
func (c *Client) createRef(ctx context.Context, branch, sha string) error {
	request := map[string]string{"ref": "refs/heads/" + branch, "sha": sha}
	status, err := c.do(ctx, http.MethodPost, "/repos/"+c.Repository+"/git/refs", request, &struct{}{})
	if status == http.StatusUnprocessableEntity || (err != nil && strings.Contains(err.Error(), "already exists")) {
		return errReferenceExists
	}
	return err
}

// branchHead returns the commit that the branch points at.
func (c *Client) branchHead(ctx context.Context, branch string) (string, error) {
	var answer struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	status, err := c.do(ctx, http.MethodGet,
		"/repos/"+c.Repository+"/git/ref/heads/"+url.PathEscape(branch), nil, &answer)
	if status == http.StatusNotFound {
		return "", errNotFound
	}
	if err != nil {
		return "", err
	}
	if answer.Object.SHA == "" {
		return "", fmt.Errorf("GitHub returned no commit for the branch %s", branch)
	}
	return answer.Object.SHA, nil
}

// commit returns the commit with its tree.
func (c *Client) commit(ctx context.Context, sha string) (commit, error) {
	var answer commit
	if _, err := c.do(ctx, http.MethodGet,
		"/repos/"+c.Repository+"/git/commits/"+url.PathEscape(sha), nil, &answer); err != nil {
		return commit{}, err
	}
	return answer, nil
}

// findPullRequest returns the most recent Pull Request for the branch, in any
// state, or nil when there is none.
func (c *Client) findPullRequest(ctx context.Context, branch string) (*pullRequest, error) {
	query := url.Values{"head": {c.owner() + ":" + branch}, "state": {"all"}, "per_page": {"1"}}
	var found []pullRequest
	if _, err := c.do(ctx, http.MethodGet,
		"/repos/"+c.Repository+"/pulls?"+query.Encode(), nil, &found); err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

// pullRequest returns one Pull Request by number.
func (c *Client) pullRequest(ctx context.Context, number int) (pullRequest, error) {
	var answer pullRequest
	if _, err := c.do(ctx, http.MethodGet,
		"/repos/"+c.Repository+"/pulls/"+strconv.Itoa(number), nil, &answer); err != nil {
		return pullRequest{}, err
	}
	return answer, nil
}

// openPullRequest opens a Pull Request from the branch into the base branch.
func (c *Client) openPullRequest(ctx context.Context, branch string, approved Approved) (*pullRequest, error) {
	request := map[string]any{
		"title": approved.Title,
		"head":  branch,
		"base":  approved.BaseBranch,
		"body":  approved.Body,
		"draft": approved.Draft,
	}
	var opened pullRequest
	status, err := c.do(ctx, http.MethodPost, "/repos/"+c.Repository+"/pulls", request, &opened)
	if status == http.StatusUnprocessableEntity && err != nil && strings.Contains(err.Error(), "already exists") {
		return nil, ErrPullRequestExists
	}
	if err != nil {
		return nil, err
	}
	return &opened, nil
}

// closePullRequest closes a Pull Request without merging it.
func (c *Client) closePullRequest(ctx context.Context, number int) error {
	_, err := c.do(ctx, http.MethodPatch,
		"/repos/"+c.Repository+"/pulls/"+strconv.Itoa(number), map[string]string{"state": "closed"}, &struct{}{})
	return err
}

// comment adds a comment to a Pull Request's thread.
func (c *Client) comment(ctx context.Context, number int, body string) error {
	_, err := c.do(ctx, http.MethodPost,
		"/repos/"+c.Repository+"/issues/"+strconv.Itoa(number)+"/comments",
		map[string]string{"body": body}, &struct{}{})
	return err
}

// do sends one request and decodes a JSON answer into out. It returns the HTTP
// status alongside any error, so that callers can tell refusals apart.
func (c *Client) do(ctx context.Context, method, apiPath string, body, out any) (int, error) {
	if err := c.validate(); err != nil {
		return 0, err
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+apiPath, reader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	token, err := c.Token()
	if err != nil {
		return 0, fmt.Errorf("read the GitHub token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, fmt.Errorf("GitHub answered %d: %w", resp.StatusCode, ErrPermissionDenied)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("GitHub answered %d: %s", resp.StatusCode, summarize(raw))
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(raw, out)
}

// summarize shortens an error body for a status message.
func summarize(raw []byte) string {
	const limit = 300
	text := strings.TrimSpace(strings.Join(strings.Fields(string(bytes.ToValidUTF8(raw, nil))), " "))
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
