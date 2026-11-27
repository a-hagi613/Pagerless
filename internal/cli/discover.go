package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// forwardReadyTimeout bounds the wait for a local port-forward to accept.
const forwardReadyTimeout = 15 * time.Second

// loopbackAddress is the only interface a discovery tunnel binds. The client
// forwards the operator token through it, so it must never listen publicly.
const loopbackAddress = "127.0.0.1"

// Service discovery constants. The chart selects the public API pods with the
// component label, while the fixed Service name stays the documented default.
const (
	defaultServiceName = "pagerless-server"
	componentLabel     = "app.kubernetes.io/component"
	nameLabel          = "app.kubernetes.io/name"
	serverComponent    = "server"
)

// tunnel is an in-process port-forward to the installed service. It lives as
// long as the command that needs it.
type tunnel struct {
	close func()
}

// Close stops the forward. It is safe to call more than once.
func (t *tunnel) Close() {
	if t != nil && t.close != nil {
		t.close()
		t.close = nil
	}
}

// resolveBase returns the API base address: an explicit --address or
// PAGERLESS_ADDRESS wins, otherwise the service is discovered through the
// current kubeconfig and reached through a local port-forward.
func (a *app) resolveBase(ctx context.Context) (string, error) {
	address := strings.TrimSpace(a.opts.address)
	if address == "" {
		address = strings.TrimSpace(os.Getenv("PAGERLESS_ADDRESS"))
	}
	if address != "" {
		return strings.TrimSuffix(address, "/"), nil
	}
	address, forwarded, err := discoverService(ctx, a.opts)
	if err != nil {
		return "", err
	}
	a.tunnel = forwarded
	return address, nil
}

// discoverService locates the installed service in the current kube context and
// forwards a loopback port to its pod.
func discoverService(ctx context.Context, opts *options) (string, *tunnel, error) {
	restConfig, contextNamespace, err := kubeConfig(opts)
	if err != nil {
		return "", nil, err
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", nil, fmt.Errorf("create the Kubernetes client: %w", err)
	}
	namespaces := []string{opts.releaseNamespace}
	if contextNamespace != "" && contextNamespace != opts.releaseNamespace {
		namespaces = append(namespaces, contextNamespace)
	}
	service, err := findService(ctx, clientset, namespaces, opts.service)
	if err != nil {
		return "", nil, err
	}
	pod, err := findPod(ctx, clientset, service)
	if err != nil {
		return "", nil, err
	}
	port, err := targetPort(service, pod)
	if err != nil {
		return "", nil, fmt.Errorf("read the service port: %w", err)
	}
	return forwardPod(ctx, restConfig, clientset, pod, port)
}

// kubeConfig loads the selected context and its namespace.
func kubeConfig(opts *options) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = opts.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.kubeContext}
	loading := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	restConfig, err := loading.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf(
			"load kubeconfig (or pass --address to reach the service directly): %w", err)
	}
	namespace, _, err := loading.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("read the namespace from kubeconfig: %w", err)
	}
	return restConfig, namespace, nil
}

// findService looks for the documented fixed service name, then for the one
// Pagerless API Service in the candidate namespaces. It never guesses among
// several installs: the operator token goes to whichever address this returns.
func findService(
	ctx context.Context, clientset kubernetes.Interface, namespaces []string, name string,
) (*corev1.Service, error) {
	service, err := serviceNamed(ctx, clientset, namespaces, name)
	if err != nil {
		return nil, err
	}
	if service != nil {
		return service, nil
	}
	if name != defaultServiceName {
		return nil, fmt.Errorf(
			"no Pagerless service named %q in %s: check --service, pass --release-namespace, "+
				"or pass --address to reach an existing service",
			name, strings.Join(namespaces, " or "))
	}
	candidates, err := apiServerServices(ctx, clientset, namespaces)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 1 {
		return &candidates[0], nil
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf(
			"no Pagerless API service named %q in %s: install the chart, pass --release-namespace, "+
				"pass --service with the installed Service name, or pass --address to reach an "+
				"existing service",
			name, strings.Join(namespaces, " or "))
	}
	return nil, fmt.Errorf(
		"found %d Pagerless API services (%s): pass --service with one name, "+
			"--release-namespace to narrow the namespace, or --address to reach a known one",
		len(candidates), strings.Join(serviceNames(candidates), ", "))
}

