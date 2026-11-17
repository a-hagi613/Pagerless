package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type configuration struct {
	repository string
	repoRoot   string
	baseCommit string
	readToken  string
	writeToken string
}

type server struct {
	configuration
	mu  sync.Mutex
	prs []*pullRequest
}

type pullRequest struct {
	number      int
	title       string
	body        string
	head        string
	base        string
	state       string
	mergeCommit string
	autoMerge   bool
}

func newServer(cfg configuration) (*server, error) {
	if !validRepository(cfg.repository) || cfg.repoRoot == "" || cfg.baseCommit == "" || cfg.readToken == "" || cfg.writeToken == "" {
		return nil, errors.New("repository, bare repository, base commit, read token, and write token are required")
	}
	return &server{configuration: cfg}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/git/", s.serveGit)
	mux.HandleFunc("POST /graphql", s.graphql)
	mux.HandleFunc("POST /api/graphql", s.graphql)
	mux.HandleFunc("/repos/", s.rest)
	mux.HandleFunc("/api/v3/repos/", s.rest)
	return mux
}

func (s *server) controlRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /__e2e/checks/{number}", s.completeCheck)
	mux.HandleFunc("POST /__e2e/reset", s.reset)
	return mux
}

func (s *server) relay(target string, caFile string) (http.Handler, error) {
	remote, err := url.Parse(target)
	if err != nil || remote.Scheme != "https" || remote.Host == "" {
		return nil, errors.New("the relay target must be an HTTPS URL")
	}
	proxy := httputil.NewSingleHostReverseProxy(remote)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "adapter relay failed", http.StatusBadGateway)
	}
	if caFile != "" {
		transportTLS, err := tlsConfig(caFile)
		if err != nil {
			return nil, fmt.Errorf("load relay CA: %w", err)
		}
		proxy.Transport = &http.Transport{TLSClientConfig: transportTLS}
	}
	return proxy, nil
}

func (s *server) authorizeBearer(w http.ResponseWriter, r *http.Request, write bool) bool {
	authorization := r.Header.Get("Authorization")
	if authorization == "Bearer "+s.writeToken || (!write && authorization == "Bearer "+s.readToken) {
		return true
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
	return false
}

func (s *server) authorizeBasic(w http.ResponseWriter, r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	write := strings.Contains(r.URL.Path, "git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
	if ok && username != "" && (password == s.writeToken || (!write && password == s.readToken)) {
		return true
	}
	w.Header().Set("WWW-Authenticate", "Basic realm=\"pagerless-e2e-sandbox\"")
	http.Error(w, "Bad credentials", http.StatusUnauthorized)
	return false
}

func (s *server) repositoryPath(w http.ResponseWriter, r *http.Request) string {
	requestPath := strings.TrimPrefix(r.URL.Path, "/api/v3")
	parts := strings.Split(strings.Trim(requestPath, "/"), "/")
	if len(parts) < 4 || parts[0] != "repos" || parts[1]+"/"+parts[2] != s.repository {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return ""
	}
	return strings.Join(parts[3:], "/")
}

func (s *server) rest(w http.ResponseWriter, r *http.Request) {
	write := r.Method != http.MethodGet
	if !s.authorizeBearer(w, r, write) {
		return
	}
	resource := s.repositoryPath(w, r)
	if resource == "" {
		return
	}
	segments := strings.Split(resource, "/")
	switch {
	case r.Method == http.MethodGet && len(segments) == 1 && segments[0] == "pulls":
		s.listPullRequests(w, r)
	case r.Method == http.MethodPost && len(segments) == 1 && segments[0] == "pulls":
		s.openPullRequest(w, r)
	case r.Method == http.MethodGet && len(segments) == 2 && segments[0] == "pulls":
		s.getPullRequest(w, r, segments[1])
	case r.Method == http.MethodGet && len(segments) == 1 && segments[0] == "commits":
		s.listCommits(w, r)
	case r.Method == http.MethodGet && len(segments) >= 2 && segments[0] == "commits":
		s.getCommit(w, r, strings.Join(segments[1:], "/"))
	case r.Method == http.MethodGet && len(segments) >= 2 && segments[0] == "contents":
		s.getContent(w, r, strings.Join(segments[1:], "/"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (s *server) listPullRequests(w http.ResponseWriter, r *http.Request) {
	head := r.URL.Query().Get("head")
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]map[string]any, 0)
	for i := len(s.prs) - 1; i >= 0; i-- {
		pr := s.prs[i]
		if head != "" && head != repositoryOwner(s.repository)+":"+pr.head {
			continue
		}
		result = append(result, s.pullRequestJSON(pr))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) openPullRequest(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil ||
		!validRef(request.Head) || !validRef(request.Base) || request.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid pull request"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, current := range s.prs {
		if current.head == request.Head && current.state == "open" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"message": "Validation Failed", "errors": []map[string]string{{"message": "A pull request already exists for this branch"}},
			})
			return
		}
	}
	if !s.branchExists(r.Context(), request.Head) || !s.branchExists(r.Context(), request.Base) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "head or base is not a repository branch"})
		return
	}
	pr := &pullRequest{number: len(s.prs) + 1, title: request.Title, body: request.Body,
		head: request.Head, base: request.Base, state: "open"}
	s.prs = append(s.prs, pr)
	w.Header().Set("Location", fmt.Sprintf("/repos/%s/pulls/%d", s.repository, pr.number))
	writeJSON(w, http.StatusCreated, s.pullRequestJSON(pr))
}

