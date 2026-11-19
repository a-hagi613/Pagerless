// Package github is the read-only GitHub client. It reads a Target Repository's
// tree, files, and recent commits, and it holds no write capability: the only
// code that writes to GitHub is internal/github/pr, which the read paths cannot
// reach (ADR-0043).
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultGitHubAPI is the base URL of GitHub's REST API.
	DefaultGitHubAPI = "https://api.github.com"

	// MaxFileBytes is the largest file that the client reads. A page of a
	// repository's manifests is far smaller, and the bound protects the
	// process from a hostile or mistaken file.
	MaxFileBytes = 1 << 20

	requestTimeout = 30 * time.Second
	maxResponse    = 8 << 20
)

// ErrNotFound means the repository, reference, or path does not exist.
var ErrNotFound = errors.New("not found")

// ErrPermissionDenied means the token cannot read what was asked for.
var ErrPermissionDenied = errors.New("permission denied")

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

// Entry is one path in a repository tree.
type Entry struct {
	// Path is the file's path relative to the repository root.
	Path string `json:"path"`
	// Type is "blob" for a file, "tree" for a directory, and "commit" for a submodule.
	Type string `json:"type"`
	// Size is the size in bytes. GitHub omits it for directories and submodules.
	Size int64 `json:"size"`
	// SHA is the object's Git SHA.
	SHA string `json:"sha"`
}

// Tree is the recursive listing of a repository at one reference.
type Tree struct {
	// Entries are the tree's files, in the order GitHub returns them.
	Entries []Entry
	// Truncated reports that GitHub capped the listing. A caller that needs
	// every path must treat a truncated tree as unusable.
	Truncated bool
}

// Files returns the entries that are files, in path order.
func (t Tree) Files() []Entry {
	files := make([]Entry, 0, len(t.Entries))
	for _, entry := range t.Entries {
		if entry.Type == "blob" {
			files = append(files, entry)
		}
	}
	return files
}

// Commit is one commit that touched a path.
type Commit struct {
	// SHA is the commit's SHA.
	SHA string `json:"sha"`
	// Message is the commit message.
	Message string `json:"-"`
	// Date is the committer's date.
	Date time.Time `json:"-"`
}

// File is the content and blob identity of one file at a reference.
type File struct {
	// Path is the file's path relative to the repository root.
	Path string
	// Blob is the Git blob SHA of the content. A Proposal records it so that a
	// changed base is detected before approval.
	Blob string
	// Content is the file's bytes.
	Content []byte
}

// Client reads one Target Repository. It holds no method that changes the
// repository (ADR-0043).
type Client struct {
	// APIURL is the base URL of the API. It defaults to DefaultGitHubAPI.
	APIURL string
	// Repository is the target repository's owner/name.
	Repository string
	// Token returns the read-scoped token, when the repository is not public. It
	// is read for each request, so that a rotated Secret takes effect.
	Token func() (string, error)
	// HTTPClient performs the requests. It defaults to a client with a timeout.
	HTTPClient *http.Client
}

// Tree returns the recursive tree at the reference: a branch name, tag, or commit SHA.
func (c *Client) Tree(ctx context.Context, ref string) (Tree, error) {
	query := url.Values{"recursive": {"1"}}
	var answer struct {
		Tree      []Entry `json:"tree"`
		Truncated bool    `json:"truncated"`
	}
	apiPath := "/repos/" + c.Repository + "/git/trees/" + url.PathEscape(ref) + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, apiPath, &answer); err != nil {
		return Tree{}, fmt.Errorf("list the tree at %s: %w", ref, err)
	}
	return Tree{Entries: answer.Tree, Truncated: answer.Truncated}, nil
}

// File returns the content and blob SHA of one file at the reference.
func (c *Client) File(ctx context.Context, ref, filePath string) (File, error) {
	cleaned := path.Clean("/" + filePath)
	if cleaned == "/" || strings.HasPrefix(cleaned, "/../") {
		return File{}, fmt.Errorf("%q is not a repository path", filePath)
	}
	query := url.Values{"ref": {ref}}
	var answer struct {
		Path     string `json:"path"`
		SHA      string `json:"sha"`
		Size     int64  `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	apiPath := "/repos/" + c.Repository + "/contents" + cleaned + "?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, apiPath, &answer); err != nil {
		return File{}, fmt.Errorf("read %s at %s: %w", filePath, ref, err)
	}
	if answer.Size > MaxFileBytes {
		return File{}, fmt.Errorf("%s at %s is %d bytes, above the %d-byte limit", filePath, ref, answer.Size, MaxFileBytes)
	}
	if answer.Encoding != "base64" {
		return File{}, fmt.Errorf("%s at %s answered with encoding %q", filePath, ref, answer.Encoding)
	}
	content, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(answer.Content))
	if err != nil {
		return File{}, fmt.Errorf("decode %s at %s: %w", filePath, ref, err)
	}
	if len(content) > MaxFileBytes {
		return File{}, fmt.Errorf("%s at %s is larger than the %d-byte limit", filePath, ref, MaxFileBytes)
	}
	return File{Path: answer.Path, Blob: answer.SHA, Content: content}, nil
}

// Commits returns at most limit commits that touched the path, newest first.
func (c *Client) Commits(ctx context.Context, filePath string, limit int) ([]Commit, error) {
	if limit <= 0 {
		return nil, nil
	}
	query := url.Values{"path": {filePath}, "per_page": {strconv.Itoa(limit)}}
	var answer []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	apiPath := "/repos/" + c.Repository + "/commits?" + query.Encode()
	if err := c.do(ctx, http.MethodGet, apiPath, &answer); err != nil {
		return nil, fmt.Errorf("list commits of %s: %w", filePath, err)
	}
	commits := make([]Commit, 0, len(answer))
	for _, item := range answer {
		commits = append(commits, Commit{SHA: item.SHA, Message: item.Commit.Message, Date: item.Commit.Committer.Date})
	}
	return commits, nil
}

func (c *Client) baseURL() string {
	if c.APIURL == "" {
		return DefaultGitHubAPI
	}
	return strings.TrimRight(c.APIURL, "/")
}

// do sends one read request and decodes its JSON answer into out.
func (c *Client) do(ctx context.Context, method, apiPath string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+apiPath, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if c.Token != nil {
		token, err := c.Token()
		if err != nil {
			return fmt.Errorf("read the GitHub token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("GitHub answered 404: %w", ErrNotFound)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("GitHub answered %d: %w", resp.StatusCode, ErrPermissionDenied)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("GitHub answered %d: %s", resp.StatusCode, summarize(raw))
	}
	return json.Unmarshal(raw, out)
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
