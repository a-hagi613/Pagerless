package cli_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/internal/cli"
)

// sessionCookie builds a signed-cookie-shaped value with a CSRF claim. The CLI
// reads the claim; the server verifies the signature.
func sessionCookie(t *testing.T) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"v": 1, "sub": "admin", "csrf": "csrf-1"})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// runCLI executes kubectl-page with the arguments and the given standard input.
func runCLI(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	root := cli.NewRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.ExecuteContext(t.Context())
	return stdout.String(), stderr.String(), err
}

func TestASecretValueIsNeverAFlag(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--api-key", "--ai-api-key", "--token", "--github-token", "--password"} {
		_, _, err := runCLI(t, "", "config", "set", "ai", flag, "sk-test")
		if err == nil {
			t.Fatalf("%s was accepted", flag)
		}
		wants := []string{"secret value may never be a flag", "interactive prompt", "standard input", "file", "env file"}
		for _, want := range wants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error for %s = %q, want it to mention %q", flag, err, want)
			}
		}
		if code := cli.ExitCode(err); code != cli.ExitUsage {
			t.Errorf("exit code for %s = %d, want %d", flag, code, cli.ExitUsage)
		}
	}
}

func TestLoginStoresOnlyTheServerAndTheSession(t *testing.T) {
	t.Parallel()
	cookie := sessionCookie(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Token != "bootstrap-token" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "pagerless_session", Value: cookie, Path: "/", HttpOnly: true})
		_ = json.NewEncoder(w).Encode(map[string]string{"expiresAt": "2026-11-19T04:00:00Z"})
	}))
	t.Cleanup(server.Close)

	configPath := filepath.Join(t.TempDir(), "nested", "config.yaml")
	out, errOut, err := runCLI(t, "BOOTSTRAP_TOKEN=bootstrap-token\n",
		"--config", configPath, "login", "--server", server.URL, "--from-stdin")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, "signed in to "+server.URL) {
		t.Errorf("output = %q, want the server it signed in to", out)
	}
	if !strings.Contains(out, "kubectl -n pagerless delete secret pagerless-admin-secret") {
		t.Errorf("output = %q, want the documented hardening step", out)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("configuration mode = %o, want 600", mode)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	stored := yamlLineValues(strings.Split(strings.TrimSpace(string(raw)), "\n"))
	if len(stored) != 2 || stored["server"] != server.URL || stored["sessionToken"] != cookie {
		t.Errorf("configuration = %v, want only server and sessionToken", stored)
	}
	if strings.Contains(string(raw), "bootstrap-token") {
		t.Error("the bootstrap token was written to the configuration file")
	}
}

// yamlLineValues reads the flat "key: value" shape that saveConfig writes.
func yamlLineValues(lines []string) map[string]string {
	values := map[string]string{}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return values
}

func TestLoginReportsDisabledMode(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"login_disabled","message":"token sign-in is disabled"}}`)
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	_, _, err := runCLI(t, "BOOTSTRAP_TOKEN=x\n",
		"--config", filepath.Join(dir, "config.yaml"), "login", "--server", server.URL, "--from-stdin")
	if err == nil || !strings.Contains(err.Error(), "auth.mode=disabled") {
		t.Fatalf("error = %v, want it to explain disabled mode", err)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) != 0 {
		t.Errorf("login wrote %d files after a refused sign-in, want none", len(entries))
	}
}

func TestConfigGetPrintsMaskedFieldsFromServerMode(t *testing.T) {
	t.Parallel()
	cookie := sessionCookie(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/settings" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "pagerless_session=") {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"managedBy":"external","manager":"team-credentials",
			"writeCredentials":false,"onboarding":true,
			"values":{"AI_BASE_URL":"https://models.example/v1"},
			"fields":{"AI_API_KEY":{"configured":true,"fingerprint":"sha256:abcd1234"},
			"GITHUB_TOKEN":{"configured":false}}}`)
	}))
	t.Cleanup(server.Close)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "server: " + server.URL + "\nsessionToken: " + cookie + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runCLI(t, "", "--config", configPath, "config", "get", "--mode", "server")
	if err != nil {
		t.Fatalf("config get: %v\n%s", err, errOut)
	}
	for _, want := range []string{"external", "team-credentials", "read-only", "AI_API_KEY", "sha256:abcd1234"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-") {
		t.Errorf("output carries a credential value:\n%s", out)
	}
}

func TestConfigSetWritesOnlyTheSubmittedKeys(t *testing.T) {
	t.Parallel()
	cookie := sessionCookie(t)
	var patched map[string]map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-CSRF-Token") != "csrf-1" {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&patched)
		_, _ = io.WriteString(w,
			`{"managedBy":"portal","manager":"pagerless-credentials","writeCredentials":true,"fields":{}}`)
	}))
	t.Cleanup(server.Close)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "server: " + server.URL + "\nsessionToken: " + cookie + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runCLI(t, "AI_API_KEY=sk-test\n",
		"--config", configPath, "config", "set", "ai", "--mode", "server", "--from-stdin",
		"--ai-base-url", "https://models.example/v1")
	if err != nil {
		t.Fatalf("config set: %v\n%s", err, errOut)
	}
	if got := patched["values"]["AI_API_KEY"]; got != "sk-test" {
		t.Errorf("AI_API_KEY = %q, want the stdin value", got)
	}
	if got := patched["values"]["AI_BASE_URL"]; got != "https://models.example/v1" {
		t.Errorf("AI_BASE_URL = %q, want the flag value", got)
	}
	if len(patched["values"]) != 2 {
		t.Errorf("values = %v, want exactly the two submitted keys", patched["values"])
	}
}

func TestRefusedExternallyManagedWriteExitsThree(t *testing.T) {
	t.Parallel()
	cookie := sessionCookie(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"settings_externally_managed",
			"message":"credentials are managed externally by Secret \"team-credentials\" and are read-only",
			"manager":"team-credentials"}}`)
	}))
	t.Cleanup(server.Close)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "server: " + server.URL + "\nsessionToken: " + cookie + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, "AI_API_KEY=sk-test\n",
		"--config", configPath, "config", "set", "ai", "--mode", "server", "--from-stdin")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error = %v, want the read-only refusal", err)
	}
	if code := cli.ExitCode(err); code != cli.ExitExternal {
		t.Errorf("exit code = %d, want %d", code, cli.ExitExternal)
	}
}
