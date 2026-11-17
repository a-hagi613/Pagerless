package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const healthcheckCommand = "healthcheck"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("expected serve, relay, or healthcheck subcommand")
	}
	switch args[0] {
	case "serve":
		return runServer(args[1:])
	case "relay":
		return runRelay(args[1:])
	case healthcheckCommand:
		return runHealthcheck(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func runHealthcheck(args []string) error {
	flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	address := flags.String("url", "http://127.0.0.1:8083/healthz", "loopback relay health URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	endpoint, err := url.Parse(*address)
	if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.Path != "/healthz" {
		return errors.New("healthcheck must use loopback HTTP /healthz")
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get(*address)
	if err != nil {
		return errors.New("the adapter relay is not healthy")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the adapter relay returned health status %d", response.StatusCode)
	}
	return nil
}

func runServer(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8080", "loopback-only host port-forwarded control API address")
	tlsListen := flags.String("tls-listen", ":8443", "HTTPS Git and GitHub API address")
	repository := flags.String("repository", "", "Sandbox owner/name")
	root := flags.String("repository-root", "/var/lib/pagerless", "directory that contains the bare repository")
	readTokenFile := flags.String("read-token-file", "/var/run/pagerless/read-token",
		"file containing the read-only adapter token")
	writeTokenFile := flags.String("write-token-file", "/var/run/pagerless/write-token",
		"file containing the disposable adapter write token")
	source := flags.String("source", "https://github.com/a-hagi613/pagerless-sandbox.git", "read-only Sandbox source URL")
	revision := flags.String("revision", "f865713644857f6717ca138056eb164c57be6e49", "pinned known-good Sandbox commit")
	cert := flags.String("tls-cert", "/var/run/pagerless/tls/tls.crt", "HTTPS certificate path")
	key := flags.String("tls-key", "/var/run/pagerless/tls/tls.key", "HTTPS private key path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !validRepository(*repository) {
		return errors.New("-repository must be <owner>/<name>")
	}
	repoRoot := filepath.Join(*root, filepath.FromSlash(*repository+".git"))
	if _, err := os.Stat(filepath.Join(repoRoot, "HEAD")); errors.Is(err, os.ErrNotExist) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := initializeRepository(ctx, repoRoot, *source, *revision); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("inspect the Sandbox bare repository: %w", err)
	}
	readToken, err := os.ReadFile(*readTokenFile)
	if err != nil {
		return fmt.Errorf("read the adapter read token: %w", err)
	}
	writeToken, err := os.ReadFile(*writeTokenFile)
	if err != nil {
		return fmt.Errorf("read the adapter write token: %w", err)
	}
	sandbox, err := newServer(configuration{
		repository: *repository,
		repoRoot:   repoRoot,
		baseCommit: *revision,
		readToken:  stringTrimSpace(readToken),
		writeToken: stringTrimSpace(writeToken),
	})
	if err != nil {
		return err
	}
	if sandbox.readToken == "" || sandbox.writeToken == "" || sandbox.readToken == sandbox.writeToken {
		return errors.New("the adapter read and write token files must contain distinct non-empty tokens")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	control := &http.Server{Addr: *listen, Handler: sandbox.controlRoutes(), ReadHeaderTimeout: 10 * time.Second}
	secure := &http.Server{Addr: *tlsListen, Handler: sandbox.routes(), ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 2)
	go func() { errCh <- control.ListenAndServe() }()
	go func() { errCh <- secure.ListenAndServeTLS(*cert, *key) }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve adapter: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = control.Shutdown(shutdown)
		_ = secure.Shutdown(shutdown)
		return nil
	}
}

func runRelay(args []string) error {
	flags := flag.NewFlagSet("relay", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8083", "loopback address exposed to GitHub MCP")
	target := flags.String("target", "", "HTTPS GitHub API adapter URL")
	ca := flags.String("ca-file", "", "PEM certificate authority for the adapter")
	if err := flags.Parse(args); err != nil {
		return err
	}
	handler, err := (&server{}).relay(*target, *ca)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve adapter relay: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func stringTrimSpace(value []byte) string {
	return strings.TrimSpace(string(value))
}
