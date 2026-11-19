package testenv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	// fieldSHA is the GitHub request and answer field that carries a Git object SHA.
	fieldSHA = "sha"

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
	// Title, Body, Head, and Base are what the writer sent.
	Title, Body, Head, Base string
	// Draft reports whether the pull request was opened as a draft.
	Draft bool
	// State is open, merged, or closed.
	State PullRequestState
	// MergeCommit is the commit on main that the merge produced, once merged.
	MergeCommit string
	// URL is the address of the pull request page.
	URL string
}

// FakeGitHub stands in for the parts of GitHub's REST API that Pagerless uses:
// the Git Data API that creates the branch and commit, and the Pull Request
// API. Its repository is a bare git repository, so every object the fake
// creates is a real one and the merge that a test triggers produces a real
// commit on main.
//
// It has no merge or auto-merge endpoint, because Pagerless never merges. A
// test plays the Operator with CompleteChecks, which merges once the Target
// Repository's Checks pass.
type FakeGitHub struct {
	// URL is the base URL of the API.
	URL string

	t    testing.TB
	repo *BareRepo

	mu       sync.Mutex
	prs      []*PullRequest
	comments map[int][]string
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
	g := &FakeGitHub{t: t, repo: repo, comments: map[int][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/git/ref/heads/{branch...}", g.readRef)
	mux.HandleFunc("POST /repos/{owner}/{name}/git/refs", g.createRef)
	mux.HandleFunc("POST /repos/{owner}/{name}/git/blobs", g.createBlob)
	mux.HandleFunc("POST /repos/{owner}/{name}/git/trees", g.createTree)
	mux.HandleFunc("POST /repos/{owner}/{name}/git/commits", g.createCommit)
	mux.HandleFunc("GET /repos/{owner}/{name}/git/commits/{sha}", g.readCommit)
	mux.HandleFunc("GET /repos/{owner}/{name}/pulls", g.list)
	mux.HandleFunc("POST /repos/{owner}/{name}/pulls", g.open)
	mux.HandleFunc("GET /repos/{owner}/{name}/pulls/{number}", g.read)
	mux.HandleFunc("PATCH /repos/{owner}/{name}/pulls/{number}", g.update)
	mux.HandleFunc("POST /repos/{owner}/{name}/issues/{number}/comments", g.comment)
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

// Token returns the token the writer must present, so the harness can configure it.
func (g *FakeGitHub) Token() string { return GitHubWriteToken }

// GraphQLRequests returns every GraphQL request received. The fake serves no
// GraphQL endpoint, because Pagerless has no auto-merge mutation to send.
func (g *FakeGitHub) GraphQLRequests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var found []string
	for _, request := range g.requests {
		if strings.Contains(request, "/graphql") {
			found = append(found, request)
		}
	}
	return found
}

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
	return append([]string(nil), g.requests...)
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

// Comments returns the comments on a pull request, oldest first.
func (g *FakeGitHub) Comments(number int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.comments[number]...)
}

// CompleteChecks stands in for the Target Repository's required Checks passing
// and the Operator merging the Pull Request. Pagerless never merges, so the
// harness plays the Operator. It returns the merge commit.
func (g *FakeGitHub) CompleteChecks(t testing.TB, number int) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := g.find(number)
	if pr == nil || pr.State != PROpen {
		t.Fatalf("pull request %d is not open: %+v", number, pr)
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
		"html_url":         pr.URL,
		"state":            "open",
		"draft":            pr.Draft,
		"merged_at":        nil,
		"merge_commit_sha": nil,
	}
	if pr.State != PROpen {
		out["state"] = "closed"
	}
	if pr.State == PRMerged {
		out["merged_at"] = time.Now().UTC().Format(time.RFC3339)
		out["merge_commit_sha"] = pr.MergeCommit
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

// git runs git in the bare repository, with stdin, outside the test goroutine.
func (g *FakeGitHub) git(ctx context.Context, stdin string, args ...string) (string, error) {
	return g.gitWithIndex(ctx, "", stdin, args...)
}

func (g *FakeGitHub) branchRef(branch string) string { return "refs/heads/" + branch }

func (g *FakeGitHub) readRef(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	branch := r.PathValue("branch")
	g.mu.Lock()
	defer g.mu.Unlock()
	sha, err := g.git(r.Context(), "", "rev-parse", "--verify", "--quiet", g.branchRef(branch)+"^{commit}")
	if err != nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":    g.branchRef(branch),
		"object": map[string]string{fieldSHA: sha, "type": "commit"},
	})
}

