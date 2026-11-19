// Package cli implements kubectl-page, the kubectl plugin for Pages.
// Commands read and write Page resources through the Kubernetes API in the
// pagerless namespace, whatever namespace the user works in. They never call
// pagerless-server directly.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// PageNamespace is the namespace that holds every Page.
const PageNamespace = "pagerless"

// NewRootCommand returns the kubectl-page command. Its own action starts a
// Page for the target that its argument names. The subcommands list and show
// Pages, and approve or dismiss their Proposals.
func NewRootCommand() *cobra.Command {
	conn := &connection{}
	start := &startOptions{conn: conn}
	root := &cobra.Command{
		Use:   "kubectl page <pod/name | deploy/name | sts/name>",
		Short: "Start, list, and show Pages for failing Targets",
		Long: `Start a Page for the Target that owns a failing pod or that is named
directly, list Pages, show a Page's Diagnosis, Proposal, and outcome, and
approve or dismiss its Proposal.

A pod is resolved to its owning Deployment or StatefulSet. A Page targets the
Target in the namespace given by --namespace, or in the namespace of the
current context. Pages are stored in the pagerless namespace.`,
		Args:         cobra.ExactArgs(1),
		RunE:         start.run,
		SilenceUsage: true,
	}
	root.Flags().StringVar(&start.hint, "hint", "",
		"guidance for the agent, passed to it as human input (at most 1000 characters)")
	root.PersistentFlags().StringVar(&conn.kubeconfig, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	root.PersistentFlags().StringVar(&conn.context, "context", "", "kubeconfig context to use")
	root.PersistentFlags().StringVarP(&conn.namespace, "namespace", "n", "",
		"namespace of the Target (default: the namespace of the current context)")
	root.AddCommand(
		newListCommand(conn),
		newShowCommand(conn),
		newApproveCommand(conn),
		newDismissCommand(conn),
		newVersionCommand(),
	)
	return root
}

// connection holds the flags that select the cluster and the Target namespace.
type connection struct {
	kubeconfig string
	context    string
	namespace  string
}

// cluster is a client for the cluster that the kubeconfig selects.
type cluster struct {
	client client.WithWatch
	// namespace is the namespace of the Targets that the user works in.
	namespace string
}

// connect builds a client from the kubeconfig and the flags.
func (c *connection) connect() (*cluster, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = c.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: c.context}
	overrides.Context.Namespace = c.namespace
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	restConfig, err := config.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	namespace, _, err := config.Namespace()
	if err != nil {
		return nil, fmt.Errorf("read the namespace from kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
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
