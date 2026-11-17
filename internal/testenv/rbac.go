package testenv

import (
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