// serviceNamed returns the Service with exactly this name, or nil when none of
// the candidate namespaces has it.
func serviceNamed(
	ctx context.Context, clientset kubernetes.Interface, namespaces []string, name string,
) (*corev1.Service, error) {
	for _, namespace := range namespaces {
		service, err := clientset.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return service, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("read service %s/%s: %w", namespace, name, err)
		}
	}
	return nil, nil
}

// apiServerServices lists the Services that front Pagerless's public API, and
// only those. A release also exposes agent and MCP Services; selecting one by a
// loose name match would send the operator token to the wrong workload.
func apiServerServices(
	ctx context.Context, clientset kubernetes.Interface, namespaces []string,
) ([]corev1.Service, error) {
	var candidates []corev1.Service
	seen := make(map[string]bool, len(namespaces))
	for _, namespace := range namespaces {
		if seen[namespace] {
			continue
		}
		seen[namespace] = true
		list, err := clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list services in %s: %w", namespace, err)
		}
		for i := range list.Items {
			item := list.Items[i]
			if len(item.Spec.Ports) > 0 && isAPIService(item) {
				candidates = append(candidates, item)
			}
		}
	}
	slices.SortFunc(candidates, func(left, right corev1.Service) int {
		if left.Namespace != right.Namespace {
			return strings.Compare(left.Namespace, right.Namespace)
		}
		return strings.Compare(left.Name, right.Name)
	})
	return candidates, nil
}

// isAPIService reports whether the Service fronts the public API. The current
// chart labels the selector, not the Service metadata, with the component; the
// earlier chart named and selected the API Service by the fixed name. A Service
// must also carry a Pagerless identity, so another chart's "server" component
// can never receive the operator token.
func isAPIService(service corev1.Service) bool {
	if service.Spec.Selector[nameLabel] == defaultServiceName {
		return true
	}
	if service.Spec.Selector[componentLabel] != serverComponent &&
		service.Labels[componentLabel] != serverComponent {
		return false
	}
	return identifiesPagerless(service)
}

// identifiesPagerless reports whether the Service's name or standard labels
// name this project. The Service name and every standard label count, so a
// release still matches when the chart's name is overridden.
func identifiesPagerless(service corev1.Service) bool {
	for _, value := range []string{
		service.Name,
		service.Labels[nameLabel],
		service.Labels["app.kubernetes.io/instance"],
		service.Labels["helm.sh/chart"],
		service.Spec.Selector[nameLabel],
		service.Spec.Selector["app.kubernetes.io/instance"],
	} {
		if strings.Contains(strings.ToLower(value), "pagerless") {
			return true
		}
	}
	return false
}

// serviceNames lists candidates as namespace/name for a diagnostic.
func serviceNames(services []corev1.Service) []string {
	names := make([]string, len(services))
	for i := range services {
		names[i] = services[i].Namespace + "/" + services[i].Name
	}
	return names
}

// findPod selects a ready running pod behind the service.
func findPod(ctx context.Context, clientset kubernetes.Interface, service *corev1.Service) (*corev1.Pod, error) {
	selector := service.Spec.Selector
	if len(selector) == 0 {
		selector = map[string]string{"app.kubernetes.io/name": service.Name}
	}
	list, err := clientset.CoreV1().Pods(service.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(selector).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list the service's pods: %w", err)
	}
	pods := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pod := list.Items[i]
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
			pods = append(pods, pod)
		}
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("service %s/%s has no running pod to forward to",
			service.Namespace, service.Name)
	}
	slices.SortFunc(pods, func(left, right corev1.Pod) int {
		return strings.Compare(left.Name, right.Name)
	})
	for i := range pods {
		if podReady(&pods[i]) {
			return &pods[i], nil
		}
	}
	return &pods[0], nil
}

