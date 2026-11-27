package githubpublish

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
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is GitHub's REST endpoint.
const DefaultAPIURL = "https://api.github.com"

// maxBody bounds one REST response.
const maxBody = 8 << 20

// queryRef is the contents endpoint's revision parameter.
const queryRef = "ref"

// REST is the GitHub REST implementation of Reader and Writer. Two instances
// are built in production, one with the read credential and one with the write
// credential, so neither path can use the other's authority.
type REST struct {
	// BaseURL is the API root, without a trailing slash. GitHub Enterprise
	// Server uses https://host/api/v3.
	BaseURL string
	// Owner and Name identify the repository.
	Owner string
	Name  string
	// Token returns the bearer credential for every request.
	Token func(ctx context.Context) (string, error)
	// HTTP performs requests.
	HTTP *http.Client
}

// NewREST builds a REST client for one repository.
func NewREST(
	apiURL, repository string, token func(context.Context) (string, error), client *http.Client,
) (*REST, error) {
	owner, name, found := strings.Cut(strings.TrimSpace(repository), "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("github: %q is not an owner/name repository", repository)
	}
	if strings.TrimSpace(apiURL) == "" {
		apiURL = DefaultAPIURL
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &REST{
		BaseURL: strings.TrimSuffix(apiURL, "/"),
		Owner:   owner,
		Name:    name,
		Token:   token,
		HTTP:    client,
	}, nil
}

// Head reads a branch commit.
func (r *REST) Head(ctx context.Context, ref string) (string, bool, error) {
	if strings.TrimSpace(ref) == "" || strings.Contains(ref, "/heads/") {
		return "", false, fmt.Errorf("github: %q is not a branch name", ref)
	}
	var response struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	status, err := r.do(ctx, http.MethodGet, "/git/ref/heads/"+url.PathEscape(ref), nil, &response)
	if status == http.StatusNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if response.Object.SHA == "" {
		return "", false, errors.New("github: the branch reference carries no commit")
	}
	return response.Object.SHA, true, nil
}

// File reads one file at a revision.
func (r *REST) File(ctx context.Context, path, ref string) ([]byte, bool, error) {
	var response struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	query := url.Values{queryRef: []string{ref}}
	status, err := r.do(ctx, http.MethodGet,
		"/contents/"+escapePath(path)+"?"+query.Encode(), nil, &response)
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if response.Type != "file" {
		return nil, false, fmt.Errorf("github: %q is not a file", path)
	}
	if response.Encoding != "" && response.Encoding != "base64" {
		return nil, false, fmt.Errorf("github: %q uses the unsupported encoding %q", path, response.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(response.Content, "\n", ""))
	if err != nil {
		return nil, false, fmt.Errorf("github: decode %q: %w", path, err)
	}
	return decoded, true, nil
}

// BlobSHA returns the blob identity of a file on a branch.
func (r *REST) BlobSHA(ctx context.Context, branch, path string) (string, error) {
	var response struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	}
	query := url.Values{queryRef: []string{branch}}
	status, err := r.do(ctx, http.MethodGet, "/contents/"+escapePath(path)+"?"+query.Encode(), nil, &response)
	if status == http.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if response.Type != "" && response.Type != "file" {
		return "", fmt.Errorf("github: %q is not a file", path)
	}
	return response.SHA, nil
}

// CreateBranch creates a branch at a commit.
func (r *REST) CreateBranch(ctx context.Context, branch, sha string) (bool, error) {
	payload := map[string]string{"ref": "refs/heads/" + branch, "sha": sha}
	status, err := r.do(ctx, http.MethodPost, "/git/refs", payload, nil)
	if status == http.StatusUnprocessableEntity || status == http.StatusConflict {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// PutFile commits one file on a branch.
func (r *REST) PutFile(ctx context.Context, branch, path, content, message, blobSHA string) error {
	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	if blobSHA != "" {
		payload["sha"] = blobSHA
	}
	status, err := r.do(ctx, http.MethodPut, "/contents/"+escapePath(path), payload, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("github: commit %q answered HTTP %d", path, status)
	}
	return nil
}

// FindPullRequest returns the open pull request whose head is branch.
func (r *REST) FindPullRequest(ctx context.Context, branch string) (*PullRequest, bool, error) {
	query := url.Values{"state": []string{"open"}, "head": []string{r.Owner + ":" + branch}}
	var response []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	status, err := r.do(ctx, http.MethodGet, "/pulls?"+query.Encode(), nil, &response)
	if err != nil {
		return nil, false, err
	}
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("github: list the pull requests answered HTTP %d", status)
	}
	if len(response) == 0 {
		return nil, false, nil
	}
	return &PullRequest{Number: response[0].Number, URL: response[0].HTMLURL}, true, nil
}

// OpenDraftPullRequest opens one draft pull request.
func (r *REST) OpenDraftPullRequest(ctx context.Context, request DraftRequest) (PullRequest, error) {
	payload := map[string]any{
		"title": request.Title,
		"head":  request.Head,
		"base":  request.Base,
		"body":  request.Body,
		"draft": true,
	}
	var response struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	status, err := r.do(ctx, http.MethodPost, "/pulls", payload, &response)
	if err != nil {
		return PullRequest{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return PullRequest{}, fmt.Errorf("github: open the pull request answered HTTP %d", status)
	}
	if response.HTMLURL == "" {
		return PullRequest{}, errors.New("github: the pull request response carries no URL")
	}
	return PullRequest{Number: response.Number, URL: response.HTMLURL}, nil
}

// do performs one request and decodes a successful response. A non-success
// status is returned with the status so a caller can treat 404 and 422 as
// meaningful, and is also returned as an error otherwise.
func (r *REST) do(ctx context.Context, method, path string, payload, result any) (int, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("github: encode the request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.BaseURL+"/repos/"+r.Owner+"/"+r.Name+path, body)
	if err != nil {
		return 0, fmt.Errorf("github: build the request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "pagerless")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if r.Token != nil {
		token, err := r.Token(ctx)
		if err != nil {
			return 0, fmt.Errorf("github: read the credential: %w", err)
		}
		if token == "" {
			return 0, errors.New("github: the credential is empty")
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := r.HTTP.Do(request)
	if err != nil {
		return 0, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return response.StatusCode, fmt.Errorf("github: read the response: %w", err)
	}
	if len(raw) > maxBody {
		return response.StatusCode, fmt.Errorf("github: the response exceeds %d bytes", maxBody)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(raw))
		if len(message) > 300 {
			message = message[:300]
		}
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnprocessableEntity ||
			response.StatusCode == http.StatusConflict {
			return response.StatusCode, nil
		}
		return response.StatusCode, fmt.Errorf("github: %s %s answered HTTP %s: %s",
			method, path, strconv.Itoa(response.StatusCode), message)
	}
	if result != nil {
		if err := json.Unmarshal(raw, result); err != nil {
			return response.StatusCode, fmt.Errorf("github: decode the response: %w", err)
		}
	}
	return response.StatusCode, nil
}

// escapePath encodes each path segment without escaping the separators.
func escapePath(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// RepositoryInfo is the metadata readiness uses to distinguish a verified
// repository read from an unverified write capability.
type RepositoryInfo struct {
	FullName      string          `json:"full_name"`
	DefaultBranch string          `json:"default_branch"`
	Permissions   map[string]bool `json:"permissions"`
}

// Repository reads the repository with this client's credential.
func (r *REST) Repository(ctx context.Context) (RepositoryInfo, error) {
	var info RepositoryInfo
	status, err := r.do(ctx, http.MethodGet, "", nil, &info)
	if err != nil {
		return RepositoryInfo{}, err
	}
	if status != http.StatusOK {
		return RepositoryInfo{}, fmt.Errorf("github: read the repository answered HTTP %d", status)
	}
	if info.FullName == "" {
		return RepositoryInfo{}, errors.New("github: the repository response carries no name")
	}
	return info, nil
}
