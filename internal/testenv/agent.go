package testenv

import (
	"context"
	"fmt"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/server"
)

// StandInAgent plays an agent Job's pod: the pod exists, and the agent API
// client carries a projected service account token that the API server issued
// for it. No container runs.
type StandInAgent struct {
	// Pod is the pod the stand-in agent pretends to run in.
	Pod *corev1.Pod
	// Token is the projected service account token, bound to Pod, with the server's audience.
	Token string
	// API is a client for the agent API that authenticates with Token.
	API *agentapi.Client
}

// StartAgent creates the pod that the Job controller would create for the
// Page's agent Job, and a token for it, as the kubelet would project it.
// The Page must be Investigating.
func (e *Env) StartAgent(t testing.TB, pageName string) *StandInAgent {
	t.Helper()
	tr := e.WaitForPhase(t, pageName, v1alpha1.PhaseInvestigating)
	var job batchv1.Job
	jobKey := types.NamespacedName{Namespace: Namespace, Name: tr.Status.AgentJobName}
	if err := e.Client.Get(t.Context(), jobKey, &job); err != nil {
		t.Fatalf("get agent Job: %v", err)
	}
	pod := e.CreatePodFor(t, &job, server.DefaultAgentServiceAccount)
	token := e.TokenFor(t, pod, server.DefaultAgentServiceAccount, agentapi.AudienceForPage(string(tr.UID)))
	return &StandInAgent{
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

// CreatePodFor creates a pod controlled by the Job, from the Job's pod template,
// running as the service account. It does what the Job controller does.
func (e *Env) CreatePodFor(t testing.TB, job *batchv1.Job, serviceAccount string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		GenerateName: job.Name + "-",
		Namespace:    job.Namespace,
		Labels:       job.Spec.Template.Labels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         batchv1.SchemeGroupVersion.String(),
			Kind:               "Job",
			Name:               job.Name,
			UID:                job.UID,
			Controller:         new(true),
			BlockOwnerDeletion: new(true),
		}},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
	pod.Spec.ServiceAccountName = serviceAccount
	if err := e.Client.Create(t.Context(), pod); err != nil {
		t.Fatalf("create pod for Job %s: %v", job.Name, err)
	}
	return pod
}

// NewPod returns a pod in the harness namespace, running as the service
// account, that satisfies the restricted Pod Security Standard that the
// namespace enforces. No Job controls it.
func NewPod(name, serviceAccount string) *corev1.Pod {
	return &corev1.Pod{
		Name: name, Namespace: Namespace,
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   new(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "main",
				Image: AgentImage,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: new(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

// TokenFor requests a service account token bound to the pod, as the kubelet
// does for a projected volume.
func (e *Env) TokenFor(t testing.TB, pod *corev1.Pod, serviceAccount, audience string) string {
	t.Helper()
	request := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: new(int64(600)),
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       pod.Name,
				UID:        pod.UID,
			},
		},
	}
	accounts := e.Clientset.CoreV1().ServiceAccounts(pod.Namespace)
	issued, err := accounts.CreateToken(t.Context(), serviceAccount, request, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("request a token for service account %s: %v", serviceAccount, err)
	}
	return issued.Status.Token
}

// UnboundTokenFor requests a token that is not bound to any pod.
func (e *Env) UnboundTokenFor(t testing.TB, namespace, serviceAccount, audience string) string {
	t.Helper()
	request := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: new(int64(600))},
	}
	accounts := e.Clientset.CoreV1().ServiceAccounts(namespace)
	issued, err := accounts.CreateToken(t.Context(), serviceAccount, request, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("request a token for service account %s: %v", serviceAccount, err)
	}
	return issued.Status.Token
}

// ClientWithToken returns an agent API client for the Page that presents the token.
func (e *Env) ClientWithToken(pageName, token string) *agentapi.Client {
	var page v1alpha1.Page
	_ = e.Client.Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: pageName}, &page)
	return &agentapi.Client{
		BaseURL: e.ReportURL,
		Page:    pageName,
		PageUID: string(page.UID),
		Token:   func() (string, error) { return token, nil },
	}
}

// JobEnd is how a stand-in Job controller ends an agent Job.
type JobEnd int

const (
	// JobDeadlineExceeded ends the Job as the Job controller does when the active deadline passes.
	JobDeadlineExceeded JobEnd = iota
	// JobFailed ends the Job as failed for another reason, such as a crashed container.
	JobFailed
	// JobComplete ends the Job as completed, as when the agent exits successfully.
	JobComplete
)

// EndJob sets the Job's status as the Job controller would when the Job ends,
// since envtest runs no Job controller.
func (e *Env) EndJob(t testing.TB, jobName string, end JobEnd) {
	t.Helper()
	ctx := t.Context()
	var job batchv1.Job
	if err := e.Client.Get(ctx, types.NamespacedName{Namespace: Namespace, Name: jobName}, &job); err != nil {
		t.Fatalf("get Job %s: %v", jobName, err)
	}
	now := metav1.NewTime(e.Clock.Now())
	started := metav1.NewTime(e.Clock.Now().Add(-time.Minute))
	job.Status.StartTime = &started
	switch end {
	case JobDeadlineExceeded, JobFailed:
		reason, message := batchv1.JobReasonDeadlineExceeded, "Job was active longer than specified deadline"
		if end == JobFailed {
			reason, message = batchv1.JobReasonBackoffLimitExceeded, "Job has reached the specified backoff limit"
		}
		job.Status.Failed = 1
		for _, conditionType := range []batchv1.JobConditionType{batchv1.JobFailureTarget, batchv1.JobFailed} {
			job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
				Type: conditionType, Status: corev1.ConditionTrue, Reason: reason, Message: message,
				LastTransitionTime: now, LastProbeTime: now,
			})
		}
	case JobComplete:
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now, LastProbeTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now, LastProbeTime: now},
		}
	default:
		t.Fatalf("unknown Job end %d", end)
	}
	if err := e.Client.Status().Update(ctx, &job); err != nil {
		t.Fatalf("end Job %s: %v", jobName, err)
	}
}

// GetJob returns a Job in the harness namespace.
func (e *Env) GetJob(ctx context.Context, name string) (*batchv1.Job, error) {
	var job batchv1.Job
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, &job); err != nil {
		return nil, fmt.Errorf("get Job %s: %w", name, err)
	}
	return &job, nil
}
