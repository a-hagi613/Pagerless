package cli

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func TestFindServiceUsesTheDocumentedName(t *testing.T) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pagerless-server", Namespace: "pagerless"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}
	found, err := findService(context.Background(), fake.NewClientset(service), []string{"pagerless"}, "pagerless-server")
	if err != nil {
		t.Fatalf("findService: %v", err)
	}
	if found.Name != "pagerless-server" {
		t.Fatalf("service = %q", found.Name)
	}
}

func TestFindServiceFallsBackToAPagerlessService(t *testing.T) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pagerless", Namespace: "ops"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}
	found, err := findService(context.Background(), fake.NewClientset(service), []string{"ops"}, "missing")
	if err != nil {
		t.Fatalf("findService: %v", err)
	}
	if found.Name != "pagerless" {
		t.Fatalf("service = %q", found.Name)
	}
}

func TestFindServiceExplainsAMissingInstall(t *testing.T) {
	_, err := findService(context.Background(), fake.NewClientset(), []string{"pagerless"}, "pagerless-server")
	if err == nil {
		t.Fatal("expected an error")
	}
	containsAll(t, ErrorText(err), "--address", "--release-namespace")
}

func TestFindPodPrefersAReadyRunningPod(t *testing.T) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "pagerless-server", Namespace: "pagerless"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "pagerless"}},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "a-pending", Namespace: "pagerless", Labels: service.Spec.Selector},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "b-running", Namespace: "pagerless", Labels: service.Spec.Selector},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
	pod, err := findPod(context.Background(), fake.NewClientset(pending, running), service)
	if err != nil {
		t.Fatalf("findPod: %v", err)
	}
	if pod.Name != "b-running" {
		t.Fatalf("pod = %q", pod.Name)
	}
}

func TestTargetPortResolvesNumericAndNamedPorts(t *testing.T) {
	numeric := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)},
	}}}
	port, err := targetPort(numeric, &corev1.Pod{})
	if err != nil || port != 8080 {
		t.Fatalf("numeric port = %d, %v", port, err)
	}
	named := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Name: "http", Port: 80, TargetPort: intstr.FromString("web")},
	}}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 9090}}},
	}}}
	port, err = targetPort(named, pod)
	if err != nil || port != 9090 {
		t.Fatalf("named port = %d, %v", port, err)
	}
	if _, err := targetPort(named, &corev1.Pod{}); err == nil {
		t.Fatal("expected an error for a missing named port")
	}
}
