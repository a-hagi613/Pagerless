// Package cli implements kubectl-page, the kubectl plugin for Pagerless. Every
// command talks to the same investigation API as the dashboard, so both clients
// submit the same request, read the same transcript, and resume from the same
// event cursor.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Exit codes, documented in the root help so that a script can branch on them.
const (
	// ExitSuccess is a command that did what it was asked.
	ExitSuccess = 0
	// ExitFailure is a runtime failure: transport, server, or an unfinished turn.
	ExitFailure = 1
	// ExitUsage is an invalid argument, flag, target, or command line.
	ExitUsage = 2
	// ExitAuth means the operator token is missing or was refused.
	ExitAuth = 3
	// ExitNotFound means the page or evidence does not exist.
	ExitNotFound = 4
	// ExitConflict means another turn is active for the page.
	ExitConflict = 5
	// ExitInterrupted is a signal received before the command could detach.
	ExitInterrupted = 130
)

// Default timeouts. A request is bounded; a followed turn is bounded generously
// because a real investigation can run for a while.
const (
	defaultTimeout       = 30 * time.Second
	defaultFollowTimeout = time.Hour
)

// ExitError carries the process exit code that a failure maps to.
type ExitError struct {
	// Code is the documented exit code.
	Code int
	// Err is the failure.
	Err error
}

// Error returns the failure text.
func (e *ExitError) Error() string { return e.Err.Error() }

// Unwrap exposes the failure to errors.Is and errors.As.
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the process exit code for an error.
func ExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	if coded, ok := errors.AsType[*ExitError](err); ok {
		return coded.Code
	}
	if api, ok := errors.AsType[*apiError](err); ok {
		return api.exitCode()
	}
	if errors.Is(err, context.Canceled) {
		return ExitInterrupted
	}
	return ExitFailure
}

// ErrorText makes a failure safe to print on one terminal line.
func ErrorText(err error) string {
	if err == nil {
		return ""
	}
	return truncate(sanitize(err.Error()), 2000)
}

// usagef returns an ExitUsage error.
func usagef(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

// options holds the flags that select the transport and describe the request.
type options struct {
	// address is an explicit API base URL, bypassing service discovery.
	address string
	// tokenFile is the file holding the operator token; - reads standard input.
	tokenFile string
	// kubeconfig is the path to the kubeconfig file.
	kubeconfig string
	// kubeContext is the kubeconfig context.
	kubeContext string
	// releaseNamespace is the namespace that holds the installed service.
	releaseNamespace string
	// service is the installed service name to discover.
	service string
	// namespace is the optional namespace hint for the investigation.
	namespace string
	// targetHint is the optional Kubernetes object hint, as kind/name.
	targetHint string
	// json writes machine-readable JSON to standard output.
	json bool
	// timeout bounds one request.
	timeout time.Duration
	// followTimeout bounds a followed turn.
	followTimeout time.Duration
	// after resumes an event stream after this sequence; -1 means unset.
	after int64
	// fromStart ignores a stored resume point.
	fromStart bool
	// limit caps a page list; zero lists every page.
	limit int
	// open asks the dashboard shortcut to open the URL in a browser.
	open bool
	// noWait makes the dashboard shortcut stop after printing the address.
	noWait bool
}

// app holds one command's resolved transport and output streams.
type app struct {
	opts   *options
	cmd    *cobra.Command
	client *client
	tunnel *tunnel
}

// out is the machine-readable or primary result stream.
func (a *app) out() io.Writer { return a.cmd.OutOrStdout() }

// errOut is the progress and diagnostics stream.
func (a *app) errOut() io.Writer { return a.cmd.ErrOrStderr() }

// in is the input stream, used only to read a token from standard input.
func (a *app) in() io.Reader { return a.cmd.InOrStdin() }

// progressf writes a progress or diagnostic line without failing the command.
func (a *app) progressf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.errOut(), format, args...)
}

// close releases a port-forward opened by this command.
func (a *app) close() {
	if a.tunnel != nil {
		a.tunnel.Close()
		a.tunnel = nil
	}
}

// connect resolves the address and token once and reuses the API client.
func (a *app) connect(ctx context.Context) (*client, error) {
	if a.client != nil {
		return a.client, nil
	}
	token, err := a.resolveToken()
	if err != nil {
		return nil, err
	}
	address, err := a.resolveBase(ctx)
	if err != nil {
		return nil, err
	}
	api, err := newClient(address, token, a.opts.timeout)
	if err != nil {
		return nil, err
	}
	a.client = api
	return api, nil
}

