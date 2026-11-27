package cli

import (
	"context"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/portforward"
)

const (
	testOpsNS     = "ops"
	testChartName = "pagerless"
)

func TestFindServiceUsesTheDocumentedName(t *testing.T) {
	service := &corev1.Service{Spec: corev1.ServiceSpec{
		Ports: []corev1.ServicePort{{Name: portHTTP, Port: 8080}},
	}}
	service.Name = "pagerless-server"
	service.Namespace = testReleaseNS
	found, err := findService(context.Background(), fake.NewClientset(service),
		[]string{testReleaseNS}, "pagerless-server")
	if err != nil {
		t.Fatalf("findService: %v", err)
	}
	if found.Name != "pagerless-server" {
		t.Fatalf("service = %q", found.Name)
	}
}

func TestFindServiceFallsBackToTheAPIService(t *testing.T) {
	// One custom Helm release exposes the API, the agent, and both MCP
	// Services. Only the API Service may receive the operator token.
	server := helmService(testOpsNS, "shop-server", "shop", serverComponent)
	objects := []runtime.Object{
		helmService(testOpsNS, "shop-agent", "shop", "agent"),
		helmService(testOpsNS, "shop-github-mcp", "shop", "github-mcp"),
		server,
		helmService(testOpsNS, "shop-kubernetes-mcp", "shop", "kubernetes-mcp"),
		// A Service that merely has pagerless in its name is not the API.
		helmService(testOpsNS, "pagerless-token-store", "shop", ""),
	}
	found, err := findService(context.Background(), fake.NewClientset(objects...),
		[]string{testOpsNS}, defaultServiceName)
	if err != nil {
		t.Fatalf("findService: %v", err)
	}
	if found.Name != server.Name {
		t.Fatalf("service = %q, want %q", found.Name, server.Name)
	}
}

func TestFindServiceFallsBackToTheLegacyFixedSelector(t *testing.T) {
	service := &corev1.Service{Spec: corev1.ServiceSpec{
		Selector: map[string]string{nameLabel: defaultServiceName},
		Ports:    []corev1.ServicePort{{Name: portHTTP, Port: 8080}},
	}}
	service.Name = defaultServiceName
	service.Namespace = testReleaseNS
	found, err := findService(context.Background(), fake.NewClientset(service),
		[]string{testReleaseNS}, defaultServiceName)
	if err != nil {
		t.Fatalf("findService: %v", err)
	}
	if found.Name != defaultServiceName {
		t.Fatalf("service = %q", found.Name)
	}
}

func TestFindServiceIgnoresAnUnrelatedServerComponent(t *testing.T) {
	// Another chart's component=server Service must not look like Pagerless.
	other := helmService(testOpsNS, "metrics-server", "metrics", serverComponent)
	delete(other.Labels, nameLabel)
	other.Labels["app.kubernetes.io/name"] = "metrics-server"
	other.Spec.Selector[nameLabel] = "metrics-server"
	_, err := findService(context.Background(), fake.NewClientset(other),
		[]string{testOpsNS}, defaultServiceName)
	if err == nil {
		t.Fatal("expected an error")
	}
	containsAll(t, ErrorText(err), "--service", "--address")
}

func TestFindServiceRefusesToGuessBetweenAPIInstalls(t *testing.T) {
	_, err := findService(context.Background(), fake.NewClientset(
		helmService(testOpsNS, "shop-server", "shop", serverComponent),
		helmService(testReleaseNS, "other-server", "other", serverComponent),
	), []string{testOpsNS, testReleaseNS}, defaultServiceName)
	if err == nil {
		t.Fatal("expected an error")
	}
	text := ErrorText(err)
	containsAll(t, text, "ops/shop-server", "pagerless/other-server",
		"--service", "--release-namespace", "--address")
}

func TestFindServiceRespectsAnExplicitService(t *testing.T) {
	// An explicit --service must not silently fall back to another install.
	_, err := findService(context.Background(), fake.NewClientset(
		helmService(testOpsNS, "shop-server", "shop", serverComponent),
	), []string{testOpsNS}, "missing-server")
	if err == nil {
		t.Fatal("expected an error")
	}
	containsAll(t, ErrorText(err), "missing-server", "--service", "--address")
}

// helmService builds the shape the chart renders: the public API Service's
// selector carries the component, and only metadata carries release labels.
func helmService(namespace, name, release, component string) *corev1.Service {
	selector := map[string]string{
		nameLabel:                    testChartName,
		"app.kubernetes.io/instance": release,
	}
	if component != "" {
		selector[componentLabel] = component
	}
	service := &corev1.Service{Spec: corev1.ServiceSpec{
		Selector: selector,
		Ports:    []corev1.ServicePort{{Name: portHTTP, Port: 8080}},
	}}
	service.Name = name
	service.Namespace = namespace
	service.Labels = map[string]string{
		nameLabel:                    testChartName,
		"app.kubernetes.io/instance": release,
	}
	return service
}

func TestPortForwardUsesAnEphemeralLoopbackPort(t *testing.T) {
	addresses, ports := portForwardSpec(8080)
	if len(addresses) != 1 || addresses[0] != "127.0.0.1" {
		t.Fatalf("addresses = %v, want only loopback", addresses)
	}
	if len(ports) != 1 || ports[0] != "0:8080" {
		t.Fatalf("ports = %v, want an ephemeral local port to 8080", ports)
	}
	// The real client-go constructor is the regression: it rejects a listen
	// address embedded in the port, as in "127.0.0.1:0:8080".
	forwarder, err := portforward.NewOnAddresses(nil, addresses, ports,
		make(chan struct{}), make(chan struct{}), io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("portforward.NewOnAddresses: %v", err)
	}
	if forwarder == nil {
		t.Fatal("expected a forwarder")
	}
}

func TestFindServiceExplainsAMissingInstall(t *testing.T) {
	_, err := findService(context.Background(), fake.NewClientset(),
		[]string{testReleaseNS}, "pagerless-server")
	if err == nil {
		t.Fatal("expected an error")
	}
	containsAll(t, ErrorText(err), "--address", "--release-namespace")
}

func TestFindPodPrefersAReadyRunningPod(t *testing.T) {
	service := &corev1.Service{Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": testReleaseNS},
	}}
	pending := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	pending.Name = "a-pending"
	pending.Namespace = testReleaseNS
	pending.Labels = service.Spec.Selector
	running := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
	}}
	running.Name = "b-running"
	running.Namespace = testReleaseNS
	running.Labels = service.Spec.Selector
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
		{Name: portHTTP, Port: 80, TargetPort: intstr.FromInt32(8080)},
	}}}
	port, err := targetPort(numeric, &corev1.Pod{})
	if err != nil || port != 8080 {
		t.Fatalf("numeric port = %d, %v", port, err)
	}
	named := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
		{Name: portHTTP, Port: 80, TargetPort: intstr.FromString("web")},
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
