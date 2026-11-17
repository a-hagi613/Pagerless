package testenv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// GitHubRepository is the owner/name of the Sandbox on the fake GitHub.
	GitHubRepository = "pagerless-test/sandbox"
	// GitHubWriteToken is the only token that the fake GitHub accepts.
	GitHubWriteToken = "ghp_fake_write_token"
)

// PullRequestState is the state of a pull request on the fake GitHub.
type PullRequestState string

const (
	// PROpen is an open pull request.
	PROpen PullRequestState = "open"
	// PRMerged is a merged pull request.
	PRMerged PullRequestState = "merged"
	// PRClosed is a pull request closed without merging.
	PRClosed PullRequestState = "closed"
)

// PullRequest is a pull request on the fake GitHub.
type PullRequest struct {
	// Number is the pull request number.
	Number int
	// Title, Body, Head, and Base are what the Executor sent.
	Title, Body, Head, Base string
	// State is open, merged, or closed.
	State PullRequestState
	// AutoMerge reports whether auto-merge is enabled, and AutoMergeMethod its merge method.
	AutoMerge       bool
	AutoMergeMethod string
	// MergeCommit is the commit on main that the merge produced, once merged.
	MergeCommit string
	// URL is the address of the pull request page.
	URL string
}

// FakeGitHub stands in for the parts of GitHub's REST and GraphQL APIs that
// the Executor uses: listing and opening pull requests, and enabling
// auto-merge. Its pull requests are backed by a bare repository, so that
// merging one produces a real commit on main. A test completes a pull
// request's check with CompleteChecks, which is what GitHub does when the
// Sandbox's required check passes.
type FakeGitHub struct {
	// URL is the base URL of the API.
	URL string

	t    testing.TB
	repo *BareRepo

	mu       sync.Mutex
	prs      []*PullRequest
	requests []string
	failures []*failure
}

type failure struct {
	method, pathContains string
	status               int
	remaining            int
}

// NewFakeGitHub starts a fake GitHub whose repository is the bare repository.
func NewFakeGitHub(t testing.TB, repo *BareRepo) *FakeGitHub {
	t.Helper()
	g := &FakeGitHub{t: t, repo: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/pulls", g.list)
	mux.HandleFunc("POST /repos/{owner}/{name}/pulls", g.open)
	mux.HandleFunc("POST /graphql", g.graphql)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.admit(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	g.URL = srv.URL
	return g
}

// Token returns the token the Executor must present, so the harness can configure it.
func (g *FakeGitHub) Token() string { return GitHubWriteToken }

// admit records the request and applies the token check and injected failures.
func (g *FakeGitHub) admit(w http.ResponseWriter, r *http.Request) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+GitHubWriteToken {
		writeJSON(w, http.StatusUnauthorized, message("Bad credentials"))
		return false
	}
	for _, f := range g.failures {
		if f.remaining > 0 && f.method == r.Method && strings.Contains(r.URL.Path, f.pathContains) {
			f.remaining--
			writeJSON(w, f.status, message("injected failure"))
			return false
		}
	}
	return true
}

// FailNext makes the next count requests with the method and a path
// containing the text fail with the status.
func (g *FakeGitHub) FailNext(method, pathContains string, status, count int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = append(g.failures, &failure{method: method, pathContains: pathContains, status: status, remaining: count})
}

// Requests returns every request received, as "METHOD /path", in order.
func (g *FakeGitHub) Requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// PullRequests returns a snapshot of the pull requests, oldest first.
func (g *FakeGitHub) PullRequests() []PullRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]PullRequest, 0, len(g.prs))
	for _, pr := range g.prs {
		out = append(out, *pr)
	}
	return out
}

// CompleteChecks stands in for the Sandbox's required check passing: GitHub
// merges the pull request if auto-merge is enabled. It returns the merge
// commit, and fails the test when auto-merge is not enabled, because nothing
// would merge the pull request then.
func (g *FakeGitHub) CompleteChecks(t testing.TB, number int) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := g.find(number)
	if pr == nil || pr.State != PROpen || !pr.AutoMerge {
		t.Fatalf("pull request %d does not merge when its check passes: %+v", number, pr)
	}
	pr.MergeCommit = g.repo.MergeBranch(pr.Head, pr.Title)
	pr.State = PRMerged
	return pr.MergeCommit
}