func (s *server) getPullRequest(w http.ResponseWriter, r *http.Request, rawNumber string) {
	number, err := strconv.Atoi(rawNumber)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.findPullRequest(number)
	if pr == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, s.pullRequestJSON(pr))
}

func (s *server) graphql(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBearer(w, r, true) {
		return
	}
	var request struct {
		Query     string `json:"query"`
		Variables struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil ||
		!strings.Contains(request.Query, "enablePullRequestAutoMerge") || request.Variables.Method != "SQUASH" {
		writeJSON(w, http.StatusOK, map[string]any{"errors": []map[string]string{{"message": "unsupported query"}}})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pr := range s.prs {
		if request.Variables.ID != fmt.Sprintf("PR_node_%d", pr.number) {
			continue
		}
		if pr.state != "open" {
			writeJSON(w, http.StatusOK, map[string]any{"errors": []map[string]string{{"message": "Pull request is not open"}}})
			return
		}
		pr.autoMerge = true
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"enablePullRequestAutoMerge": map[string]any{"pullRequest": map[string]int{"number": pr.number}},
		}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"errors": []map[string]string{{"message": "Could not resolve to a node"}}})
}

func (s *server) completeCheck(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBearer(w, r, true) {
		return
	}
	var request struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&request); err != nil || request.Status != "success" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "the required manifest check must pass"})
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.findPullRequest(number)
	if pr == nil || pr.state != "open" || !pr.autoMerge {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "pull request is not eligible for auto-merge"})
		return
	}
	commit, err := s.squashMerge(r.Context(), pr)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "pull request did not merge"})
		return
	}
	pr.state, pr.mergeCommit = "merged", commit
	writeJSON(w, http.StatusOK, map[string]string{"merge_commit_sha": commit})
}

func (s *server) reset(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBearer(w, r, true) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	refs, err := gitOutput(r.Context(), s.repoRoot, "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "cannot list Sandbox branches"})
		return
	}
	for _, ref := range strings.Fields(refs) {
		if ref == "refs/heads/main" {
			continue
		}
		if err := runGit(r.Context(), "", "--git-dir", s.repoRoot, "update-ref", "-d", ref); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "cannot remove Scenario branches"})
			return
		}
	}
	if err := runGit(r.Context(), "", "--git-dir", s.repoRoot, "update-ref", "refs/heads/main", s.baseCommit); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "cannot reset Sandbox main"})
		return
	}
	s.prs = nil
	writeJSON(w, http.StatusOK, map[string]string{"main": s.baseCommit})
}

func (s *server) pullRequestJSON(pr *pullRequest) map[string]any {
	var mergedAt any
	var mergeCommit any
	if pr.state == "merged" {
		mergedAt = time.Now().UTC().Format(time.RFC3339)
		mergeCommit = pr.mergeCommit
	}
	var autoMerge any
	if pr.autoMerge && pr.state == "open" {
		autoMerge = map[string]string{"merge_method": "SQUASH"}
	}
	state := "open"
	if pr.state == "merged" {
		state = "closed"
	}
	return map[string]any{
		"number": pr.number, "node_id": fmt.Sprintf("PR_node_%d", pr.number),
		"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", s.repository, pr.number),
		"title":    pr.title, "body": pr.body, "head": map[string]any{"ref": pr.head},
		"base": map[string]any{"ref": pr.base}, "state": state, "merged": pr.state == "merged",
		"merged_at": mergedAt, "merge_commit_sha": mergeCommit, "auto_merge": autoMerge,
	}
}

