// Package config reads the installation's settings from the environment. Helm
// renders these values from chart settings and externally provisioned Secrets;
// every secret is a file path, never a literal value in a flag.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the process configuration.
type Config struct {
	ListenAddr string
	DataDir    string
	Version    string

	OperatorTokenFile string
	InternalTokenFile string
	MCPTokenFile      string

	AgentURL       string
	CallbackOrigin string

	KubernetesMCPURL string
	GitHubMCPURL     string

	GitHubRepository string
	GitHubBaseBranch string
	GitHubPathPrefix string
	GitHubAPIURL     string
	GitHubReadToken  string
	GitHubWriteToken string

	NamespaceAllowlist []string

	Retention         time.Duration
	TurnTimeout       time.Duration
	ToolTimeout       time.Duration
	StopGrace         time.Duration
	MaxActiveTurns    int
	MaxToolCalls      int
	MaxResultBytes    int
	BudgetToolCalls   int
	BudgetModelCalls  int
	BudgetInputTokens int
	BudgetSeconds     int

	TLSCertFile string
	TLSKeyFile  string
}

// Load reads and validates the configuration. A missing required setting is
// named in the error so the installer can fix it without reading source.
func Load() (Config, error) {
	config := Config{
		ListenAddr:        env("PAGERLESS_LISTEN_ADDR", ":8080"),
		DataDir:           env("PAGERLESS_DATA_DIR", "/var/lib/pagerless"),
		Version:           env("PAGERLESS_VERSION", "dev"),
		OperatorTokenFile: env("PAGERLESS_OPERATOR_TOKEN_FILE", ""),
		InternalTokenFile: env("PAGERLESS_INTERNAL_TOKEN_FILE", ""),
		MCPTokenFile:      env("PAGERLESS_MCP_TOKEN_FILE", ""),
		AgentURL:          env("PAGERLESS_AGENT_URL", ""),
		CallbackOrigin:    env("PAGERLESS_CALLBACK_ORIGIN", ""),
		KubernetesMCPURL:  env("PAGERLESS_KUBERNETES_MCP_URL", ""),
		GitHubMCPURL:      env("PAGERLESS_GITHUB_MCP_URL", ""),
		GitHubRepository:  env("PAGERLESS_GITHUB_REPOSITORY", ""),
		GitHubBaseBranch:  env("PAGERLESS_GITHUB_BASE_BRANCH", "main"),
		GitHubPathPrefix:  env("PAGERLESS_GITHUB_PATH_PREFIX", ""),
		GitHubAPIURL:      env("PAGERLESS_GITHUB_API_URL", "https://api.github.com"),
		GitHubReadToken:   env("PAGERLESS_GITHUB_READ_TOKEN_FILE", ""),
		GitHubWriteToken:  env("PAGERLESS_GITHUB_WRITE_TOKEN_FILE", ""),
		TLSCertFile:       env("PAGERLESS_TLS_CERT_FILE", ""),
		TLSKeyFile:        env("PAGERLESS_TLS_KEY_FILE", ""),
	}
	var err error
	if config.Retention, err = duration("PAGERLESS_RETENTION", 7*24*time.Hour); err != nil {
		return Config{}, err
	}
	if config.TurnTimeout, err = duration("PAGERLESS_TURN_TIMEOUT", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if config.ToolTimeout, err = duration("PAGERLESS_TOOL_TIMEOUT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if config.StopGrace, err = duration("PAGERLESS_STOP_GRACE", 10*time.Second); err != nil {
		return Config{}, err
	}
	if config.MaxActiveTurns, err = integer("PAGERLESS_MAX_ACTIVE_TURNS", 4); err != nil {
		return Config{}, err
	}
	if config.MaxToolCalls, err = integer("PAGERLESS_MAX_TOOL_CALLS", 16); err != nil {
		return Config{}, err
	}
	if config.MaxResultBytes, err = integer("PAGERLESS_MAX_RESULT_BYTES", 64<<10); err != nil {
		return Config{}, err
	}
	if config.BudgetToolCalls, err = integer("PAGERLESS_BUDGET_TOOL_CALLS", 16); err != nil {
		return Config{}, err
	}
	if config.BudgetModelCalls, err = integer("PAGERLESS_BUDGET_MODEL_CALLS", 8); err != nil {
		return Config{}, err
	}
	if config.BudgetInputTokens, err = integer("PAGERLESS_BUDGET_INPUT_TOKENS", 0); err != nil {
		return Config{}, err
	}
	if config.BudgetSeconds, err = integer("PAGERLESS_BUDGET_SECONDS", 600); err != nil {
		return Config{}, err
	}
	config.NamespaceAllowlist = splitList(env("PAGERLESS_NAMESPACE_ALLOWLIST", ""))
	return config, config.validate()
}

func (c Config) validate() error {
	if strings.TrimSpace(c.OperatorTokenFile) == "" {
		return errors.New("PAGERLESS_OPERATOR_TOKEN_FILE is required")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("PAGERLESS_DATA_DIR is required")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("PAGERLESS_TLS_CERT_FILE and PAGERLESS_TLS_KEY_FILE must be set together")
	}
	if c.AgentURL != "" && c.CallbackOrigin == "" {
		return errors.New("PAGERLESS_CALLBACK_ORIGIN is required when PAGERLESS_AGENT_URL is set")
	}
	if c.AgentURL == "" {
		return nil
	}
	if !strings.HasPrefix(c.AgentURL, "http://") && !strings.HasPrefix(c.AgentURL, "https://") {
		return errors.New("PAGERLESS_AGENT_URL must be an http or https URL")
	}
	if c.GitHubRepository != "" {
		owner, name, found := strings.Cut(c.GitHubRepository, "/")
		if !found || owner == "" || name == "" || strings.Contains(name, "/") {
			return errors.New("PAGERLESS_GITHUB_REPOSITORY must be owner/name")
		}
	}
	return nil
}

// ReadToken reads one credential file. A projected Secret rotates in place, so
// the reader is called per request rather than once at startup.
func ReadToken(path string) func() (string, error) {
	return func() (string, error) {
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("the credential file is not configured")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read the credential file: %w", err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", errors.New("the credential file is empty")
		}
		return token, nil
	}
}

// ContextToken adapts a token reader to the context-aware shape the clients use.
func ContextToken(reader func() (string, error)) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return reader() }
}

// Env returns one setting, for the chart notes and scripts.
func env(key, fallback string) string {
	if value, found := os.LookupEnv(key); found {
		return value
	}
	return fallback
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw, found := os.LookupEnv(key)
	if !found || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return value, nil
}

func integer(key string, fallback int) (int, error) {
	raw, found := os.LookupEnv(key)
	if !found || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return value, nil
}

func splitList(value string) []string {
	items := make([]string, 0, 4)
	for part := range strings.SplitSeq(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}
