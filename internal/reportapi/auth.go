// Package reportapi is the HTTP endpoint through which an agent Job posts step
// events and its report to pagerless-server (ADR-0001).
package reportapi

import (
	"context"
	"errors"
	"fmt"
	"slices"

	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
)

const (
	podNameExtra = "authentication.kubernetes.io/pod-name"
	podUIDExtra  = "authentication.kubernetes.io/pod-uid"
)

var (
	// ErrUnauthenticated means the caller presented no valid token for this server.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden means the caller is authenticated but is not the Page's Job.
	ErrForbidden = errors.New("forbidden")
)

// Authenticator verifies that a request comes from the agent Job of one
// particular Page. It checks the projected service account token with a
// TokenReview, then follows the pod the token is bound to up to its Job and
// the Job up to the Page.
type Authenticator struct {
	// Client creates TokenReviews.
	Client client.Client
	// Reader reads Pods and Jobs directly from the API server.
	Reader client.Reader
	// Namespace is the namespace that holds Pages, agent Jobs, and the agent service account.
	Namespace string
	// ServiceAccount is the name of the agent service account.
	ServiceAccount string
}

// Caller is an authenticated agent pod.
type Caller struct {
	podName string
	podUID  string
}

// Identify verifies the token and returns the pod it is bound to. The token
// must be a valid token for the agent service account, issued for the
// server's audience and bound to a pod.
func (a *Authenticator) Identify(ctx context.Context, token string) (*Caller, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: no bearer token", ErrUnauthenticated)
	}
	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{agentapi.Audience}},
	}
	if err := a.Client.Create(ctx, review); err != nil {
		return nil, fmt.Errorf("review token: %w", err)
	}
	if !review.Status.Authenticated {
		return nil, fmt.Errorf("%w: token rejected", ErrUnauthenticated)
	}
	if !slices.Contains(review.Status.Audiences, agentapi.Audience) {
		return nil, fmt.Errorf("%w: token is not issued for audience %s", ErrUnauthenticated, agentapi.Audience)
	}
	wantUser := fmt.Sprintf("system:serviceaccount:%s:%s", a.Namespace, a.ServiceAccount)
	if review.Status.User.Username != wantUser {
		return nil, fmt.Errorf("%w: token does not belong to the agent service account", ErrForbidden)
	}
	podName := first(review.Status.User.Extra[podNameExtra])
	podUID := first(review.Status.User.Extra[podUIDExtra])
	if podName == "" || podUID == "" {
		return nil, fmt.Errorf("%w: token is not bound to a pod", ErrForbidden)
	}
	return &Caller{podName: podName, podUID: podUID}, nil
}

func first(values authenticationv1.ExtraValue) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// Authorize verifies that the caller's pod belongs to the Job that this
// Page started: the pod is controlled by the Job recorded in the Page's
// status, and that Job is controlled by the Page.
func (a *Authenticator) Authorize(ctx context.Context, caller *Caller, t *v1alpha1.Page) error {
	if t.Status.AgentJobName == "" {
		return fmt.Errorf("%w: the Page has no agent Job", ErrForbidden)
	}
	return a.authorizeJob(ctx, caller, t, t.Status.AgentJobName)
}

// AuthorizeNarrative verifies that the caller's pod belongs to the Narrative Job
// that this Page started.
func (a *Authenticator) AuthorizeNarrative(ctx context.Context, caller *Caller, t *v1alpha1.Page) error {
	if t.Status.NarrativeJobName == "" {
		return fmt.Errorf("%w: the Page has no Narrative Job", ErrForbidden)
	}
	return a.authorizeJob(ctx, caller, t, t.Status.NarrativeJobName)
}

// authorizeJob verifies that the caller's pod is controlled by the named Job and
// that the Job is controlled by the Page.
func (a *Authenticator) authorizeJob(ctx context.Context, caller *Caller, t *v1alpha1.Page, jobName string) error {
	var pod corev1.Pod
	if err := a.Reader.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: caller.podName}, &pod); err != nil {
		return fmt.Errorf("%w: pod %s not found", ErrForbidden, caller.podName)
	}
	if string(pod.UID) != caller.podUID {
		return fmt.Errorf("%w: pod identity changed", ErrForbidden)
	}
	var job batchv1.Job
	if err := a.Reader.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: jobName}, &job); err != nil {
		return fmt.Errorf("%w: the Page's Job was not found", ErrForbidden)
	}
	if !metav1.IsControlledBy(&job, t) || !metav1.IsControlledBy(&pod, &job) {
		return fmt.Errorf("%w: the caller is not the Job of Page %s", ErrForbidden, t.Name)
	}
	return nil
}
