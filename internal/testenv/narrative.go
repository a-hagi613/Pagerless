package testenv

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/server"
)

// StandInNarrative plays a Narrative Job's pod: the pod exists, and the agent API
// client carries a projected service account token that the API server issued for
// it. No container runs.
type StandInNarrative struct {
	// Job is the Narrative Job the Page started.
	Job *batchv1.Job
	// Pod is the pod the stand-in Job pretends to run in.
	Pod *corev1.Pod
	// Token is the projected service account token, bound to Pod, with the server's audience.
	Token string
	// API is a client for the agent API that authenticates with Token.
	API *agentapi.Client
}

// StartNarrative creates the pod that the Job controller would create for the
// Page's Narrative Job, and a token for it, as the kubelet would project it.
// The Page must have reached a terminal Verification outcome.
func (e *Env) StartNarrative(t testing.TB, pageName string) *StandInNarrative {
	t.Helper()
	tr := e.WaitFor(t, pageName, "the Narrative Job is recorded", func(tr *v1alpha1.Page) bool {
		return tr.Status.NarrativeJobName != ""
	})
	// The reconciler records the Job's name before it creates the Job, so that no
	// pod can start before the agent API can authorize it.
	var job batchv1.Job
	jobKey := types.NamespacedName{Namespace: Namespace, Name: tr.Status.NarrativeJobName}
	err := wait.PollUntilContextTimeout(t.Context(), pollInterval, waitTimeout, true,
		func(ctx context.Context) (bool, error) {
			if err := e.Client.Get(ctx, jobKey, &job); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("wait for Narrative Job %s: %v", jobKey.Name, err)
	}
	pod := e.CreatePodFor(t, &job, server.DefaultAgentServiceAccount)
	token := e.TokenFor(t, pod, server.DefaultAgentServiceAccount, agentapi.AudienceForPage(string(tr.UID)))
	return &StandInNarrative{
		Job:   &job,
		Pod:   pod,
		Token: token,
		API: &agentapi.Client{
			BaseURL: e.ReportURL,
			Page:    pageName,
			PageUID: string(tr.UID),
			Token:   func() (string, error) { return token, nil },
		},
	}
}