// Close closes the pull request without merging it.
func (g *FakeGitHub) Close(t testing.TB, number int) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := g.find(number)
	if pr == nil || pr.State != PROpen {
		t.Fatalf("pull request %d is not open: %+v", number, pr)
	}
	pr.State = PRClosed
}

func (g *FakeGitHub) find(number int) *PullRequest {
	for _, pr := range g.prs {
		if pr.Number == number {
			return pr
		}
	}
	return nil
}

func (g *FakeGitHub) json(pr *PullRequest) map[string]any {
	out := map[string]any{
		"number":           pr.Number,
		"node_id":          "PR_node_" + strconv.Itoa(pr.Number),
		"html_url":         pr.URL,
		"state":            "open",
		"merged_at":        nil,
		"merge_commit_sha": nil,
		"auto_merge":       nil,
	}
	if pr.State != PROpen {
		out["state"] = "closed"
	}
	if pr.State == PRMerged {
		out["merged_at"] = time.Now().UTC().Format(time.RFC3339)
		out["merge_commit_sha"] = pr.MergeCommit
	}
	if pr.AutoMerge && pr.State == PROpen {
		out["auto_merge"] = map[string]string{"merge_method": pr.AutoMergeMethod}
	}
	return out
}

func (g *FakeGitHub) ownsRepository(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("owner")+"/"+r.PathValue("name") != GitHubRepository {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return false
	}
	return true
}

func (g *FakeGitHub) list(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	head := r.URL.Query().Get("head")
	g.mu.Lock()
	defer g.mu.Unlock()
	found := []map[string]any{}
	owner, _, _ := strings.Cut(GitHubRepository, "/")
	for _, pr := range slices.Backward(g.prs) {
		if head == "" || head == owner+":"+pr.Head {
			found = append(found, g.json(pr))
		}
	}
	writeJSON(w, http.StatusOK, found)
}

func (g *FakeGitHub) open(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	var request struct {
		Title, Body, Head, Base string
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, message("Problems parsing JSON"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pr := range g.prs {
		if pr.Head == request.Head && pr.State == PROpen {
			writeJSON(w, http.StatusUnprocessableEntity,
				validationFailed("A pull request already exists for "+request.Head+"."))
			return
		}
	}
	if !g.branchExists(r.Context(), request.Head) || !g.branchExists(r.Context(), request.Base) {
		writeJSON(w, http.StatusUnprocessableEntity,
			validationFailed("head or base is not a branch of the repository"))
		return
	}
	number := len(g.prs) + 1
	pr := &PullRequest{
		Number: number, Title: request.Title, Body: request.Body, Head: request.Head, Base: request.Base,
		State: PROpen, URL: fmt.Sprintf("%s/%s/pull/%d", g.URL, GitHubRepository, number),
	}
	g.prs = append(g.prs, pr)
	writeJSON(w, http.StatusCreated, g.json(pr))
}

func (g *FakeGitHub) branchExists(ctx context.Context, name string) bool {
	_, err := gitOutput(ctx, g.repo.URL, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// graphql answers the one mutation the Executor sends: enablePullRequestAutoMerge.
func (g *FakeGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string
		Variables struct{ ID, Method string }
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
		!strings.Contains(request.Query, "enablePullRequestAutoMerge") {
		writeJSON(w, http.StatusOK, graphqlError("unsupported query"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pr := range g.prs {
		if "PR_node_"+strconv.Itoa(pr.Number) != request.Variables.ID {
			continue
		}
		if pr.State != PROpen {
			writeJSON(w, http.StatusOK, graphqlError("Pull request is not open"))
			return
		}
		pr.AutoMerge, pr.AutoMergeMethod = true, request.Variables.Method
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"enablePullRequestAutoMerge": map[string]any{"pullRequest": map[string]int{"number": pr.Number}},
		}})
		return
	}
	writeJSON(w, http.StatusOK, graphqlError("Could not resolve to a node"))
}

// message is the body of a REST error.
func message(text string) map[string]string { return map[string]string{"message": text} }

// validationFailed is the body of the 422 answer to a request that GitHub refuses.
func validationFailed(text string) map[string]any {
	return map[string]any{"message": "Validation Failed", "errors": []map[string]string{message(text)}}
}

// graphqlError is the body of a GraphQL answer that carries an error.
func graphqlError(text string) map[string]any {
	return map[string]any{"errors": []map[string]string{message(text)}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