func (g *FakeGitHub) createRef(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	var request struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !strings.HasPrefix(request.Ref, "refs/heads/") {
		writeJSON(w, http.StatusUnprocessableEntity, validationFailed("ref must name a branch"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	zero := strings.Repeat("0", len(request.SHA))
	if _, err := g.git(r.Context(), "", "update-ref", request.Ref, request.SHA, zero); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, validationFailed("Reference already exists"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ref":    request.Ref,
		"object": map[string]string{fieldSHA: request.SHA, "type": "commit"},
	})
}

func (g *FakeGitHub) createBlob(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	var request struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Encoding != "utf-8" {
		writeJSON(w, http.StatusUnprocessableEntity, validationFailed("a blob must be utf-8 content"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	sha, err := g.git(r.Context(), request.Content, "hash-object", "-w", "--stdin")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, message(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{fieldSHA: sha})
}

func (g *FakeGitHub) createTree(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	var request struct {
		BaseTree string      `json:"base_tree"`
		Tree     []treeEntry `json:"tree"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.BaseTree == "" {
		writeJSON(w, http.StatusUnprocessableEntity, validationFailed("a tree needs a base_tree"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	index := filepath.Join(g.t.TempDir(), "index")
	ctx := r.Context()
	if _, err := g.gitWithIndex(ctx, index, "", "read-tree", request.BaseTree); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, validationFailed(err.Error()))
		return
	}
	for _, entry := range request.Tree {
		if entry.Mode != "100644" || entry.Type != "blob" {
			writeJSON(w, http.StatusUnprocessableEntity, validationFailed("unsupported tree entry"))
			return
		}
		if _, err := g.gitWithIndex(ctx, index, "", "update-index", "--add", "--cacheinfo",
			entry.Mode+","+entry.SHA+","+entry.Path); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, validationFailed(err.Error()))
			return
		}
	}
	sha, err := g.gitWithIndex(ctx, index, "", "write-tree")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, message(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{fieldSHA: sha})
}

// treeEntry is one entry of a Git tree request.
type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

func (g *FakeGitHub) createCommit(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	var request struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Tree == "" || len(request.Parents) != 1 {
		writeJSON(w, http.StatusUnprocessableEntity, validationFailed("a commit needs one tree and one parent"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	args := []string{"commit-tree", request.Tree, "-p", request.Parents[0], "-F", "-"}
	sha, err := g.git(r.Context(), request.Message, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, message(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		fieldSHA: sha,
		"tree":   map[string]string{fieldSHA: request.Tree},
	})
}

func (g *FakeGitHub) readCommit(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	tree, err := g.git(r.Context(), "", "rev-parse", "--verify", "--quiet", r.PathValue("sha")+"^{tree}")
	if err != nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{fieldSHA: r.PathValue("sha"), "tree": map[string]string{fieldSHA: tree}})
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
	for _, pr := range g.prs {
		if head == "" || head == owner+":"+pr.Head {
			found = append(found, g.json(pr))
		}
	}
	writeJSON(w, http.StatusOK, found)
}

func (g *FakeGitHub) read(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := g.find(number)
	if pr == nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return
	}
	writeJSON(w, http.StatusOK, g.json(pr))
}

func (g *FakeGitHub) open(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	var request struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
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
		Draft: request.Draft, State: PROpen,
		URL: fmt.Sprintf("%s/%s/pull/%d", g.URL, GitHubRepository, number),
	}
	g.prs = append(g.prs, pr)
	writeJSON(w, http.StatusCreated, g.json(pr))
}

func (g *FakeGitHub) update(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return
	}
	var request struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, message("Problems parsing JSON"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := g.find(number)
	if pr == nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return
	}
	// Closing a merged pull request changes nothing, as on GitHub.
	if request.State == "closed" && pr.State == PROpen {
		pr.State = PRClosed
	}
	writeJSON(w, http.StatusOK, g.json(pr))
}

func (g *FakeGitHub) comment(w http.ResponseWriter, r *http.Request) {
	if !g.ownsRepository(w, r) {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return
	}
	var request struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Body == "" {
		writeJSON(w, http.StatusUnprocessableEntity, validationFailed("a comment needs a body"))
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.find(number) == nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))
		return
	}
	g.comments[number] = append(g.comments[number], request.Body)
	writeJSON(w, http.StatusCreated, map[string]string{"body": request.Body})
}

func (g *FakeGitHub) branchExists(ctx context.Context, name string) bool {
	_, err := gitOutput(ctx, g.repo.URL, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// message is the body of a REST error.
func message(text string) map[string]string { return map[string]string{"message": text} }

// validationFailed is the body of the 422 answer to a request that GitHub refuses.
func validationFailed(text string) map[string]any {
	return map[string]any{"message": "Validation Failed", "errors": []map[string]string{message(text)}}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// gitWithIndex runs git in the bare repository with an optional private index,
// for tree construction.
func (g *FakeGitHub) gitWithIndex(ctx context.Context, index, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", g.repo.URL}, args...)...)
	cmd.Dir = g.repo.URL
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Pagerless Test", "GIT_AUTHOR_EMAIL=test@pagerless.dev",
		"GIT_COMMITTER_NAME=Pagerless Test", "GIT_COMMITTER_EMAIL=test@pagerless.dev",
	)
	if index != "" {
		env = append(env, "GIT_INDEX_FILE="+index)
	}
	cmd.Env = env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
