// Package cli implements kubectl-page, the kubectl plugin for Pages. It runs
// `kubectl page` and never talks to GitHub. Commands either use the kubeconfig
// against the Kubernetes API, or, with --server, the Portal's API; the mode is
// reported on every run and is never chosen by silent fallback (ADR-0033).
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// DefaultPageNamespace is the release namespace that holds every Page and the
// credentials Secret, unless --release-namespace says otherwise.
const DefaultPageNamespace = "pagerless"

// Exit codes, documented in the root help so that a script can branch on them.
const (
	// ExitSuccess is a command that did what it was asked.
	ExitSuccess = 0
	// ExitFailure is a runtime failure: the cluster, the server, or the write.
	ExitFailure = 1
	// ExitUsage is an invalid argument, flag, or command line.
	ExitUsage = 2
	// ExitExternal means a write was refused because a secret manager owns the Secret.
	ExitExternal = 3
	// ExitStale means the Proposal changed between display and decision.
	ExitStale = 4
)

// Transport modes.
const (
	modeKubeconfig = "kubeconfig"
	modeServer     = "server"
)

// ExitError carries the process exit code that a failure maps to.
type ExitError struct {
	// Code is the documented exit code.
	Code int
	// Err is the failure.
	Err error
}

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
	return ExitFailure
}

// usagef returns an ExitUsage error.
func usagef(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

// exactlyOneArg reports cobra's arity failure as a usage error. Every command
// that takes an argument takes exactly one.
func exactlyOneArg(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return usagef("%v", err)
	}
	return nil
}

// noArgs reports cobra's arity failure as a usage error.
func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return usagef("%v", err)
	}
	return nil
}

// options holds the flags that select the transport and the namespaces.
type options struct {
	// kubeconfig is the path to the kubeconfig file.
	kubeconfig string
	// context is the kubeconfig context.
	context string
	// namespace is the namespace of the Targets.
	namespace string
	// pageNamespace is the release namespace that holds Pages and the Secret.
	pageNamespace string
	// mode is kubeconfig or server.
	mode string
	// server is the Portal base URL in server mode.
	server string
	// configPath is the CLI's local configuration file.
	configPath string
	// credentialsSecret is the Secret that holds the credentials in kubeconfig mode.
	credentialsSecret string
}

