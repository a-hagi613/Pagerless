package main

import (
	"bufio"
	"bytes"
	"context"
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

func initializeRepository(ctx context.Context, directory, source, revision string) error {
	if source == "" || revision == "" {
		return fmt.Errorf("the source URL and base revision are required")
	}
	if err := os.MkdirAll(path.Dir(directory), 0o750); err != nil {
		return fmt.Errorf("create the repository directory: %w", err)
	}
	if err := runGit(ctx, "", "init", "--bare", "--initial-branch=main", directory); err != nil {
		return err
	}
	if err := runGit(ctx, "", "--git-dir", directory, "fetch", "--depth=1", source, revision); err != nil {
		return fmt.Errorf("fetch the pinned Sandbox base revision: %w", err)
	}
	if err := runGit(ctx, "", "--git-dir", directory, "update-ref", "refs/heads/main", "FETCH_HEAD"); err != nil {
		return err
	}
	if err := runGit(ctx, "", "--git-dir", directory, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	if err := runGit(ctx, "", "--git-dir", directory, "config", "http.receivepack", "true"); err != nil {
		return err
	}
	return nil
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
