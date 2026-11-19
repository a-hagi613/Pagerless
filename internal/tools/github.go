package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultGitHubAPI is where a read goes when no base URL is configured.
	defaultGitHubAPI = "https://api.github.com"
	// githubTimeout bounds one read.
	githubTimeout = 20 * time.Second
	// githubResponseLimit bounds one response body.
	githubResponseLimit = 2 << 20
	// maxRepoFileBytes bounds one file read.
	maxRepoFileBytes = 64 << 10
	// maxTreeEntries bounds one tree result.
	maxTreeEntries = 200
	// maxCommits bounds one commit list.
	maxCommits = 20
)

// GitHubReader is the read-only part of the GitHub API the broker needs. The
// writer lives in its own package and is never reachable from here.
type GitHubReader interface {
	// Tree lists the tree under a path at a ref.
	Tree(ctx context.Context, repository Repository, ref, directory string) (Tree, error)
	// File reads one file at a ref.
	File(ctx context.Context, repository Repository, ref, filePath string) (File, error)
	// Commits lists recent commits at a ref, optionally for one path.
	Commits(ctx context.Context, repository Repository, ref, filePath string, limit int) ([]Commit, error)
}

// TreeEntry is one path in a repository tree.
type TreeEntry struct {
	// Path is the path relative to the repository root.
	Path string `json:"path"`
	// Type is blob or tree.
	Type string `json:"type,omitempty"`
	// Size is the blob size in bytes.
	Size int64 `json:"size,omitempty"`
}

// Tree is one bounded tree listing.
type Tree struct {
	// Path is the directory listed, relative to the repository root.
	Path string `json:"path"`
	// Entries are the paths under it.
	Entries []TreeEntry `json:"entries"`
	// Truncated reports that the bound cut the listing.
	Truncated bool `json:"truncated,omitempty"`
}

// File is one repository file.
type File struct {
	// Path is the path relative to the repository root.
	Path string `json:"path"`
	// SHA is the Git blob SHA of the file at the ref.
	SHA string `json:"sha,omitempty"`
	// Ref is the ref it was read at.
	Ref string `json:"ref"`
	// Size is the file size in bytes.
	Size int64 `json:"size"`
	// Content is the decoded text.
	Content string `json:"content"`
	// Truncated reports that the bound cut the content.
	Truncated bool `json:"truncated,omitempty"`
}

// Commit is one repository commit.
type Commit struct {
	// SHA is the commit's hash.
	SHA string `json:"sha"`
	// Message is its first line.
	Message string `json:"message"`
	// Author is the commit author's name.
	Author string `json:"author,omitempty"`
	// At is when it was committed.
	At time.Time `json:"at,omitempty"`
	// Paths are the paths it touched, when the API returned them.
	Paths []string `json:"paths,omitempty"`
}

// HTTPGitHub reads GitHub's REST API with a token. The token is read for each
// request so a rotated Secret takes effect; it is never logged or returned.
type HTTPGitHub struct {
	// APIURL is the API base URL. It defaults to https://api.github.com.
	APIURL string
	// Token returns the read-scoped token.
	Token func() (string, error)
	// HTTPClient performs requests. It defaults to a client with a timeout.
	HTTPClient *http.Client
}

// baseURL returns the API base URL.
func (g *HTTPGitHub) baseURL() string {
	if g.APIURL == "" {
		return defaultGitHubAPI
	}
	return strings.TrimRight(g.APIURL, "/")
}

// Tree lists a bounded part of a repository tree.
func (g *HTTPGitHub) Tree(ctx context.Context, repository Repository, ref, directory string) (Tree, error) {
	query := url.Values{"recursive": {"1"}}
	requestPath := fmt.Sprintf("/repos/%s/%s/git/trees/%s?%s", repository.Owner, repository.Name,
		url.PathEscape(ref), query.Encode())
	var answer struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := g.get(ctx, requestPath, &answer); err != nil {
		return Tree{}, err
	}
	prefix := strings.Trim(path.Clean("/"+directory), "/")
	result := Tree{Path: prefix}
	for _, entry := range answer.Tree {
		if prefix != "" && entry.Path != prefix && !strings.HasPrefix(entry.Path, prefix+"/") {
			continue
		}
		if entry.Type != "blob" && entry.Type != "tree" {
			continue
		}
		result.Entries = append(result.Entries, TreeEntry{Path: entry.Path, Type: entry.Type, Size: entry.Size})
		if len(result.Entries) >= maxTreeEntries {
			result.Truncated = true
			break
		}
	}
	return result, nil
}