func (s *server) findPullRequest(number int) *pullRequest {
	for _, pr := range s.prs {
		if pr.number == number {
			return pr
		}
	}
	return nil
}

func (s *server) branchExists(ctx context.Context, name string) bool {
	_, err := gitOutput(ctx, s.repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

func (s *server) squashMerge(ctx context.Context, pr *pullRequest) (string, error) {
	directory, err := os.MkdirTemp("", "pagerless-e2e-merge-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	work := path.Join(directory, "work")
	if err := runGit(ctx, directory, "clone", "--quiet", s.repoRoot, work); err != nil {
		return "", err
	}
	if err := runGit(ctx, work, "config", "user.name", "Pagerless E2E Adapter"); err != nil {
		return "", err
	}
	if err := runGit(ctx, work, "config", "user.email", "e2e@pagerless.dev"); err != nil {
		return "", err
	}
	if err := runGit(ctx, work, "checkout", "--quiet", "main"); err != nil {
		return "", err
	}
	if err := runGit(ctx, work, "merge", "--squash", "origin/"+pr.head); err != nil {
		return "", err
	}
	if err := runGit(ctx, work, "commit", "--quiet", "--message", pr.title); err != nil {
		return "", err
	}
	if err := runGit(ctx, work, "push", "--quiet", "origin", "HEAD:refs/heads/main"); err != nil {
		return "", err
	}
	commit, err := gitWorkingOutput(ctx, work, "rev-parse", "HEAD")
	return strings.TrimSpace(commit), err
}

func (s *server) listCommits(w http.ResponseWriter, r *http.Request) {
	args := []string{"log", "--format=%H%x00%s%x00%aI", "-20", "main"}
	if pathQuery := r.URL.Query().Get("path"); pathQuery != "" {
		if !validTreePath(pathQuery) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid path"})
			return
		}
		args = append(args, "--", pathQuery)
	}
	out, err := gitOutput(r.Context(), s.repoRoot, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "cannot read commits"})
		return
	}
	commits := make([]map[string]any, 0)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) != 3 {
			continue
		}
		commits = append(commits, map[string]any{
			"sha": fields[0], "commit": map[string]any{
				"message": fields[1], "author": map[string]string{"date": fields[2]},
				"committer": map[string]string{"date": fields[2]},
			},
		})
	}
	writeJSON(w, http.StatusOK, commits)
}

func (s *server) getCommit(w http.ResponseWriter, r *http.Request, revision string) {
	if strings.Contains(revision, "..") || strings.ContainsAny(revision, "\\\x00") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid commit"})
		return
	}
	out, err := gitOutput(r.Context(), s.repoRoot, "show", "--no-patch", "--format=%H%x00%P%x00%s%x00%aI", revision)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	fields := strings.Split(strings.TrimSpace(out), "\x00")
	if len(fields) != 4 {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "cannot read commit"})
		return
	}
	parents := make([]map[string]string, 0)
	for _, parent := range strings.Fields(fields[1]) {
		parents = append(parents, map[string]string{"sha": parent})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sha": fields[0], "commit": map[string]any{
			"message": fields[2], "author": map[string]string{"date": fields[3]},
			"committer": map[string]string{"date": fields[3]},
		}, "parents": parents,
	})
}

func (s *server) getContent(w http.ResponseWriter, r *http.Request, file string) {
	if !validTreePath(file) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid path"})
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = "main"
	}
	if strings.Contains(ref, "..") || strings.ContainsAny(ref, "\\\x00") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid ref"})
		return
	}
	content, err := gitOutputBytes(r.Context(), s.repoRoot, "show", ref+":"+file)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	shaRaw, err := gitOutput(r.Context(), s.repoRoot, "rev-parse", ref+":"+file)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	sha := strings.TrimSpace(shaRaw)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": path.Base(file), "path": file, "sha": sha, "size": len(content), "type": "file",
		"content": base64.StdEncoding.EncodeToString(content), "encoding": "base64",
	})
}

func gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	output, err := gitOutputBytes(ctx, directory, args...)
	return string(output), err
}

func gitWorkingOutput(ctx context.Context, directory string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func gitOutputBytes(ctx context.Context, directory string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"--git-dir", directory}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && validRef(parts[0]) && validRef(parts[1])
}

func validRef(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, "..~^:?*[\\\x00") && !strings.Contains(value, " ")
}

func validTreePath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../") && !strings.Contains(value, "\x00")
}

func repositoryOwner(value string) string {
	owner, _, _ := strings.Cut(value, "/")
	return owner
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
