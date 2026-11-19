package testenv

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultVerificationPoll is how often the harness's server checks a Page that is being verified.
const defaultVerificationPoll = 100 * time.Millisecond

// appContainer is the name of the one container in the pods that tests create.
const appContainer = "app"

// WithVerificationPollInterval sets how often pagerless-server checks an Executing Page for a sync and observes
// a Verifying Page.
func WithVerificationPollInterval(d time.Duration) Option {
	return func(o *options) { o.verificationPoll = d }
}

// MarkDeploymentAvailable sets the status that the Deployment controller would set once the rollout of the
// current generation completes. Envtest runs no controllers, so a test plays them.
func (e *Env) MarkDeploymentAvailable(t testing.TB, namespace, name string) {
	t.Helper()
	e.SetDeploymentReplicas(t, namespace, name, 1, 1)
}

// SetDeploymentReplicas sets the Deployment's observed generation, its updated and total replicas, and its
// available replicas.
func (e *Env) SetDeploymentReplicas(t testing.TB, namespace, name string, updated, available int32) {
	t.Helper()
	var d appsv1.Deployment
	if err := e.Client.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &d); err != nil {
		t.Fatalf("get Deployment %s: %v", name, err)
	}
	d.Status.ObservedGeneration = d.Generation
	d.Status.Replicas = updated
	d.Status.UpdatedReplicas = updated
	d.Status.AvailableReplicas = available
	d.Status.ReadyReplicas = available
	if err := e.Client.Status().Update(t.Context(), &d); err != nil {
		t.Fatalf("update the status of Deployment %s: %v", name, err)
	}
}

// Termination describes a container's termination for CreateTargetPod.
type Termination struct {
	// Reason is the termination reason, such as OOMKilled.
	Reason string
	// FinishedAt is when the container ended.
	FinishedAt time.Time
	// Last puts the termination in the container's last state, as the kubelet does after it restarts the
	// container. Otherwise it is the container's current state.
	Last bool
}

// CreateTargetPod creates a pod with the labels in the namespace, and sets the status of its one container,
// named app. A nil termination means the container runs.
func (e *Env) CreateTargetPod(
	t testing.TB, namespace, name string, labels map[string]string, termination *Termination,
) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		Name:      name,
		Namespace: namespace,
		Labels:    labels,
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: appContainer, Image: "registry.invalid/app:1.0"}},
		},
	}
	if err := e.Client.Create(t.Context(), pod); err != nil {
		t.Fatalf("create pod %s: %v", name, err)
	}
	status := corev1.ContainerStatus{Name: appContainer, Ready: termination == nil}
	if termination == nil {
		status.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(e.Clock.Now())}
	} else {
		ended := &corev1.ContainerStateTerminated{
			Reason: termination.Reason, ExitCode: 137, FinishedAt: metav1.NewTime(termination.FinishedAt),
		}
		status.RestartCount = 1
		if termination.Last {
			status.LastTerminationState.Terminated = ended
			status.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(termination.FinishedAt)}
		} else {
			status.State.Terminated = ended
		}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
	if err := e.Client.Status().Update(t.Context(), pod); err != nil {
		t.Fatalf("update the status of pod %s: %v", name, err)
	}
	return pod
}
