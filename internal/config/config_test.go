package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadNamesTheMissingOperatorCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGERLESS_OPERATOR_TOKEN_FILE", "")
	t.Setenv("PAGERLESS_DATA_DIR", dir)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PAGERLESS_OPERATOR_TOKEN_FILE") {
		t.Fatalf("missing token error %v", err)
	}
}

func TestLoadReadsTheDocumentedSettings(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "operator")
	if err := os.WriteFile(tokenFile, []byte("token\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PAGERLESS_OPERATOR_TOKEN_FILE", tokenFile)
	t.Setenv("PAGERLESS_DATA_DIR", dir)
	t.Setenv("PAGERLESS_AGENT_URL", "http://agent:8090")
	t.Setenv("PAGERLESS_CALLBACK_ORIGIN", "http://server:8080")
	t.Setenv("PAGERLESS_GITHUB_REPOSITORY", "acme/cluster")
	t.Setenv("PAGERLESS_RETENTION", "24h")
	t.Setenv("PAGERLESS_MAX_ACTIVE_TURNS", "2")
	t.Setenv("PAGERLESS_NAMESPACE_ALLOWLIST", "shop, payments")
	config, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.Retention.Hours() != 24 || config.MaxActiveTurns != 2 {
		t.Fatalf("config %+v", config)
	}
	if len(config.NamespaceAllowlist) != 2 || config.NamespaceAllowlist[1] != "payments" {
		t.Fatalf("allowlist %v", config.NamespaceAllowlist)
	}
	if config.GitHubBaseBranch != "main" {
		t.Fatalf("default base branch %q", config.GitHubBaseBranch)
	}
}

func TestLoadRefusesAnInvalidDuration(t *testing.T) {
	t.Setenv("PAGERLESS_OPERATOR_TOKEN_FILE", "/tmp/token")
	t.Setenv("PAGERLESS_DATA_DIR", t.TempDir())
	t.Setenv("PAGERLESS_RETENTION", "forever")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PAGERLESS_RETENTION") {
		t.Fatalf("invalid duration error %v", err)
	}
}