// run binds a command body to this app and releases its transport afterwards.
func (a *app) run(body func(context.Context, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		a.cmd = cmd
		defer a.close()
		return body(cmd.Context(), args)
	}
}

// NewRootCommand returns the kubectl-page command.
func NewRootCommand() *cobra.Command {
	opts := &options{after: -1}
	instance := &app{opts: opts}
	root := &cobra.Command{
		Use:   "page [question...]",
		Short: "Ask why a Kubernetes workload is failing and follow the evidence",
		Long: `kubectl page opens an investigation from a terminal question and streams the
agent's tool calls, evidence, and answer. It uses the same API as the dashboard.

The server address is discovered from the current kube context: the plugin finds
the installed Service and forwards a loopback port to it. Pass --address to use
a reachable address directly. The operator token is read from PAGERLESS_TOKEN,
PAGERLESS_TOKEN_FILE, or --token-file; it is never a flag value, never printed,
and never placed in a URL.

Examples:
  kubectl page "Why is checkout timing out?" -n shop
  kubectl page "Which workloads have no memory limits?"
  kubectl page ask --target Deployment/checkout "Why is it restarting?"
  kubectl page list
  kubectl page show p-123
  kubectl page reply p-123 "What changed in the last deploy?"
  kubectl page follow p-123
  kubectl page activity p-123
  kubectl page evidence p-123 c-9
  kubectl page diff p-123
  kubectl page stop p-123
  kubectl page dashboard

Ctrl-C detaches from the stream and leaves the turn running; resume with
'kubectl page follow <pageId>'. Stopping a turn is always an explicit 'stop'.

Exit codes:
  0    the command succeeded
  1    the command failed, or the turn ended failed or was stopped
  2    the command line, target, or server request was invalid
  3    the operator token is missing or was refused
  4    the page or evidence was not found
  5    another turn is already active for the page
  130  interrupted before the command could detach`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: instance.run(func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return instance.cmd.Help()
			}
			return instance.ask(ctx, strings.Join(args, " "), "")
		}),
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usagef("%v", err)
	})
	persistent := root.PersistentFlags()
	persistent.StringVar(&opts.address, "address", "",
		"API base URL; skips service discovery")
	persistent.StringVar(&opts.tokenFile, "token-file", "",
		"file holding the operator token (- reads standard input)")
	persistent.StringVar(&opts.kubeconfig, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	persistent.StringVar(&opts.kubeContext, "context", "", "kubeconfig context to use")
	persistent.StringVar(&opts.releaseNamespace, "release-namespace", "pagerless",
		"namespace that holds the Pagerless service")
	persistent.StringVar(&opts.service, "service", "pagerless-server",
		"installed Service name to discover")
	persistent.StringVarP(&opts.namespace, "namespace", "n", "",
		"optional namespace hint for the investigation")
	persistent.BoolVar(&opts.json, "json", false,
		"write machine-readable JSON to standard output (follow writes one event per line); "+
			"progress stays on standard error")
	persistent.DurationVar(&opts.timeout, "timeout", defaultTimeout,
		"timeout for one API request")
	persistent.DurationVar(&opts.followTimeout, "follow-timeout", defaultFollowTimeout,
		"time to wait for a followed turn to finish")
	root.Flags().StringVar(&opts.targetHint, "target", "",
		"optional Kubernetes target, as kind/name or namespace/kind/name")
	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		if opts.timeout <= 0 {
			return usagef("--timeout must be greater than zero")
		}
		if opts.followTimeout <= 0 {
			return usagef("--follow-timeout must be greater than zero")
		}
		return nil
	}
	root.AddCommand(
		newAskCommand(instance),
		newListCommand(instance),
		newShowCommand(instance),
		newReplyCommand(instance),
		newFollowCommand(instance),
		newActivityCommand(instance),
		newEvidenceCommand(instance),
		newDiffCommand(instance),
		newStopCommand(instance),
		newDashboardCommand(instance),
		newVersionCommand(instance),
	)
	return root
}

// newAskCommand makes the prompt-first invocation available under an explicit
// verb when the question itself starts with a subcommand name.
func newAskCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ask <question...>",
		Short: "Open an investigation from a question",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("ask needs a question")
			}
			return nil
		},
		RunE: a.run(func(ctx context.Context, args []string) error {
			return a.ask(ctx, strings.Join(args, " "), "")
		}),
	}
	cmd.Flags().StringVar(&a.opts.targetHint, "target", "",
		"optional Kubernetes target, as kind/name or namespace/kind/name")
	return cmd
}
