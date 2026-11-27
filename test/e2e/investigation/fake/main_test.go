package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	keyRole  = "role"
	roleUser = "user"
	roleTool = "tool"

	testQuestion = "why is checkout failing"
)

// modelRequest builds one Chat Completions request body.
func modelRequest(messages []map[string]any, tools ...string) []byte {
	specs := make([]map[string]any, 0, len(tools))
	for _, name := range tools {
		specs = append(specs, map[string]any{
			"type": "function", "function": map[string]string{"name": name},
		})
	}
	body, _ := json.Marshal(map[string]any{"messages": messages, "tools": specs})
	return body
}

func askModel(t *testing.T, body []byte) map[string]any {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	(&modelServer{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("model answered %d", response.Code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return decoded
}

func toolCall(t *testing.T, decoded map[string]any) (string, map[string]any) {
	t.Helper()
	message := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	function := call["function"].(map[string]any)
	var arguments map[string]any
	if err := json.Unmarshal([]byte(function["arguments"].(string)), &arguments); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	return function["name"].(string), arguments
}

func TestTheModelRefusesAnUnknownEndpointPath(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1-broken/chat/completions",
		bytes.NewReader(modelRequest([]map[string]any{{"role": "user", "content": "why"}}, "resources_get")))
	(&modelServer{}).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("a wrong endpoint path answered %d", response.Code)
	}
}

func TestTheModelReadsBeforeSubmitting(t *testing.T) {
	t.Parallel()
	name, arguments := toolCall(t, askModel(t, modelRequest(
		[]map[string]any{{keyRole: roleUser, keyContent: testQuestion}},
		"resources_get", "submit_answer", "read_playbook",
	)))
	if name != "resources_get" {
		t.Fatalf("first tool %q", name)
	}
	if arguments["namespace"] != "shop" || arguments["name"] != "checkout" {
		t.Fatalf("read arguments %+v", arguments)
	}
}

func TestSubmitAnswerCitesTheRuntimeCallID(t *testing.T) {
	t.Parallel()
	envelope := `<tool-result nonce="n" source="resources_get">
{"callId":"c-0123456789abcdef","kind":"Deployment"}
</tool-result nonce="n">`
	messages := []map[string]any{
		{keyRole: roleUser, keyContent: testQuestion},
		{keyRole: roleTool, keyContent: envelope},
	}
	name, arguments := toolCall(t, askModel(t, modelRequest(messages,
		"resources_get", "submit_answer")))
	if name != "submit_answer" {
		t.Fatalf("second tool %q", name)
	}
	citations := arguments["citations"].([]any)
	if citations[0].(map[string]any)["callId"] != "c-0123456789abcdef" {
		t.Fatalf("citations %+v", citations)
	}
	proposal := arguments["proposal"].(map[string]any)
	base, _ := proposal["baseSha"].(string)
	if len(base) != 40 || strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("baseSha %q is not a full revision", base)
	}
	files := proposal["files"].([]any)
	if files[0].(map[string]any)["path"] != proposalPath {
		t.Fatalf("files %+v", files)
	}
}

func githubCall(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, reader))
	return response
}

func TestFakeGitHubPublishesOneDraftAndNeverMerges(t *testing.T) {
	t.Parallel()
	repository := newGitHub()
	if response := githubCall(t, repository, http.MethodGet,
		"/repos/acme/cluster/git/ref/heads/main", nil); response.Code != http.StatusOK {
		t.Fatalf("base ref answered %d", response.Code)
	}
	if response := githubCall(t, repository, http.MethodPost,
		"/repos/acme/cluster/git/refs",
		map[string]string{"ref": "refs/heads/pagerless/p1-t1", "sha": baseSHA}); response.Code != http.StatusCreated {
		t.Fatalf("branch answered %d", response.Code)
	}
	content := base64.StdEncoding.EncodeToString([]byte("kind: Deployment\n"))
	if response := githubCall(t, repository, http.MethodPut,
		"/repos/acme/cluster/contents/"+proposalPath,
		map[string]string{"content": content, "branch": "pagerless/p1-t1"}); response.Code != http.StatusCreated {
		t.Fatalf("commit answered %d", response.Code)
	}
	if response := githubCall(t, repository, http.MethodPost,
		"/repos/acme/cluster/pulls", map[string]any{
			"title": "t", "head": "pagerless/p1-t1", "base": "main", "body": "b", "draft": true,
		}); response.Code != http.StatusCreated {
		t.Fatalf("pull request answered %d", response.Code)
	}
	pulls := githubCall(t, repository, http.MethodGet, "/_test/pulls", nil)
	var decoded []pull
	if err := json.Unmarshal(pulls.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode pulls: %v", err)
	}
	if len(decoded) != 1 || !decoded[0].Draft || decoded[0].Base != "main" {
		t.Fatalf("pulls %+v", decoded)
	}
	if response := githubCall(t, repository, http.MethodPut,
		"/repos/acme/cluster/pulls/1", map[string]any{}); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("merge answered %d", response.Code)
	}
	// The base revision stays immutable after a branch commit.
	base := githubCall(t, repository, http.MethodGet,
		"/repos/acme/cluster/contents/"+proposalPath+"?ref="+baseSHA, nil)
	if !strings.Contains(base.Body.String(), base64.StdEncoding.EncodeToString([]byte(baseManifest))) {
		t.Fatalf("the base content changed: %s", base.Body.String())
	}
}

func TestTheOutageToggleFailsTheModelAndRecovers(t *testing.T) {
	modelOutage.Store(false)
	t.Cleanup(func() { modelOutage.Store(false) })
	handler := &modelServer{}
	toggle := httptest.NewRecorder()
	handler.ServeHTTP(toggle, httptest.NewRequest(http.MethodGet, "/_test/outage?on=1", nil))
	if toggle.Code != http.StatusOK {
		t.Fatalf("toggle answered %d", toggle.Code)
	}
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(modelRequest([]map[string]any{{keyRole: roleUser, keyContent: testQuestion}}, "resources_get"))))
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("the outage answered %d", failed.Code)
	}
	recoverToggle := httptest.NewRecorder()
	handler.ServeHTTP(recoverToggle, httptest.NewRequest(http.MethodGet, "/_test/outage?on=0", nil))
	recovered := askModel(t, modelRequest(
		[]map[string]any{{keyRole: roleUser, keyContent: testQuestion}}, "resources_get"))
	if recovered["choices"] == nil {
		t.Fatalf("the model did not recover: %+v", recovered)
	}
}