// File reads one repository file's decoded text.
func (g *HTTPGitHub) File(ctx context.Context, repository Repository, ref, filePath string) (File, error) {
	if !validRepositoryPath(filePath) {
		return File{}, fmt.Errorf("path %q is not a repository-relative path", filePath)
	}
	query := url.Values{"ref": {ref}}
	requestPath := fmt.Sprintf("/repos/%s/%s/contents/%s?%s", repository.Owner, repository.Name,
		escapePath(filePath), query.Encode())
	var answer struct {
		Path     string `json:"path"`
		SHA      string `json:"sha"`
		Size     int64  `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := g.get(ctx, requestPath, &answer); err != nil {
		return File{}, err
	}
	if answer.Encoding != "base64" {
		return File{}, fmt.Errorf("file %q is not base64-encoded text", filePath)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(answer.Content, "\n", ""))
	if err != nil {
		return File{}, fmt.Errorf("decode file %q: %w", filePath, err)
	}
	result := File{Path: answer.Path, SHA: answer.SHA, Ref: ref, Size: answer.Size}
	if len(decoded) > maxRepoFileBytes {
		decoded = decoded[:maxRepoFileBytes]
		result.Truncated = true
	}
	if !isText(decoded) {
		return File{}, fmt.Errorf("file %q is binary", filePath)
	}
	result.Content = string(decoded)
	return result, nil
}

// Commits lists recent commits at a ref.
func (g *HTTPGitHub) Commits(
	ctx context.Context, repository Repository, ref, filePath string, limit int,
) ([]Commit, error) {
	if limit <= 0 || limit > maxCommits {
		limit = 10
	}
	query := url.Values{"sha": {ref}, "per_page": {strconv.Itoa(limit)}}
	if filePath != "" {
		if !validRepositoryPath(filePath) {
			return nil, fmt.Errorf("path %q is not a repository-relative path", filePath)
		}
		query.Set("path", filePath)
	}
	requestPath := fmt.Sprintf("/repos/%s/%s/commits?%s", repository.Owner, repository.Name, query.Encode())
	var answer []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := g.get(ctx, requestPath, &answer); err != nil {
		return nil, err
	}
	commits := make([]Commit, 0, len(answer))
	for _, item := range answer {
		message := item.Commit.Message
		if index := strings.IndexByte(message, '\n'); index >= 0 {
			message = message[:index]
		}
		commits = append(commits, Commit{
			SHA: item.SHA, Message: message, Author: item.Commit.Author.Name, At: item.Commit.Author.Date,
		})
	}
	return commits, nil
}

// get sends one read and decodes its JSON answer.
func (g *HTTPGitHub) get(ctx context.Context, requestPath string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL()+requestPath, nil)
	if err != nil {
		return fmt.Errorf("build the GitHub read: %w", err)
	}
	if g.Token != nil {
		token, err := g.Token()
		if err != nil {
			return fmt.Errorf("read the GitHub token: %w", err)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpClient := g.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: githubTimeout}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("send the GitHub read: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, githubResponseLimit))
	if err != nil {
		return fmt.Errorf("read the GitHub answer: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("GitHub answered %d to a read", response.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode the GitHub answer: %w", err)
	}
	return nil
}

// escapePath escapes each segment of a repository path.
func escapePath(filePath string) string {
	segments := strings.Split(filePath, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// validRepositoryPath reports whether a path is a clean relative path.
func validRepositoryPath(filePath string) bool {
	if filePath == "" || strings.HasPrefix(filePath, "/") || strings.ContainsRune(filePath, 0) {
		return false
	}
	for segment := range strings.SplitSeq(filePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// isText reports whether a decoded file is likely text. A NUL byte within the
// first kilobyte marks a binary blob, which the model cannot use and which may
// carry a secret.
func isText(content []byte) bool {
	return !slices.Contains(content[:min(len(content), 1024)], byte(0))
}
