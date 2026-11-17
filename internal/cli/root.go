// Package cli implements kubectl-triage, the kubectl plugin for Triages.
// Commands read and write Triage resources through the Kubernetes API in the
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

// TriageNamespace is the namespace that holds every Triage.
const TriageNamespace = "pagerless"

// NewRootCommand returns the kubectl-triage command. Its subcommands attach to
// this root.
func NewRootCommand() *cobra.Command {
	conn := &connection{}
	root := &cobra.Command{
		Use:          "kubectl triage",
		Short:        "Work with Triages of failing Workloads",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&conn.kubeconfig, "kubeconfig", "",
		"path to the kubeconfig file (default: $KUBECONFIG, then ~/.kube/config)")
	root.PersistentFlags().StringVar(&conn.context, "context", "", "kubeconfig context to use")
	root.PersistentFlags().StringVarP(&conn.namespace, "namespace", "n", "",
		"namespace of the Workload (default: the namespace of the current context)")
	root.AddCommand(newApproveCommand(conn), newRejectCommand(conn))
	return root
}

// connection holds the flags that select the cluster and the Workload namespace.
type connection struct {
	kubeconfig string
	context    string
	namespace  string
}

// cluster is a client for the cluster that the kubeconfig selects.
type cluster struct {
	client client.WithWatch
	// namespace is the namespace of the Workloads that the user works in.
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
