package testenv

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/internal/executor"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

// defaultExecutionPoll is how often the harness's server asks whether a pull request has merged.
const defaultExecutionPoll = 100 * time.Millisecond

// WithExecutionPollInterval sets how often pagerless-server asks the Executor
// whether an open pull request has merged.
func WithExecutionPollInterval(d time.Duration) Option {
	return func(o *options) { o.executionPoll = d }
}

// SandboxBranch is the branch of the Sandbox that Argo CD deploys and that pull requests target.
const SandboxBranch = "main"

// ExecutorAccount is the Kubernetes username of pagerless-executor's service account.
const ExecutorAccount = "system:serviceaccount:" + Namespace + ":pagerless-executor"

// startExecutor runs pagerless-executor in process, on a loopback listener,
// under its own service account identity so that a test exercises the
// permissions that deploy/pagerless-executor grants. Its Sandbox is the
// harness's bare repository and its GitHub is the fake. It returns the base URL.
func startExecutor(t testing.TB, env *Env) string {
	t.Helper()
	scheme, err := executor.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	cfg := rest.CopyConfig(env.Config)
	cfg.QPS, cfg.Burst = -1, 0
	cfg.Impersonate = rest.ImpersonationConfig{UserName: ExecutorAccount}
	reader, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create a client with the Executor's identity: %v", err)
	}
	token := func() (string, error) { return env.GitHub.Token(), nil }
	runner, err := executor.New(executor.Config{
		Reader:    reader,
		Namespace: Namespace,
		Sandbox:   sandbox.Repository{URL: env.Sandbox.URL, Branch: SandboxBranch},
		GitHub:    &executor.GitHub{APIURL: env.GitHub.URL, Repository: GitHubRepository, Token: token},
		Token:     token,
	})
	if err != nil {
		t.Fatalf("create pagerless-executor: %v", err)
	}
	handler := &executor.Handler{
		Auth: &executor.Authenticator{
			Client: reader, Namespace: Namespace, ServiceAccount: executor.DefaultServerServiceAccount,
		},
		Executor: runner,
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the Executor: %v", err)
	}
	srv := &http.Server{Handler: handler.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + listener.Addr().String()
}

// issueToken requests a service account token for the audience. It reports a
// failure as an error, so that pagerless-server's token source can use it.
func (e *Env) issueToken(ctx context.Context, namespace, serviceAccount, audience string) (string, error) {
	request := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: new(int64(600))},
	}
	accounts := e.Clientset.CoreV1().ServiceAccounts(namespace)
	issued, err := accounts.CreateToken(ctx, serviceAccount, request, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("request a token for service account %s: %w", serviceAccount, err)
	}
	return issued.Status.Token, nil
}

// ExecutorClient returns a client for pagerless-executor that presents the
// token, or no token when it is empty.
func (e *Env) ExecutorClient(token string) *executorapi.Client {
	return &executorapi.Client{
		BaseURL: e.ExecutorURL,
		Token: func() (string, error) {
			if token == "" {
				return "", nil
			}
			return token, nil
		},
	}
}

// ServerExecutorClient returns a client for pagerless-executor that
// authenticates as pagerless-server does: a token of the server's service
// account whose audience is the Executor's.
func (e *Env) ServerExecutorClient(t testing.TB) *executorapi.Client {
	t.Helper()
	return e.ExecutorClient(e.TokenForAccount(t, "pagerless-server", executorapi.Audience))
}

// TokenForAccount requests a token that is not bound to a pod, for a service
// account in the harness namespace.
func (e *Env) TokenForAccount(t testing.TB, serviceAccount, audience string) string {
	t.Helper()
	token, err := e.issueToken(t.Context(), Namespace, serviceAccount, audience)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
