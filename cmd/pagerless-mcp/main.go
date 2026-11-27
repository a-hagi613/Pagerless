// Command pagerless-mcp runs one read-only integration: the Kubernetes
// provider with its own read-only identity, or the GitHub provider with a read
// credential. Neither process has a write tool.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/a-hagi613/pagerless/internal/config"
	"github.com/a-hagi613/pagerless/internal/githubmcp"
	"github.com/a-hagi613/pagerless/internal/grafanamcp"
	"github.com/a-hagi613/pagerless/internal/kubemcp"
	"github.com/a-hagi613/pagerless/internal/mcpserver"
	"github.com/a-hagi613/pagerless/internal/prometheusmcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pagerless-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	provider := flag.String("provider", "", "kubernetes, github, prometheus or grafana")
	listen := flag.String("listen", ":8091", "listen address")
	tokenFile := flag.String("token-file", "", "file holding the installer-provided bearer credential")
	apiTokenFile := flag.String("api-token-file", "",
		"optional file holding the upstream Prometheus or Grafana bearer token")
	prometheusURL := flag.String("prometheus-url", "", "Prometheus base URL (prometheus)")
	grafanaURL := flag.String("grafana-url", "", "Grafana base URL (grafana)")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig file; empty uses the in-cluster identity")
	repository := flag.String("repository", "", "owner/name repository (github)")
	baseBranch := flag.String("base-branch", "main", "source base branch to resolve to a commit (github)")
	apiURL := flag.String("github-api-url", "https://api.github.com", "GitHub API URL (github)")
	githubTokenFile := flag.String("github-token-file", "", "file holding the read-scoped GitHub token (github)")
	pathPrefix := flag.String("path-prefix", "", "repository directory the read tools may access (github)")
	namespaces := flag.String("namespace-allowlist", "",
		"comma-separated namespaces the read tools may access (kubernetes)")
	flag.Parse()

	if *tokenFile == "" {
		return errors.New("--token-file is required")
	}
	token := config.ReadToken(*tokenFile)
	if _, err := token(); err != nil {
		return fmt.Errorf("read the integration token: %w", err)
	}
	server := &mcpserver.Server{Name: "pagerless-" + *provider, Token: token, Tools: nil}
	var upstreamToken func(context.Context) (string, error)
	if *apiTokenFile != "" {
		upstreamToken = config.ContextToken(config.ReadToken(*apiTokenFile))
	}
	switch strings.ToLower(*provider) {
	case "kubernetes":
		kubernetes, err := kubemcp.New(*kubeconfig, split(*namespaces))
		if err != nil {
			return err
		}
		server.Tools = kubernetes.Tools()
		server.Name = "pagerless-kubernetes-mcp"
	case "github":
		if *repository == "" || *githubTokenFile == "" {
			return errors.New("--repository and --github-token-file are required for the github provider")
		}
		github, err := githubmcp.New(*apiURL, *repository, *baseBranch, *pathPrefix,
			config.ReadToken(*githubTokenFile))
		if err != nil {
			return err
		}
		server.Tools = github.Tools()
		server.Name = "pagerless-github-mcp"
	case "prometheus":
		if *prometheusURL == "" {
			return errors.New("--prometheus-url is required for the prometheus provider")
		}
		prometheus, err := prometheusmcp.New(*prometheusURL, upstreamToken, nil)
		if err != nil {
			return err
		}
		server.Tools = prometheus.Tools()
		server.Name = "pagerless-prometheus-mcp"
	case "grafana":
		if *grafanaURL == "" {
			return errors.New("--grafana-url is required for the grafana provider")
		}
		grafana, err := grafanamcp.New(*grafanaURL, upstreamToken, nil)
		if err != nil {
			return err
		}
		server.Tools = grafana.Tools()
		server.Name = "pagerless-grafana-mcp"
	default:
		return errors.New("--provider must be kubernetes, github, prometheus or grafana")
	}

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- httpServer.ListenAndServe() }()
	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	}
}

func split(value string) []string {
	items := make([]string, 0, 4)
	for part := range strings.SplitSeq(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}
