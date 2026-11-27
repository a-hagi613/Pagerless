// Package server composes the investigation service from its configuration,
// serves the public API and the embedded dashboard, and owns the process
// lifecycle.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/a-hagi613/pagerless/dashboard"
	"github.com/a-hagi613/pagerless/internal/agentclient"
	"github.com/a-hagi613/pagerless/internal/config"
	"github.com/a-hagi613/pagerless/internal/githubpublish"
	"github.com/a-hagi613/pagerless/internal/investigations"
	"github.com/a-hagi613/pagerless/internal/mcp"
	"github.com/a-hagi613/pagerless/internal/transcript"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 20 * time.Second
	httpTimeout       = 30 * time.Second
)

// Server is one running installation.
type Server struct {
	cfg     config.Config
	store   *transcript.Store
	service *investigations.Service
	http    *http.Server
}

// New builds the server and every integration from the configuration. A
// missing optional integration leaves the server running but degraded; a
// missing operator credential is fatal, because serving without authentication
// would expose the installation.
func New(ctx context.Context, cfg config.Config) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create the data directory: %w", err)
	}
	operatorToken := config.ReadToken(cfg.OperatorTokenFile)
	if _, err := operatorToken(); err != nil {
		return nil, fmt.Errorf("read the operator token: %w", err)
	}
	internalToken := config.ReadToken(cfg.InternalTokenFile)
	mcpTokenFile := cfg.MCPTokenFile
	if mcpTokenFile == "" {
		mcpTokenFile = cfg.InternalTokenFile
	}
	mcpToken := config.ReadToken(mcpTokenFile)
	readToken := config.ReadToken(cfg.GitHubReadToken)
	writeToken := config.ReadToken(cfg.GitHubWriteToken)

	store, err := transcript.Open(ctx, transcript.Options{Path: transcript.Path(cfg.DataDir)})
	if err != nil {
		return nil, err
	}

	var agent *agentclient.Client
	if cfg.AgentURL != "" {
		agent = agentclient.New(cfg.AgentURL, config.ContextToken(internalToken), &http.Client{Timeout: 0})
	}
	kubernetesClient := mcp.New("kubernetes", cfg.KubernetesMCPURL, config.ContextToken(mcpToken),
		&http.Client{Timeout: httpTimeout})
	githubMCP := mcp.New("github", cfg.GitHubMCPURL, config.ContextToken(mcpToken),
		&http.Client{Timeout: httpTimeout})
	var extras []mcp.NamedAdapter
	if cfg.PrometheusMCPURL != "" {
		client := mcp.New("prometheus", cfg.PrometheusMCPURL, config.ContextToken(mcpToken),
			&http.Client{Timeout: httpTimeout})
		extras = append(extras, mcp.NamedAdapter{Name: "prometheus_mcp", Adapter: &mcp.ReadOnly{
			Client:  client,
			Allowed: []string{"prometheus_query", "prometheus_alerts", "prometheus_targets"},
		}})
	}
	if cfg.GrafanaMCPURL != "" {
		client := mcp.New("grafana", cfg.GrafanaMCPURL, config.ContextToken(mcpToken),
			&http.Client{Timeout: httpTimeout})
		extras = append(extras, mcp.NamedAdapter{Name: "grafana_mcp", Adapter: &mcp.ReadOnly{
			Client:  client,
			Allowed: []string{"grafana_alerts", "grafana_dashboards", "grafana_dashboard", "grafana_datasources"},
		}})
	}
	var toolbox *mcp.Toolbox
	if cfg.KubernetesMCPURL != "" || cfg.GitHubMCPURL != "" || len(extras) > 0 {
		var kubernetes *mcp.Kubernetes
		if cfg.KubernetesMCPURL != "" {
			kubernetes = &mcp.Kubernetes{Client: kubernetesClient, Namespaces: cfg.NamespaceAllowlist}
		}
		var github *mcp.GitHub
		if cfg.GitHubMCPURL != "" {
			github = &mcp.GitHub{Client: githubMCP, Repository: cfg.GitHubRepository, PathPrefix: cfg.GitHubPathPrefix}
		}
		toolbox = mcp.NewToolbox(kubernetes, github, extras...)
	}

	publisher, publishCheck, err := publishing(cfg, readToken, writeToken)
	if err != nil {
		return nil, err
	}

	var service *investigations.Service
	serviceConfig := investigations.Config{
		Version:        cfg.Version,
		Agent:          agent,
		Toolbox:        toolbox,
		Publisher:      publisher,
		CallbackOrigin: cfg.CallbackOrigin,
		PublishCheck:   publishCheck,
		Gate: func(ctx context.Context) error {
			return service.ReadyGate(ctx)
		},
		Retention:         cfg.Retention,
		MaxActiveTurns:    cfg.MaxActiveTurns,
		TurnTimeout:       cfg.TurnTimeout,
		StopGrace:         cfg.StopGrace,
		ToolTimeout:       cfg.ToolTimeout,
		MaxToolCalls:      cfg.MaxToolCalls,
		MaxResultBytes:    cfg.MaxResultBytes,
		BudgetToolCalls:   cfg.BudgetToolCalls,
		BudgetModelCalls:  cfg.BudgetModelCalls,
		BudgetInputTokens: cfg.BudgetInputTokens,
		BudgetSeconds:     cfg.BudgetSeconds,
	}
	service, err = investigations.New(ctx, store, serviceConfig)
	if err != nil {
		_ = store.Close()
		return nil, err
	}

	api := investigations.NewAPI(
		service,
		investigations.NewAuth(operatorToken, internalToken),
		NewSPA(dashboard.Assets()),
	)
	handler := api.Handler()
	trigger := newTriggerServer(cfg, service, operatorToken)
	root := http.NewServeMux()
	root.Handle("/mcp", trigger.Handler())
	root.Handle("/", handler)
	handler = root
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	return &Server{cfg: cfg, store: store, service: service, http: httpServer}, nil
}

