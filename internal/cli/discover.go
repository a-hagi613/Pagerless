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

// findService looks for the documented fixed service name, then for any
// pagerless service in the candidate namespaces.
func findService(
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
	for _, namespace := range namespaces {
		list, err := clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list services in %s: %w", namespace, err)
		}
		candidates := make([]corev1.Service, 0, len(list.Items))
		for i := range list.Items {
			item := list.Items[i]
			if strings.Contains(strings.ToLower(item.Name), "pagerless") && len(item.Spec.Ports) > 0 {
				candidates = append(candidates, item)
			}
		}
		slices.SortFunc(candidates, func(left, right corev1.Service) int {
			return strings.Compare(left.Name, right.Name)
		})
		if len(candidates) > 0 {
			return &candidates[0], nil
		}
	}
	return nil, fmt.Errorf(
		"no Pagerless service named %q in %s: install the chart, pass --release-namespace, "+
			"or pass --address to reach an existing service",
		name, strings.Join(namespaces, " or "))
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
	forwarder, err := portforward.New(dialer,
		[]string{fmt.Sprintf("127.0.0.1:0:%d", port)}, stop, ready, io.Discard, io.Discard)
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
