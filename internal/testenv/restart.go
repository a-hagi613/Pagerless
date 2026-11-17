package testenv

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// PodReplacement plays a Deployment's ReplicaSet controller while the Executor
// restarts a Workload's pods: for every pod that disappears it creates a ready
// replacement, and it records the deletions in order. Envtest runs no
// controllers, so a test plays the one that restores the replicas.
type PodReplacement struct {
	replicas int32

	mu      sync.Mutex
	deleted []string
	waited  bool
	err     error
}

// Deleted returns the pods that disappeared, in the order they were observed.
func (p *PodReplacement) Deleted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.deleted)
}

// WaitedForEveryReplacement reports whether every deletion happened only after
// the previous replacement was ready. It is false when a second pod disappeared
// before the replicas had recovered, which is what deleting two pods at once
// looks like from the controller's side.
func (p *PodReplacement) WaitedForEveryReplacement() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waited
}

// Err returns the first failure to create a replacement, if any.
func (p *PodReplacement) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *PodReplacement) record(name string, waited bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, name)
	p.waited = p.waited && waited
}

func (p *PodReplacement) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
}

// ReplaceDeletedPods starts playing the ReplicaSet controller for the pods that
// the selector matches, keeping replicas of them until the test ends. Call it
// before approving a RestartWorkload Proposal.
func (e *Env) ReplaceDeletedPods(
	t testing.TB, namespace, selector string, replicas int32,
) *PodReplacement {
	t.Helper()
	replacement := &PodReplacement{replicas: replicas, waited: true}
	ctx, cancel := context.WithCancel(t.Context())
	watcher, err := e.Clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("watch the pods selected by %s: %v", selector, err)
	}
	t.Cleanup(func() {
		cancel()
		watcher.Stop()
	})
	go replacement.play(ctx, e, namespace, selector, watcher)
	return replacement
}

// play creates a ready replacement for each deleted pod.
func (p *PodReplacement) play(
	ctx context.Context, env *Env, namespace, selector string, watcher watch.Interface,
) {
	created := 0
	for event := range watcher.ResultChan() {
		if event.Type != watch.Deleted {
			continue
		}
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		ready := env.readyPods(ctx, namespace, selector)
		p.record(pod.Name, ready == int(p.replicas)-1)
		created++
		if err := env.createReplacement(ctx, namespace, fmt.Sprintf("replacement-%d", created), pod.Labels); err != nil {
			p.fail(err)
			return
		}
	}
}

// readyPods counts the pods that the selector matches and that are ready.
func (e *Env) readyPods(ctx context.Context, namespace, selector string) int {
	pods, err := e.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return -1
	}
	ready := 0
	for i := range pods.Items {
		if containersReady(&pods.Items[i]) {
			ready++
		}
	}
	return ready
}

// createReplacement creates a pod with the labels and marks its container ready.
func (e *Env) createReplacement(
	ctx context.Context, namespace, name string, labels map[string]string,
) error {
	pod := &corev1.Pod{}
	pod.Name, pod.Namespace, pod.Labels = name, namespace, labels
	pod.Spec = corev1.PodSpec{
		Containers: []corev1.Container{{Name: appContainer, Image: "registry.invalid/app:1.0"}},
	}
	if err := e.Client.Create(ctx, pod); err != nil {
		return fmt.Errorf("create replacement pod %s: %w", name, err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: appContainer, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	if err := e.Client.Status().Update(ctx, pod); err != nil {
		return fmt.Errorf("mark replacement pod %s ready: %w", name, err)
	}
	return nil
}

// containersReady reports whether every container of the pod is ready.
func containersReady(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}
