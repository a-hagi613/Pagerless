package githubmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer read-token" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/repos/acme/cluster":
			_, _ = response.Write([]byte(`{"full_name":"acme/cluster","default_branch":"main","private":true}`))
		case "/repos/acme/cluster/contents/apps/checkout/deployment.yaml":
			content := base64.StdEncoding.EncodeToString([]byte("kind: Deployment\n"))
			_, _ = response.Write([]byte(
				`{"type":"file","encoding":"base64","size":16,"content":"` + content + `"}`))
		case "/repos/acme/cluster/contents/apps/checkout":
			_, _ = response.Write([]byte(
				`[{"name":"deployment.yaml","path":"apps/checkout/deployment.yaml","type":"file"}]`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	github, err := New(server.URL, "acme/cluster", "apps/checkout",
		func() (string, error) { return "read-token", nil })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	github.HTTP = server.Client()
	return github
}

func TestReadToolsAreReadOnly(t *testing.T) {
	t.Parallel()
	github := testServer(t)
	for _, tool := range github.Tools() {
		switch tool.Name {
		case "github_repository", "github_get_file", "github_list_directory":
		default:
			t.Fatalf("the provider advertises the non-read tool %q", tool.Name)
		}
	}
}

func TestGetFileAndDirectory(t *testing.T) {
	t.Parallel()
	github := testServer(t)
	result, err := github.getFile(context.Background(), json.RawMessage(`{"path":"apps/checkout/deployment.yaml"}`))
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if !strings.Contains(result, "kind: Deployment") {
		t.Fatalf("file content %q", result)
	}
	entries, err := github.listDirectory(context.Background(), json.RawMessage(`{"path":"apps/checkout"}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(entries, "deployment.yaml") {
		t.Fatalf("entries %q", entries)
	}
}

func TestReadsStayInsideThePathPrefix(t *testing.T) {
	t.Parallel()
	github := testServer(t)
	for _, path := range []string{"../secret.yaml", "apps/prod/deployment.yaml", "/etc/passwd", `apps\checkout\x`} {
		arguments := json.RawMessage(`{"path":` + quote(path) + `}`)
		if _, err := github.getFile(context.Background(), arguments); err == nil {
			t.Fatalf("%q was accepted", path)
		}
	}
	allowed := json.RawMessage(`{"path":"apps/checkout/deployment.yaml"}`)
	if _, err := github.getFile(context.Background(), allowed); err != nil {
		t.Fatalf("an allowed path was refused: %v", err)
	}
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
