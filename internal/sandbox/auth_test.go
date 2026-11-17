package sandbox_test

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const readToken = "private-sandbox-read-token"

func gitHTTPBackend(t *testing.T, repo *testenv.BareRepo) http.Handler {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	return &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(output)), "git-http-backend"),
		Dir:  filepath.Dir(repo.URL),
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(repo.URL),
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=pagerless",
		},
	}
}

func trustedGitServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), "git-ca.pem")
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	return server
}

func TestPrivateSandboxReadsAuthenticateOverVerifiedTLS(t *testing.T) {
	repo, _ := repoWith(t)
	backend := gitHTTPBackend(t, repo)
	server := trustedGitServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, token, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || token != readToken {
			http.Error(w, "read access denied", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	sb := sandbox.Repository{
		URL: server.URL + "/sandbox.git", Branch: mainBranch,
		ReadToken: func() (string, error) { return readToken, nil },
	}
	head, err := sb.Head(t.Context())
	if err != nil || head != repo.Head() {
		t.Fatalf("authenticated head = %q, %v, want %s", head, err, repo.Head())
	}
	checkout, err := sb.Checkout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	content, err := checkout.ReadFile(t.Context(), manifest)
	if err != nil || !strings.Contains(string(content), "replicas: 1") {
		t.Fatalf("authenticated manifest = %q, %v", content, err)
	}
}

func TestPrivateSandboxRefusesMissingOrWrongReadTokens(t *testing.T) {
	repo, _ := repoWith(t)
	backend := gitHTTPBackend(t, repo)
	server := trustedGitServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, token, _ := r.BasicAuth()
		if token != readToken {
			http.Error(w, "read access denied", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	for _, source := range []func() (string, error){nil, func() (string, error) { return "wrong-token", nil }} {
		sb := sandbox.Repository{URL: server.URL + "/sandbox.git", Branch: mainBranch, ReadToken: source}
		if _, err := sb.Head(t.Context()); err == nil {
			t.Error("Head without the required read token succeeded")
		}
		if checkout, err := sb.Checkout(t.Context()); err == nil {
			_ = checkout.Close()
			t.Error("Checkout without the required read token succeeded")
		}
	}
}

func TestPrivateSandboxReadsTheRotatedTokenForEveryOperation(t *testing.T) {
	repo, _ := repoWith(t)
	backend := gitHTTPBackend(t, repo)
	var expected atomic.Value
	expected.Store(readToken)
	server := trustedGitServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, token, _ := r.BasicAuth()
		if token != expected.Load().(string) {
			http.Error(w, "read access denied", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	calls := 0
	sb := sandbox.Repository{
		URL: server.URL + "/sandbox.git", Branch: mainBranch,
		ReadToken: func() (string, error) {
			calls++
			return expected.Load().(string), nil
		},
	}
	if _, err := sb.Head(t.Context()); err != nil {
		t.Fatal(err)
	}
	expected.Store("rotated-private-sandbox-read-token")
	checkout, err := sb.Checkout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	if calls != 2 {
		t.Errorf("read token source called %d times, want once for each of two operations", calls)
	}
}

func TestSandboxIgnoresInheritedGitAuthenticationAndURLRewrites(t *testing.T) {
	repo, _ := repoWith(t)
	backend := gitHTTPBackend(t, repo)
	var unrelated atomic.Int64
	server := trustedGitServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/other.git/") {
			unrelated.Add(1)
			http.Error(w, "unrelated repository", http.StatusForbidden)
			return
		}
		_, token, _ := r.BasicAuth()
		if token != readToken {
			http.Error(w, "read access denied", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	sb := sandbox.Repository{
		URL: server.URL + "/sandbox.git", Branch: mainBranch,
		ReadToken: func() (string, error) { return readToken, nil },
	}
	trace := filepath.Join(t.TempDir(), "git-trace")
	t.Setenv("GIT_TRACE", trace)
	t.Setenv("GIT_TRACE_CURL", trace)
	t.Setenv("GIT_TRACE_REDACT", "0")
	t.Setenv("SSLKEYLOGFILE", trace)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'url."+server.URL+"/other.git.insteadOf="+sb.URL+"'")
	if _, err := sb.Head(t.Context()); err != nil {
		t.Fatalf("inherited configuration changed the authenticated Head: %v", err)
	}
	if unrelated.Load() != 0 {
		t.Fatal("Git contacted an unrelated repository from inherited URL rewriting")
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatalf("Git wrote an inherited trace that could contain authentication: %v", err)
	}

	// A read without a token callback must not silently use inherited authentication.
	sb.ReadToken = nil
	netrc := filepath.Join(t.TempDir(), "netrc")
	netrcContents := []byte("machine 127.0.0.1 login x-access-token password " + readToken + "\n")
	if err := os.WriteFile(netrc, netrcContents, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NETRC", netrc)
	credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + readToken))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'http.extraHeader=Authorization: Basic "+credentials+"'")
	if _, err := sb.Head(t.Context()); err == nil {
		t.Fatal("an unauthenticated Head used inherited credentials")
	}
}

func TestSandboxReadErrorsNeverExposeTheReadToken(t *testing.T) {
	credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + readToken))
	server := trustedGitServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A proxy or Git server can reflect authentication in its error response.
		http.Error(w, fmt.Sprintf("denied %s %s", readToken, credentials), http.StatusForbidden)
	}))
	sb := sandbox.Repository{
		URL: server.URL + "/sandbox.git", Branch: mainBranch,
		ReadToken: func() (string, error) { return readToken, nil },
	}
	for _, clone := range []bool{false, true} {
		var err error
		if clone {
			_, err = sb.Checkout(t.Context())
		} else {
			_, err = sb.Head(t.Context())
		}
		if err == nil {
			t.Fatal("an unauthorized read succeeded")
		}
		for _, secret := range []string{readToken, credentials} {
			if strings.Contains(err.Error(), secret) {
				t.Fatal("a Sandbox read error exposed authentication")
			}
		}
	}
}

func TestSandboxReadCredentialsAreNotForwardedToRedirects(t *testing.T) {
	var redirected atomic.Int64
	var authenticated atomic.Int64
	server := trustedGitServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/other.git/") {
			redirected.Add(1)
			http.Error(w, "unrelated repository", http.StatusForbidden)
			return
		}
		if _, token, _ := r.BasicAuth(); token == readToken {
			authenticated.Add(1)
		}
		http.Redirect(w, r, "/other.git/info/refs?service=git-upload-pack", http.StatusTemporaryRedirect)
	}))
	sb := sandbox.Repository{
		URL: server.URL + "/sandbox.git", Branch: mainBranch,
		ReadToken: func() (string, error) { return readToken, nil },
	}
	if _, err := sb.Head(t.Context()); err == nil {
		t.Fatal("a redirected read succeeded")
	}
	if authenticated.Load() != 1 || redirected.Load() != 0 {
		t.Errorf("authenticated requests = %d, redirected requests = %d, want 1 and 0",
			authenticated.Load(), redirected.Load())
	}
}