// publishing builds the isolated write path. A missing repository or credential
// leaves it unconfigured, which readiness reports instead of failing startup.
func publishing(
	cfg config.Config, readToken, writeToken func() (string, error),
) (*githubpublish.Publisher, func(context.Context) (string, error), error) {
	if cfg.GitHubRepository == "" || cfg.GitHubReadToken == "" || cfg.GitHubWriteToken == "" {
		return nil, nil, nil
	}
	reader, err := githubpublish.NewREST(cfg.GitHubAPIURL, cfg.GitHubRepository,
		func(context.Context) (string, error) { return readToken() }, &http.Client{Timeout: httpTimeout})
	if err != nil {
		return nil, nil, fmt.Errorf("configure the GitHub reader: %w", err)
	}
	writer, err := githubpublish.NewREST(cfg.GitHubAPIURL, cfg.GitHubRepository,
		func(context.Context) (string, error) { return writeToken() }, &http.Client{Timeout: httpTimeout})
	if err != nil {
		return nil, nil, fmt.Errorf("configure the GitHub writer: %w", err)
	}
	publisher := &githubpublish.Publisher{
		Config: githubpublish.Config{
			Repository: cfg.GitHubRepository,
			BaseBranch: cfg.GitHubBaseBranch,
			PathPrefix: cfg.GitHubPathPrefix,
		},
		Reader: reader,
		Writer: writer,
	}
	check := func(ctx context.Context) (string, error) {
		info, err := writer.Repository(ctx)
		if err != nil {
			return "", err
		}
		if _, found := info.Permissions["push"]; found && !info.Permissions["push"] {
			return "", errors.New("the publishing credential does not report write access")
		}
		if info.DefaultBranch != "" && info.DefaultBranch != cfg.GitHubBaseBranch {
			return "", fmt.Errorf("the configured base branch is not the repository default branch")
		}
		if _, found, err := reader.Head(ctx, cfg.GitHubBaseBranch); err != nil {
			return "", err
		} else if !found {
			return "", fmt.Errorf("the configured base branch does not exist")
		}
		if info.Permissions["push"] {
			return "repository write access is reported by GitHub; no test pull request was created", nil
		}
		return "the repository is reachable with the publishing credential; " +
			"write capability is confirmed only when a proposal is published", nil
	}
	return publisher, check, nil
}

// Run serves until the context ends, then stops the listener and the turns.
func (s *Server) Run(ctx context.Context) error {
	serverErrors := make(chan error, 1)
	go func() {
		if s.cfg.TLSCertFile != "" {
			s.http.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			serverErrors <- s.http.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
			return
		}
		serverErrors <- s.http.ListenAndServe()
	}()
	slog.Info("pagerless server listening", "address", s.cfg.ListenAddr, "version", s.cfg.Version)
	go s.service.RunRetention(ctx)
	select {
	case err := <-serverErrors:
		s.service.Close()
		_ = s.store.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = s.http.Shutdown(shutdownContext)
		s.service.Close()
		return s.store.Close()
	}
}
