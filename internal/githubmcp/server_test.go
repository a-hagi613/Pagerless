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

const (
	testBaseSHA    = "1111111111111111111111111111111111111111"
	testReleaseSHA = "2222222222222222222222222222222222222222"
	testPath       = "apps/checkout/deployment.yaml"
	testPrefix     = "apps/checkout"
)

func testServer(t *testing.T, baseBranch string) *Server {
	t.Helper()
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer read-token" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/repos/acme/cluster":
			_, _ = response.Write([]byte(`{"full_name":"acme/cluster","default_branch":"main","private":true}`))
		case "/repos/acme/cluster/git/ref/heads/main":
			_, _ = response.Write([]byte(`{"object":{"sha":"` + testBaseSHA + `"}}`))
		case "/repos/acme/cluster/git/ref/heads/release":
			_, _ = response.Write([]byte(`{"object":{"sha":"` + testReleaseSHA + `"}}`))
		case "/repos/acme/cluster/contents/" + testPath:
			content := base64.StdEncoding.EncodeToString([]byte("kind: Deployment\n"))
			_, _ = response.Write([]byte(
				`{"type":"file","encoding":"base64","size":16,"content":"` + content + `"}`))
		case "/repos/acme/cluster/contents/" + testPrefix:
			_, _ = response.Write([]byte(
				`[{"name":"deployment.yaml","path":"` + testPath + `","type":"file"}]`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	github, err := New(server.URL, "acme/cluster", baseBranch, testPrefix,
		func() (string, error) { return "read-token", nil })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	github.HTTP = server.Client()
	return github
}

func TestReadToolsAreReadOnly(t *testing.T) {
	t.Parallel()
	github := testServer(t, "main")
	for _, tool := range github.Tools() {
		switch tool.Name {
		case "github_repository", "github_get_file", "github_list_directory":
		default:
			t.Fatalf("the provider advertises the non-read tool %q", tool.Name)
		}
	}
}

func TestRepositoryResolvesTheConfiguredBaseBranch(t *testing.T) {
	t.Parallel()
	github := testServer(t, "main")
	result, err := github.repository(context.Background(), nil)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["baseSha"] != testBaseSHA || decoded["baseBranch"] != "main" {
		t.Fatalf("repository evidence %+v", decoded)
	}
	if decoded["pathPrefix"] != testPrefix {
		t.Fatalf("path scope %+v", decoded)
	}
}

func TestRepositoryResolvesANonDefaultBaseBranch(t *testing.T) {
	t.Parallel()
	github := testServer(t, "release")
	result, err := github.repository(context.Background(), nil)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["baseSha"] != testReleaseSHA || decoded["baseBranch"] != "release" {
		t.Fatalf("repository evidence %+v", decoded)
	}
}

func TestRepositoryRefusesAMissingBaseBranch(t *testing.T) {
	t.Parallel()
	github := testServer(t, "missing")
	if _, err := github.repository(context.Background(), nil); err == nil {
		t.Fatal("a missing base branch resolved")
	}
}

func TestTheRootDirectoryStartsAtTheConfiguredScope(t *testing.T) {
	t.Parallel()
	github := testServer(t, "main")
	result, err := github.listDirectory(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var decoded struct {
		Path     string `json:"path"`
		Revision string `json:"revision"`
		Entries  []struct {
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Path != testPrefix || decoded.Revision != testBaseSHA {
		t.Fatalf("directory evidence %+v", decoded)
	}
	if len(decoded.Entries) != 1 || decoded.Entries[0].Path != testPath {
		t.Fatalf("entries %+v", decoded.Entries)
	}
}

func TestGetFileAndDirectory(t *testing.T) {
	t.Parallel()
	github := testServer(t, "main")
	result, err := github.getFile(context.Background(), json.RawMessage(`{"path":"`+testPath+`"}`))
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if !strings.Contains(result, "kind: Deployment") {
		t.Fatalf("file content %q", result)
	}
}

func TestReadsStayInsideThePathPrefix(t *testing.T) {
	t.Parallel()
	github := testServer(t, "main")
	for _, path := range []string{"../secret.yaml", "apps/prod/deployment.yaml", "/etc/passwd", `apps\checkout\x`} {
		arguments := json.RawMessage(`{"path":` + quote(path) + `}`)
		if _, err := github.getFile(context.Background(), arguments); err == nil {
			t.Fatalf("%q was accepted", path)
		}
	}
	if _, err := github.listDirectory(context.Background(), json.RawMessage(`{"path":"apps/prod"}`)); err == nil {
		t.Fatal("a directory outside the scope was accepted")
	}
	allowed := json.RawMessage(`{"path":"` + testPath + `"}`)
	if _, err := github.getFile(context.Background(), allowed); err != nil {
		t.Fatalf("an allowed path was refused: %v", err)
	}
}

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