func TestSandboxRefusesUnsafeAuthenticatedURLsBeforeReadingTheToken(t *testing.T) {
	for _, raw := range []string{
		"http://github.com/owner/sandbox.git", "file:///tmp/sandbox.git", "ssh://git@github.com/owner/sandbox.git",
		"https://user:embedded-token@github.com/owner/sandbox.git", "https://github.com/owner/sandbox.git?token=other",
		"https://github.com/owner/sandbox.git#fragment", "https://github.com/owner/../other.git", "https://github.com",
	} {
		called := false
		sb := sandbox.Repository{
			URL: raw, Branch: mainBranch,
			ReadToken: func() (string, error) {
				called = true
				return readToken, nil
			},
		}
		if err := sb.Check(); err == nil {
			t.Error("Check accepted an unsafe authenticated Sandbox URL")
		}
		if _, err := sb.Head(t.Context()); err == nil {
			t.Error("an unsafe authenticated Sandbox URL was accepted")
		}
		if called {
			t.Error("the read token was requested before its destination was checked")
		}
	}
}

func TestSandboxIgnoresConfigurationInTheTemporaryDirectorysParentRepository(t *testing.T) {
	repo, _ := repoWith(t)
	backend := gitHTTPBackend(t, repo)
	server := trustedGitServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, token, _ := r.BasicAuth()
		if token != readToken {
			http.Error(w, "read access denied", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	work := repo.Clone()
	setting := "url." + server.URL + "/other.git.insteadOf"
	if output, err := exec.CommandContext(t.Context(), "git", "-C", work, "config", setting,
		server.URL+"/sandbox.git").CombinedOutput(); err != nil {
		t.Fatalf("configure the parent repository: %v: %s", err, output)
	}
	t.Setenv("TMPDIR", work)
	sb := sandbox.Repository{
		URL: server.URL + "/sandbox.git", Branch: mainBranch,
		ReadToken: func() (string, error) { return readToken, nil },
	}
	if _, err := sb.Head(t.Context()); err != nil {
		t.Fatalf("parent repository configuration changed the Sandbox read: %v", err)
	}
}
