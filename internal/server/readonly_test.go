package server_test

import (
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/internal/testenv"
)

// The verbs and resources the authorization matrix exercises.
const (
	verbGet    = "get"
	verbList   = "list"
	verbWatch  = "watch"
	verbCreate = "create"
	verbUpdate = "update"
	verbPatch  = "patch"
	verbDelete = "delete"

	groupApps      = "apps"
	groupBatch     = "batch"
	groupCore      = ""
	groupEvents    = "events.k8s.io"
	groupPagerless = "pagerless.dev"
	groupDiscovery = "discovery.k8s.io"

	resourceDeployments = "deployments"
	resourceSecrets     = "secrets"
	resourcePods        = "pods"
	resourceEvents      = "events"
)

// TestTheServerIdentityIsStrictlyReadOnly asks the API server's own authorizer
// what the chart's RBAC grants the server. The grant is the product's defining
// constraint: the server reads failures and proposes every change as a pull
// request, so no write to a workload, Event, or Node may be allowed.
func TestTheServerIdentityIsStrictlyReadOnly(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	user := env.ServerConfig.Impersonate.UserName
	cases := []struct {
		what      string
		verb      string
		namespace string
		group     string
		resource  string
		name      string
		allowed   bool
	}{
		{"read a Target", verbGet, testenv.SandboxNamespace, groupApps, resourceDeployments, "", true},
		{"list Targets", verbList, testenv.SandboxNamespace, groupApps, resourceDeployments, "", true},
		{"read pods", verbGet, testenv.SandboxNamespace, "", resourcePods, "", true},
		{"read a pod log", verbGet, testenv.SandboxNamespace, "", "pods/log", "", true},
		{"read Events", verbGet, testenv.SandboxNamespace, "", resourceEvents, "", true},
		{"read the durable Event API", verbList, testenv.SandboxNamespace, groupEvents, resourceEvents, "", true},
		{"read endpoints", verbList, testenv.SandboxNamespace, "discovery.k8s.io", "endpointslices", "", true},
		{"read nodes", verbList, "", "", "nodes", "", true},
		{"read Pages", verbGet, testenv.Namespace, groupPagerless, "pages", "", true},
		{"write Page status", verbUpdate, testenv.Namespace, groupPagerless, "pages/status", "", true},
		{"create an agent Job", verbCreate, testenv.Namespace, groupBatch, "jobs", "", true},
		{"read the credentials Secret", verbGet, testenv.Namespace, "", resourceSecrets, credentialsSecret, true},
		{"update the credentials Secret", verbUpdate, testenv.Namespace, "", resourceSecrets, credentialsSecret, true},

		{"update a Deployment", verbUpdate, testenv.SandboxNamespace, groupApps, resourceDeployments, "", false},
		{"patch a StatefulSet", verbPatch, testenv.SandboxNamespace, groupApps, "statefulsets", "", false},
		{"create a DaemonSet", verbCreate, testenv.SandboxNamespace, groupApps, "daemonsets", "", false},
		{"delete a pod", verbDelete, testenv.SandboxNamespace, "", resourcePods, "", false},
		{"delete a node", verbDelete, "", "", "nodes", "", false},
		{"create an Event", verbCreate, testenv.SandboxNamespace, "", resourceEvents, "", false},
		{"scale a Deployment", verbUpdate, testenv.SandboxNamespace, groupApps, "deployments/scale", "", false},
		{"exec into a pod", verbCreate, testenv.SandboxNamespace, "", "pods/exec", "", false},
		{"attach to a pod", verbCreate, testenv.SandboxNamespace, "", "pods/attach", "", false},
		{"port-forward to a pod", verbCreate, testenv.SandboxNamespace, "", "pods/portforward", "", false},
		{"mint a service account token", verbCreate, testenv.SandboxNamespace, "", "serviceaccounts/token", "", false},
		{"list Secrets", verbList, testenv.Namespace, "", resourceSecrets, "", false},
		{"watch Secrets", verbWatch, testenv.Namespace, "", resourceSecrets, "", false},
		{"create Secrets", verbCreate, testenv.Namespace, "", resourceSecrets, "", false},
		{"read another Secret", verbGet, testenv.Namespace, "", resourceSecrets, "some-other-secret", false},
		{"impersonate", "impersonate", "", "", "users", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			review, err := env.Clientset.AuthorizationV1().SubjectAccessReviews().Create(t.Context(),
				&authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
					User: user,
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: tc.namespace, Verb: tc.verb, Group: tc.group,
						Resource: tc.resource, Name: tc.name,
					},
				}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("review %s %s: %v", tc.verb, tc.resource, err)
			}
			if review.Status.Allowed != tc.allowed {
				t.Errorf("%s %s/%s = %v, want %v: %s",
					tc.verb, tc.group, tc.resource, review.Status.Allowed, tc.allowed, review.Status.Reason)
			}
		})
	}
}
