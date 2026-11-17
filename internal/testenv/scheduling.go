package testenv

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// CreateNode creates a node that reports the allocatable resources, so that a
// test can exercise the bounds that apply to an unschedulable Workload. Envtest
// runs no scheduler and no kubelet, so the node never hosts a pod.
func (e *Env) CreateNode(t testing.TB, name string, allocatable corev1.ResourceList) *corev1.Node {
	t.Helper()
	node := &corev1.Node{}
	node.Name = name
	if err := e.Client.Create(t.Context(), node); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	node.Status.Capacity = allocatable.DeepCopy()
	node.Status.Allocatable = allocatable.DeepCopy()
	if err := e.Client.Status().Update(t.Context(), node); err != nil {
		t.Fatalf("set the status of node %s: %v", name, err)
	}
	return node
}
