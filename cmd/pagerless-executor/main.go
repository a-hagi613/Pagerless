// Command pagerless-executor is the only Pagerless component that holds write
// credentials (ADR-0005). It carries out an approved Proposal on request of
// pagerless-server, after confirming the Approval itself (ADR-0014).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/a-hagi613/pagerless/internal/executor"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pagerless-executor:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		namespace = flag.String("namespace", os.Getenv("POD_NAMESPACE"),
			"namespace that holds Triages and TriageApprovals (default: $POD_NAMESPACE)")
		bind       = flag.String("bind-address", ":8080", "address the Executor's API and health probe listen on")
		sandboxURL = flag.String("sandbox-url", os.Getenv("SANDBOX_URL"),
			"git URL of the Sandbox, which the Executor pushes to (default: $SANDBOX_URL)")
		sandboxBranch = flag.String("sandbox-branch", "main", "branch of the Sandbox that pull requests target")
		repository    = flag.String("github-repository", os.Getenv("PAGERLESS_GITHUB_REPOSITORY"),
			"Sandbox owner/name (default: $PAGERLESS_GITHUB_REPOSITORY, otherwise derived from --sandbox-url)")
		apiURL    = flag.String("github-api-url", executor.DefaultGitHubAPI, "base URL of GitHub's API")
		tokenFile = flag.String("github-token-file", "/var/run/pagerless/github/token",
			"file that holds the write-scoped GitHub token")
		serverAccount = flag.String("server-service-account", executor.DefaultServerServiceAccount,
			"service account that pagerless-server runs as, the only caller the Executor accepts")
	)
	zapOptions := zap.Options{}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))

	if *namespace == "" || *sandboxURL == "" {
		return errors.New("--namespace and --sandbox-url are required")
	}
	if *repository == "" {
		derived, err := executor.RepositoryFromURL(*sandboxURL)
		if err != nil {
			return fmt.Errorf("derive --github-repository: %w", err)
		}
		*repository = derived
	}
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	scheme, err := executor.Scheme()
	if err != nil {
		return err
	}
	// A plain client reads straight from the API server, so the Executor never acts on a stale cache.
	reader, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	token := func() (string, error) {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			return "", fmt.Errorf("read the GitHub token: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	runner, err := executor.New(executor.Config{
		Reader:    reader,
		Namespace: *namespace,
		Sandbox:   sandbox.Repository{URL: *sandboxURL, Branch: *sandboxBranch},
		GitHub:    &executor.GitHub{APIURL: *apiURL, Repository: *repository, Token: token},
		Token:     token,
	})
	if err != nil {
		return err
	}
	handler := &executor.Handler{
		Auth:     &executor.Authenticator{Client: reader, Namespace: *namespace, ServiceAccount: *serverAccount},
		Executor: runner,
	}

	listener, err := net.Listen("tcp", *bind)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ctx := ctrl.SetupSignalHandler()
	srv := &http.Server{Handler: handler.Routes(), ReadHeaderTimeout: readHeaderTimeout}
	ctrl.Log.WithName("setup").Info("starting pagerless-executor", "namespace", *namespace, "repository", *repository)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(listener) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
