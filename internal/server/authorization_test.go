package server_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/internal/testenv"
)

const updateVerb = "update"

func TestServerMayValidateAWorkloadButCannotUpdateItLive(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	asServer, err := client.New(env.ServerConfig, client.Options{Scheme: env.Client.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: sandboxNamespace, Name: checkout}
	// Admission policy caches are asynchronous. Wait until this identity is denied an unchanged live update
	// before testing mutations; the readiness probe cannot change the Workload's desired state.
	eventually(t, "the dry-run-only policy is enforced", func() bool {
		probe := liveDeployment(t, env, checkout)
		err := asServer.Update(t.Context(), probe)
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "pagerless-server-dry-run-only")
	})
	var update appsv1.Deployment
	if err := asServer.Get(t.Context(), key, &update); err != nil {
		t.Fatal(err)
	}
	update.Spec.Replicas = new(int32(2))
	if err := asServer.Update(t.Context(), &update, client.DryRunAll); err != nil {
		t.Fatalf("server-side dry run was denied: %v", err)
	}
	if got := liveDeployment(t, env, checkout); *got.Spec.Replicas != 1 {
		t.Fatalf("dry run persisted %d replicas", *got.Spec.Replicas)
	}
	update = *liveDeployment(t, env, checkout)
	update.Spec.Replicas = new(int32(3))
	err = asServer.Update(t.Context(), &update)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("live update = %v, want the admission policy's dry-run-only refusal", err)
	}
	if got := liveDeployment(t, env, checkout); *got.Spec.Replicas != 1 {
		t.Errorf("refused server update persisted %d replicas", *got.Spec.Replicas)
	}
	update = *liveDeployment(t, env, checkout)
	update.Spec.Replicas = new(int32(2))
	if err := env.Client.Update(t.Context(), &update); err != nil {
		t.Fatalf("another identity was denied by the server-only admission policy: %v", err)
	}
	for _, verb := range []string{"get", updateVerb} {
		review, err := env.Clientset.AuthorizationV1().SubjectAccessReviews().Create(t.Context(),
			&authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User: env.ServerConfig.Impersonate.UserName,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: "outside", Verb: verb, Group: "apps", Resource: "deployments",
				},
			}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed {
			t.Errorf("server may %s Workloads outside the PatchResources allowlist", verb)
		}
	}

	const statefulName = "ledger-authorization"
	stateful := &appsv1.StatefulSet{
		Name: statefulName, Namespace: sandboxNamespace,
		Spec: appsv1.StatefulSetSpec{
			Replicas: new(int32(1)), ServiceName: statefulName,
			Selector: update.Spec.Selector, Template: update.Spec.Template,
		},
	}
	if err := env.Client.Create(t.Context(), stateful); err != nil {
		t.Fatal(err)
	}
	stateful.Spec.Replicas = new(int32(2))
	if err := asServer.Update(t.Context(), stateful, client.DryRunAll); err != nil {
		t.Fatalf("StatefulSet dry run was denied: %v", err)
	}
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(stateful), stateful); err != nil {
		t.Fatal(err)
	}
	if *stateful.Spec.Replicas != 1 {
		t.Fatal("StatefulSet dry run changed the stored replicas")
	}
	stateful.Spec.Replicas = new(int32(2))
	if err := asServer.Update(t.Context(), stateful); !apierrors.IsForbidden(err) {
		t.Fatalf("live StatefulSet update = %v, want Forbidden", err)
	}
}
