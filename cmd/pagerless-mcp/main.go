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
	"github.com/a-hagi613/pagerless/internal/kubemcp"
	"github.com/a-hagi613/pagerless/internal/mcpserver"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pagerless-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	provider := flag.String("provider", "", "kubernetes or github")
	listen := flag.String("listen", ":8091", "listen address")
	tokenFile := flag.String("token-file", "", "file holding the installer-provided bearer credential")
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
	default:
		return errors.New("--provider must be kubernetes or github")
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