// NewRootCommand returns the kubectl-page command.
func NewRootCommand() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:   "page",
		Short: "Open, inspect, and decide Pages from the terminal",
		Long: `kubectl page drives Pagerless from a terminal and mirrors the Portal command
for command.

Commands are run in one of two modes. kubeconfig mode (the default) reads and
writes Page resources through the Kubernetes API, with the permissions of the
current kubeconfig user. server mode (--server <url>) calls the Portal's HTTP
API with the session that ` + "`kubectl page login`" + ` stored. The mode that was used is
reported on standard error, and a failure never falls back to the other mode.

Exit codes:
  0  the command succeeded
  1  the command failed
  2  the command line was invalid
  3  a write was refused because a secret manager owns the credentials Secret
  4  the Proposal changed between display and decision

No command accepts a secret value as a flag. A credential is read from an
interactive prompt without echo, from standard input, from a file, or from an
env file, so it never reaches shell history or a process listing.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.kubeconfig, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	root.PersistentFlags().StringVar(&opts.context, "context", "", "kubeconfig context to use")
	root.PersistentFlags().StringVarP(&opts.namespace, "namespace", "n", "",
		"namespace of the Target (default: the namespace of the current kubeconfig context)")
	root.PersistentFlags().StringVar(&opts.pageNamespace, "release-namespace", DefaultPageNamespace,
		"namespace that holds Pages and the credentials Secret")
	root.PersistentFlags().StringVar(&opts.mode, "mode", "",
		"transport: kubeconfig (default) or server")
	root.PersistentFlags().StringVar(&opts.server, "server", "",
		"Portal base URL; implies --mode server")
	root.PersistentFlags().StringVar(&opts.configPath, "config", "",
		"path to the CLI configuration file (default: ${XDG_CONFIG_HOME:-~/.config}/pagerless/config.yaml)")
	root.PersistentFlags().StringVar(&opts.credentialsSecret, "credentials-secret", "pagerless-credentials",
		"Secret that holds the credentials in kubeconfig mode")

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usagef("%v", err)
	})
	// Secret-bearing flags are accepted by the parser only so that they can be
	// refused with an explanation; they are hidden and always fail.
	for _, name := range forbiddenFlags {
		root.PersistentFlags().String(name, "", "")
		_ = root.PersistentFlags().MarkHidden(name)
	}

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return resolveMode(cmd, opts)
	}

	root.AddCommand(
		newNewCommand(opts),
		newGetCommand(opts),
		newListCommand(opts),
		newDiffCommand(opts),
		newApproveCommand(opts),
		newRejectCommand(opts, "reject", false),
		newRejectCommand(opts, "dismiss", true),
		newConfigCommand(opts),
		newLoginCommand(opts),
		newVersionCommand(opts),
	)
	return root
}

// forbiddenFlags are the secret-bearing flags that no command accepts.
var forbiddenFlags = []string{
	"api-key", "apikey", "ai-api-key", "token", "bootstrap-token", "admin-token",
	"password", "secret", "github-token", "connector-token", "session-token",
}

// refuseSecretFlags returns the documented refusal when a forbidden flag was
// given.
func refuseSecretFlags(cmd *cobra.Command) error {
	for _, name := range forbiddenFlags {
		if cmd.Flags().Changed(name) {
			return usagef("--%s is not accepted: a secret value may never be a flag; "+
				"use an interactive prompt, standard input, a file, or an env file", name)
		}
	}
	return nil
}

// resolveMode validates the transport selection. A missing --mode falls back to
// kubeconfig only when no server was named: the two are never mixed silently.
func resolveMode(cmd *cobra.Command, opts *options) error {
	if err := refuseSecretFlags(cmd); err != nil {
		return err
	}
	mode := strings.TrimSpace(opts.mode)
	if mode == "" {
		if opts.server != "" {
			mode = modeServer
		} else {
			mode = modeKubeconfig
		}
	}
	switch mode {
	case modeKubeconfig:
		if opts.server != "" {
			return usagef("--server selects server mode; drop --mode kubeconfig")
		}
	case modeServer:
		if strings.TrimSpace(opts.server) == "" {
			stored, err := loadConfig(opts.configPath)
			if err != nil {
				return err
			}
			if stored.Server == "" {
				return usagef("server mode needs --server <url> once; `kubectl page login --server <url>` stores it")
			}
			opts.server = stored.Server
		}
	default:
		return usagef("unknown mode %q: use kubeconfig or server", mode)
	}
	opts.mode = mode
	if cmd.Name() == "login" {
		return nil
	}
	return nil
}

// session is one command's transport: a cluster client, or a server client.
type session struct {
	mode string
	opts *options
	clus *cluster
	api  *apiClient
}

// openSession builds the transport and reports which mode it uses. The report
// goes to standard error so that machine-readable output stays clean.
func openSession(cmd *cobra.Command, opts *options) (*session, error) {
	s := &session{mode: opts.mode, opts: opts}
	switch opts.mode {
	case modeServer:
		stored, err := loadConfig(opts.configPath)
		if err != nil {
			return nil, err
		}
		api, err := newAPIClient(opts.server, stored.SessionToken)
		if err != nil {
			return nil, err
		}
		s.api = api
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "using server mode (%s)\n", opts.server)
	default:
		clus, err := connect(opts)
		if err != nil {
			return nil, err
		}
		s.clus = clus
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "using kubeconfig mode (%s)\n", clus.namespace)
	}
	return s, nil
}

// cluster is a client for the cluster that the kubeconfig selects.
type cluster struct {
	client client.WithWatch
	// namespace is the namespace of the Targets that the user works in.
	namespace string
}

// connect builds a client from the kubeconfig and the flags.
func connect(opts *options) (*cluster, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = opts.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.context}
	overrides.Context.Namespace = opts.namespace
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	restConfig, err := config.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	namespace, _, err := config.Namespace()
	if err != nil {
		return nil, fmt.Errorf("read the namespace from kubeconfig: %w", err)
	}
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register Kubernetes types: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register pagerless.dev types: %w", err)
	}
	cl, err := client.NewWithWatch(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return &cluster{client: cl, namespace: namespace}, nil
}
