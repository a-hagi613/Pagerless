package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// ensureRepository returns the commit at refs/heads/main, initializing the bare
// repository from the pinned revision when it is absent or incomplete. The clone
// is staged beside the final path so a failed start cannot leave a partial
// repository that a later start would accept as valid.
func ensureRepository(ctx context.Context, directory, source, revision string) (string, error) {
	if commit, err := repositoryCommit(ctx, directory); err == nil {
		return commit, nil
	}
	if err := os.RemoveAll(directory); err != nil {
		return "", fmt.Errorf("remove the incomplete Sandbox repository: %w", err)
	}
	staging := directory + ".init"
	if err := os.RemoveAll(staging); err != nil {
		return "", fmt.Errorf("remove the staged Sandbox repository: %w", err)
	}
	commit, err := initializeRepository(ctx, staging, source, revision)
	if err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	if err := os.Rename(staging, directory); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("install the Sandbox repository: %w", err)
	}
	return commit, nil
}

func repositoryCommit(ctx context.Context, directory string) (string, error) {
	output, err := gitOutput(ctx, directory, "rev-parse", "--verify", "refs/heads/main^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(output)
	if !validCommit(commit) {
		return "", errors.New("the Sandbox repository has no commit at refs/heads/main")
	}
	return commit, nil
}

func initializeRepository(ctx context.Context, directory, source, revision string) (string, error) {
	if source == "" || revision == "" {
		return "", fmt.Errorf("the source URL and base revision are required")
	}
	if err := os.MkdirAll(path.Dir(directory), 0o750); err != nil {
		return "", fmt.Errorf("create the repository directory: %w", err)
	}
	if err := runGit(ctx, "", "init", "--bare", "--initial-branch=main", directory); err != nil {
		return "", err
	}
	if err := runGit(ctx, "", "--git-dir", directory, "fetch", "--depth=1", source, revision); err != nil {
		return "", fmt.Errorf("fetch the pinned Sandbox base revision: %w", err)
	}
	// Validation of an unschedulable Workload compares against the request that
	// the Sandbox's known-good tag declares, so the tag must exist locally.
	if err := runGit(ctx, "", "--git-dir", directory, "fetch", "--depth=1", source,
		"refs/tags/known-good:refs/tags/known-good"); err != nil {
		return "", fmt.Errorf("fetch the Sandbox known-good tag: %w", err)
	}
	// The pinned revision may be an annotated tag. A branch tip must be a
	// commit, so peel it before updating the reference.
	output, err := gitOutput(ctx, directory, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve the pinned Sandbox revision to a commit: %w", err)
	}
	commit := strings.TrimSpace(output)
	if !validCommit(commit) {
		return "", errors.New("the pinned Sandbox revision does not resolve to a commit")
	}
	if err := runGit(ctx, "", "--git-dir", directory, "update-ref", "refs/heads/main", commit); err != nil {
		return "", err
	}
	if err := runGit(ctx, "", "--git-dir", directory, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return "", err
	}
	if err := runGit(ctx, "", "--git-dir", directory, "config", "http.receivepack", "true"); err != nil {
		return "", err
	}
	return commit, nil
}

func validCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func runGit(ctx context.Context, directory string, args ...string) error {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Pagerless E2E Adapter", "GIT_AUTHOR_EMAIL=e2e@pagerless.dev",
		"GIT_COMMITTER_NAME=Pagerless E2E Adapter", "GIT_COMMITTER_EMAIL=e2e@pagerless.dev")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s *server) serveGit(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/git/" || r.URL.Path == "/git" {
		http.NotFound(w, r)
		return
	}
	if !s.authorizeBasic(w, r) {
		return
	}
	pathInfo := strings.TrimPrefix(r.URL.EscapedPath(), "/git")
	repositoryPath := "/" + s.repository + ".git"
	outsideRepository := !strings.HasPrefix(pathInfo, repositoryPath+"/") && pathInfo != repositoryPath
	if !strings.HasPrefix(pathInfo, "/") || strings.Contains(pathInfo, "..") || outsideRepository {
		http.NotFound(w, r)
		return
	}
	command := exec.CommandContext(r.Context(), "git", "http-backend")
	contentLength := max(r.ContentLength, 0)
	command.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_PROJECT_ROOT="+path.Dir(path.Dir(s.repoRoot)),
		"GIT_HTTP_EXPORT_ALL=1", "PATH_INFO="+pathInfo, "QUERY_STRING="+r.URL.RawQuery,
		"REQUEST_METHOD="+r.Method, "CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"CONTENT_LENGTH="+strconv.FormatInt(contentLength, 10), "REMOTE_ADDR="+remoteAddress(r),
		"REMOTE_USER=pagerless-e2e", "AUTH_TYPE=Basic", "SERVER_PROTOCOL="+r.Proto,
		"GATEWAY_INTERFACE=CGI/1.1", "SERVER_SOFTWARE=pagerless-e2e-adapter",
		"HTTP_GIT_PROTOCOL="+r.Header.Get("Git-Protocol"))
	command.Stdin = r.Body
	var output bytes.Buffer
	command.Stdout = &output
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		http.Error(w, "Git request failed", http.StatusBadGateway)
		return
	}
	response := bytes.NewReader(output.Bytes())
	bodyReader := bufio.NewReader(response)
	reader := textproto.NewReader(bodyReader)
	headers, err := reader.ReadMIMEHeader()
	if err != nil {
		http.Error(w, "Git backend returned an invalid response", http.StatusBadGateway)
		return
	}
	status := http.StatusOK
	if raw := headers.Get("Status"); raw != "" {
		value, _, _ := strings.Cut(raw, " ")
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil {
			status = parsed
		}
	}
	for key, values := range headers {
		if strings.EqualFold(key, "Status") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, bodyReader)
}

func remoteAddress(r *http.Request) string {
	if host, _, found := strings.Cut(r.RemoteAddr, ":"); found {
		return host
	}
	return r.RemoteAddr
}