// podReady reports whether the pod's Ready condition is true.
func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// targetPort resolves the container port behind the service's HTTP port.
func targetPort(service *corev1.Service, pod *corev1.Pod) (int32, error) {
	port, ok := httpServicePort(service)
	if !ok {
		return 0, fmt.Errorf("service %s/%s exposes no port", service.Namespace, service.Name)
	}
	switch port.TargetPort.Type {
	case intstr.Int:
		if port.TargetPort.IntVal > 0 {
			return port.TargetPort.IntVal, nil
		}
		return port.Port, nil
	case intstr.String:
		for i := range pod.Spec.Containers {
			for j := range pod.Spec.Containers[i].Ports {
				if pod.Spec.Containers[i].Ports[j].Name == port.TargetPort.StrVal {
					return pod.Spec.Containers[i].Ports[j].ContainerPort, nil
				}
			}
		}
		return 0, fmt.Errorf("pod %s has no container port named %q",
			pod.Name, port.TargetPort.StrVal)
	default:
		return port.Port, nil
	}
}

// httpServicePort prefers the port named http.
func httpServicePort(service *corev1.Service) (corev1.ServicePort, bool) {
	for _, port := range service.Spec.Ports {
		if port.Name == "http" {
			return port, true
		}
	}
	if len(service.Spec.Ports) > 0 {
		return service.Spec.Ports[0], true
	}
	return corev1.ServicePort{}, false
}

// portForwardSpec returns the listen addresses and local:remote port pairs for
// one discovered pod port.
func portForwardSpec(port int32) (addresses, ports []string) {
	return []string{loopbackAddress}, []string{fmt.Sprintf("0:%d", port)}
}

// forwardPod opens a loopback port-forward to one pod and waits until it is
// ready to accept connections.
func forwardPod(
	ctx context.Context, restConfig *rest.Config, clientset kubernetes.Interface,
	pod *corev1.Pod, port int32,
) (string, *tunnel, error) {
	request := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("portforward")
	roundTripper, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return "", nil, fmt.Errorf("prepare the port-forward: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, request.URL())
	stop := make(chan struct{})
	ready := make(chan struct{})
	failures := make(chan error, 1)
	addresses, forwardPorts := portForwardSpec(port)
	// client-go keeps the listen address separate from the port pair; an
	// address inside the pair ("127.0.0.1:0:8080") is an invalid port format.
	forwarder, err := portforward.NewOnAddresses(dialer, addresses, forwardPorts,
		stop, ready, io.Discard, io.Discard)
	if err != nil {
		return "", nil, fmt.Errorf("prepare the port-forward: %w", err)
	}
	var once sync.Once
	stopForward := func() { once.Do(func() { close(stop) }) }
	go func() { failures <- forwarder.ForwardPorts() }()
	timer := time.NewTimer(forwardReadyTimeout)
	defer timer.Stop()
	select {
	case <-ready:
	case err := <-failures:
		stopForward()
		return "", nil, fmt.Errorf("forward to pod %s: %w", pod.Name, err)
	case <-timer.C:
		stopForward()
		return "", nil, fmt.Errorf("forward to pod %s timed out after %s",
			pod.Name, forwardReadyTimeout)
	case <-ctx.Done():
		stopForward()
		return "", nil, ctx.Err()
	}
	ports, err := forwarder.GetPorts()
	if err != nil {
		stopForward()
		return "", nil, fmt.Errorf("read the forwarded port: %w", err)
	}
	if len(ports) == 0 {
		stopForward()
		return "", nil, fmt.Errorf("the port-forward opened no local port")
	}
	address := fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
	return address, &tunnel{close: stopForward}, nil
}
