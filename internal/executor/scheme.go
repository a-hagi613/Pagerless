package executor

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// DefaultServerServiceAccount is the service account of pagerless-server, the only caller the Executor accepts.
const DefaultServerServiceAccount = "pagerless-server"

// Scheme returns the scheme that the Executor uses: the Kubernetes types it
// needs for TokenReviews, and the Triage types. The Executor does not import
// pagerless-server, so that the process holding write credentials shares no
// code with the one that handles untrusted agent output.
func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register pagerless.dev types: %w", err)
	}
	return scheme, nil
}
