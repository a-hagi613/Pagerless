package testenv

import (
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// rbacGrantTimeout bounds how long a grant may take to become visible. The
	// informer normally catches up in milliseconds; the bound exists so that a
	// broken grant fails the test instead of hanging it.
	rbacGrantTimeout = 20 * time.Second

	rbacGrantPollInterval = 50 * time.Millisecond
)

// Can reports whether the API server's authorizer lets the user, with the
// groups, perform the verb on the resource in the harness namespace.
func (e *Env) Can(t testing.TB, user string, groups []string, verb, group, resource, subresource string) bool {
	t.Helper()
	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user,
			Groups: groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   Namespace,
				Verb:        verb,
				Group:       group,
				Resource:    resource,
				Subresource: subresource,
			},
		},
	}
	result, err := e.Clientset.AuthorizationV1().SubjectAccessReviews().Create(t.Context(), review, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("review access of %s to %s %s: %v", user, verb, resource, err)
	}
	return result.Status.Allowed
}

// waitUntilCan blocks until the authorizer grants the verb, because a Role or a
// RoleBinding is effective only after the API server's authorization informer
// has observed it. A test that checks a grant it created a moment ago would
// otherwise race that informer and see the permissions from before the grant.
func (e *Env) waitUntilCan(t testing.TB, user, verb, group, resource, subresource string) {
	t.Helper()
	deadline := time.Now().Add(rbacGrantTimeout)
	for {
		if e.Can(t, user, nil, verb, group, resource, subresource) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not granted %s on %s in %s within %s", user, verb, resource, Namespace, rbacGrantTimeout)
		}
		time.Sleep(rbacGrantPollInterval)
	}
}
