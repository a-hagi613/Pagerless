package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubAPIAndGitTransportRequireCheckBeforeSquashMerge(t *testing.T) {
	bare, baseCommit := testRepository(t)
	adapter, err := newServer(configuration{repository: "pagerless-test/sandbox", repoRoot: bare,
		baseCommit: baseCommit, readToken: "read-token", writeToken: "write-token"})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(adapter.routes())
	t.Cleanup(api.Close)
	control := httptest.NewServer(adapter.controlRoutes())
	t.Cleanup(control.Close)
	commits := requestAdapterWithToken(t, api.URL, "read-token", "GET", "/api/v3/repos/pagerless-test/sandbox/commits?per_page=5", nil)
	if commits.status != http.StatusOK || !strings.Contains(string(commits.body), baseCommit) {
		t.Fatalf("the GitHub MCP commit-list route returned %d: %s", commits.status, commits.body)
	}
	contents := requestAdapterWithToken(t, api.URL, "read-token", "GET", "/api/v3/repos/pagerless-test/sandbox/contents/workload.yaml?ref=main", nil)
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(contents.body, &file); err != nil || contents.status != http.StatusOK || file.Encoding != "base64" {
		t.Fatalf("the GitHub MCP file route returned %d: %s (%v)", contents.status, contents.body, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(file.Content)
	if err != nil || string(decoded) != "memory: 64Mi\n" {
		t.Fatalf("the GitHub MCP file route returned unexpected content %q (%v)", decoded, err)
	}
	deniedPR := requestAdapterWithToken(t, api.URL, "read-token", "POST", "/repos/pagerless-test/sandbox/pulls", map[string]string{
		"title": "Unauthorized", "body": "read token must not write", "head": "pagerless/recovery", "base": "main",
	})
	if deniedPR.status != http.StatusUnauthorized {
		t.Fatalf("the read-only token opened a pull request: %d %s", deniedPR.status, deniedPR.body)
	}

	work := filepath.Join(t.TempDir(), "work")
	gitWithCredentials(t, t.TempDir(), api.URL+"/git/pagerless-test/sandbox.git", "read-token", "clone", api.URL+"/git/pagerless-test/sandbox.git", work)
	gitTest(t, work, "config", "user.name", "E2E Test")
	gitTest(t, work, "config", "user.email", "e2e-test@pagerless.dev")
	gitTest(t, work, "checkout", "-b", "pagerless/recovery")
	if err := os.WriteFile(filepath.Join(work, "workload.yaml"), []byte("memory: 512Mi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, work, "add", "workload.yaml")
	gitTest(t, work, "commit", "--message", "Restore the Workload memory request")
	readPush := exec.Command("git", "push", "origin", "HEAD:refs/heads/pagerless/read-denied")
	readPush.Dir = work
	readPush.Env = append(os.Environ(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http."+api.URL+"/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+base64TestToken("read-token"), "GIT_TERMINAL_PROMPT=0")
	if output, err := readPush.CombinedOutput(); err == nil {
		t.Fatalf("the read-only token pushed a branch: %s", output)
	}
	gitWithCredentials(t, work, api.URL+"/git/pagerless-test/sandbox.git", "write-token", "push", "origin", "HEAD:refs/heads/pagerless/recovery")

	opened := requestAdapterWithToken(t, api.URL, "write-token", "POST", "/repos/pagerless-test/sandbox/pulls", map[string]string{
		"title": "Restore Workload memory", "body": "Validated change", "head": "pagerless/recovery", "base": "main",
	})
	if opened.status != http.StatusCreated {
		t.Fatalf("opening the pull request returned %d: %s", opened.status, opened.body)
	}
	var pr struct {
		Number int    `json:"number"`
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(opened.body, &pr); err != nil || pr.Number != 1 || pr.NodeID != "PR_node_1" {
		t.Fatalf("unexpected pull request response: %s (%v)", opened.body, err)
	}
	graphql := map[string]any{
		"query":     "mutation { enablePullRequestAutoMerge(input: {}) { pullRequest { number } } }",
		"variables": map[string]string{"id": pr.NodeID, "method": "SQUASH"},
	}
	if response := requestAdapterWithToken(t, api.URL, "write-token", "POST", "/api/graphql", graphql); response.status != http.StatusOK {
		t.Fatalf("enabling auto-merge returned %d: %s", response.status, response.body)
	}

	failedCheck := requestAdapterWithToken(t, control.URL, "write-token", "POST", "/__e2e/checks/1", map[string]string{"status": "failure"})
	if failedCheck.status != http.StatusBadRequest || gitRev(t, bare, "refs/heads/main") != baseCommit {
		t.Fatalf("a failing required check changed main: status=%d body=%s", failedCheck.status, failedCheck.body)
	}
	merged := requestAdapterWithToken(t, control.URL, "write-token", "POST", "/__e2e/checks/1", map[string]string{"status": "success"})
	if merged.status != http.StatusOK {
		t.Fatalf("a passing required check returned %d: %s", merged.status, merged.body)
	}
	var result struct {
		MergeCommit string `json:"merge_commit_sha"`
	}
	if err := json.Unmarshal(merged.body, &result); err != nil || result.MergeCommit == "" {
		t.Fatalf("the successful check did not return a merge commit: %s (%v)", merged.body, err)
	}
	if head := gitRev(t, bare, "refs/heads/main"); head != result.MergeCommit || head == baseCommit {
		t.Fatalf("main is %s after merge, expected merge commit %s after %s", head, result.MergeCommit, baseCommit)
	}
	if content := strings.TrimSpace(gitTest(t, bare, "show", "main:workload.yaml")); content != "memory: 512Mi" {
		t.Fatalf("main does not contain the pull request's change: %q", content)
	}
	closed := requestAdapterWithToken(t, api.URL, "read-token", "GET", "/api/v3/repos/pagerless-test/sandbox/pulls/1", nil)
	if closed.status != http.StatusOK || !strings.Contains(string(closed.body), `"merged":true`) {
		t.Fatalf("the API does not report the pull request merged: %d %s", closed.status, closed.body)
	}
	if reset := requestAdapterWithToken(t, control.URL, "read-token", "POST", "/__e2e/reset", map[string]string{}); reset.status != http.StatusUnauthorized {
		t.Fatalf("the read-only token reset the adapter: %d %s", reset.status, reset.body)
	}
	if reset := requestAdapterWithToken(t, api.URL, "write-token", "POST", "/__e2e/reset", map[string]string{}); reset.status != http.StatusNotFound {
		t.Fatalf("the HTTPS GitHub surface exposed the control API: %d %s", reset.status, reset.body)
	}
	if reset := requestAdapterWithToken(t, control.URL, "write-token", "POST", "/__e2e/reset", map[string]string{}); reset.status != http.StatusOK {
		t.Fatalf("reset returned %d: %s", reset.status, reset.body)
	}
	if head := gitRev(t, bare, "refs/heads/main"); head != baseCommit {
		t.Fatalf("reset moved main to %s instead of pinned base %s", head, baseCommit)
	}
	prs := requestAdapterWithToken(t, api.URL, "read-token", "GET", "/repos/pagerless-test/sandbox/pulls", nil)
	if prs.status != http.StatusOK || string(prs.body) != "[]\n" {
		t.Fatalf("reset preserved pull requests: %d %s", prs.status, prs.body)
	}
}

type adapterResponse struct {
	status int
	body   []byte
}

func requestAdapter(t *testing.T, base, method, route string, body any) adapterResponse {
	return requestAdapterWithToken(t, base, "write-token", method, route, body)
}

func requestAdapterWithToken(t *testing.T, base, token, method, route string, body any) adapterResponse {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, base+route, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var answer adapterResponse
	answer.status = response.StatusCode
	answer.body, _ = io.ReadAll(response.Body)
	return answer
}

func testRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "source")
	bare := filepath.Join(root, "pagerless-test", "sandbox.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o750); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "init", "--bare", "--initial-branch=main", bare)
	gitTest(t, root, "clone", bare, work)
	gitTest(t, work, "config", "user.name", "E2E Test")
	gitTest(t, work, "config", "user.email", "e2e-test@pagerless.dev")
	if err := os.WriteFile(filepath.Join(work, "workload.yaml"), []byte("memory: 64Mi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, work, "add", "workload.yaml")
	gitTest(t, work, "commit", "--message", "Initial healthy Sandbox")
	gitTest(t, work, "push", "origin", "HEAD:main")
	gitTest(t, bare, "config", "http.receivepack", "true")
	return bare, gitRev(t, bare, "refs/heads/main")
}

func gitRev(t *testing.T, repository, ref string) string {
	t.Helper()
	return strings.TrimSpace(gitTest(t, repository, "rev-parse", ref))
}

func gitWithCredentials(t *testing.T, directory, remote, token string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http."+remote+".extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+base64TestToken(token))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func base64TestToken(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("pagerless:" + token))
}

func gitTest(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=E2E Test", "GIT_AUTHOR_EMAIL=e2e-test@pagerless.dev",
		"GIT_COMMITTER_NAME=E2E Test", "GIT_COMMITTER_EMAIL=e2e-test@pagerless.dev")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
