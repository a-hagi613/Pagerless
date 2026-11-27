// Package kubemcp is the read-only Kubernetes MCP provider. It is deployed
// with its own ServiceAccount and a read-only ClusterRole, so the Kubernetes
// API enforces the same boundary as the code: no Secret, exec, attach, proxy or
// mutating request exists here.
package kubemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/a-hagi613/pagerless/internal/mcpserver"
)

const (
	defaultListLimit = 50
	maxListLimit     = 100
	maxLogBytes      = 256 << 10
	maxResultBytes   = 512 << 10

	groupApps               = "apps"
	keyKind                 = "kind"
	keyName                 = "name"
	keyNamespace            = "namespace"
	keyAdditionalProperties = "additionalProperties"
	keyType                 = "type"
	keyObject               = "object"
	keyProperties           = "properties"
	keyRequired             = "required"
	schemaInteger           = "integer"
	typeString              = "string"
)

// resource is one admitted kind.
type resource struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}

// resources is the complete read map. A kind absent here cannot be read even if
// discovery reported it.
var resources = map[string]resource{
	"pod":        {gvr: schema.GroupVersionResource{Version: "v1", Resource: "pods"}, namespaced: true},
	"service":    {gvr: schema.GroupVersionResource{Version: "v1", Resource: "services"}, namespaced: true},
	"configmap":  {gvr: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, namespaced: true},
	"event":      {gvr: schema.GroupVersionResource{Version: "v1", Resource: "events"}, namespaced: true},
	keyNamespace: {gvr: schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}},
	"node":       {gvr: schema.GroupVersionResource{Version: "v1", Resource: "nodes"}},
	"persistentvolumeclaim": {
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}, namespaced: true},
	"serviceaccount": {
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, namespaced: true},
	"deployment": {
		gvr: schema.GroupVersionResource{Group: groupApps, Version: "v1", Resource: "deployments"}, namespaced: true},
	"replicaset": {
		gvr: schema.GroupVersionResource{Group: groupApps, Version: "v1", Resource: "replicasets"}, namespaced: true},
	"statefulset": {
		gvr: schema.GroupVersionResource{Group: groupApps, Version: "v1", Resource: "statefulsets"}, namespaced: true},
	"daemonset": {
		gvr: schema.GroupVersionResource{Group: groupApps, Version: "v1", Resource: "daemonsets"}, namespaced: true},
	"job": {
		gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, namespaced: true},
	"cronjob": {
		gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, namespaced: true},
	"ingress": {
		gvr:        schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
		namespaced: true},
	"networkpolicy": {
		gvr:        schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
		namespaced: true},
	"horizontalpodautoscaler": {
		gvr: schema.GroupVersionResource{Group: "autoscaling", Version: "v2",
			Resource: "horizontalpodautoscalers"}, namespaced: true},
	"poddisruptionbudget": {
		gvr:        schema.GroupVersionResource{Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"},
		namespaced: true},
}

// Server is the read-only Kubernetes provider.
type Server struct {
	Dynamic    dynamic.Interface
	Clientset  kubernetes.Interface
	Discovery  discovery.DiscoveryInterface
	Namespaces []string
}

// New builds the provider from an in-cluster configuration or a kubeconfig.
func New(kubeconfig string, namespaces []string) (*Server, error) {
	var (
		config *rest.Config
		err    error
	)
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("build the Kubernetes configuration: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build the dynamic client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build the typed client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build the discovery client: %w", err)
	}
	return &Server{Dynamic: dynamicClient, Clientset: clientset, Discovery: discoveryClient, Namespaces: namespaces}, nil
}

// Tools returns the provider's read tools.
func (s *Server) Tools() []mcpserver.Tool {
	return []mcpserver.Tool{
		{
			Name: "api_resources",
			Description: "List the API resources the cluster advertises. " +
				"Discovery does not grant permission to read a resource.",
			InputSchema: schemaOf(map[string]any{keyType: keyObject,
				keyProperties: map[string]any{}, keyAdditionalProperties: false}),
			Call: s.apiResources,
		},
		{
			Name:        "namespaces_list",
			Description: "List namespaces with their status.",
			InputSchema: schemaOf(map[string]any{keyType: keyObject,
				keyProperties: map[string]any{}, keyAdditionalProperties: false}),
			Call: s.namespaces,
		},
		{
			Name:        "resources_list",
			Description: "List one admitted resource kind, optionally in a namespace and with label or field selectors.",
			InputSchema: schemaOf(map[string]any{
				keyType: keyObject,
				keyProperties: map[string]any{
					keyKind:         map[string]any{keyType: typeString},
					keyNamespace:    map[string]any{keyType: typeString},
					"labelSelector": map[string]any{keyType: typeString},
					"fieldSelector": map[string]any{keyType: typeString},
					"limit":         map[string]any{keyType: schemaInteger},
				},
				keyRequired:             []string{keyKind},
				keyAdditionalProperties: false,
			}),
			Call: s.list,
		},
		{
			Name:        "resources_get",
			Description: "Read one admitted resource by kind, namespace and name.",
			InputSchema: schemaOf(map[string]any{
				keyType: keyObject,
				keyProperties: map[string]any{
					keyKind:      map[string]any{keyType: typeString},
					keyNamespace: map[string]any{keyType: typeString},
					keyName:      map[string]any{keyType: typeString},
				},
				"required":              []string{"kind", "name"},
				keyAdditionalProperties: false,
			}),
			Call: s.get,
		},
		{
			Name:        "events_list",
			Description: "List Events, optionally for one kind and name, newest first.",
			InputSchema: schemaOf(map[string]any{
				keyType: keyObject,
				keyProperties: map[string]any{
					keyNamespace: map[string]any{keyType: typeString},
					keyKind:      map[string]any{keyType: typeString},
					keyName:      map[string]any{keyType: typeString},
					"limit":      map[string]any{keyType: schemaInteger},
				},
				keyAdditionalProperties: false,
			}),
			Call: s.events,
		},
		{
			Name:        "pods_log",
			Description: "Read a bounded tail of one pod's logs. The previous container instance may be requested.",
			InputSchema: schemaOf(map[string]any{
				keyType: keyObject,
				keyProperties: map[string]any{
					keyNamespace: map[string]any{keyType: typeString},
					keyName:      map[string]any{keyType: typeString},
					"container":  map[string]any{keyType: typeString},
					"tailLines":  map[string]any{keyType: schemaInteger},
					"previous":   map[string]any{"type": "boolean"},
				},
				"required":              []string{"namespace", "name"},
				keyAdditionalProperties: false,
			}),
			Call: s.logs,
		},
	}
}

func (s *Server) apiResources(_ context.Context, _ json.RawMessage) (string, error) {
	groups, err := s.Discovery.ServerPreferredResources()
	if err != nil && groups == nil {
		return "", fmt.Errorf("discover the API resources: %w", err)
	}
	type entry struct {
		GroupVersion string   `json:"groupVersion"`
		Name         string   `json:"name"`
		Kind         string   `json:"kind"`
		Namespaced   bool     `json:"namespaced"`
		Verbs        []string `json:"verbs"`
	}
	entries := make([]entry, 0, 128)
	for _, group := range groups {
		for _, resource := range group.APIResources {
			if len(entries) >= 500 {
				break
			}
			entries = append(entries, entry{
				GroupVersion: group.GroupVersion, Name: resource.Name, Kind: resource.Kind,
				Namespaced: resource.Namespaced, Verbs: resource.Verbs,
			})
		}
	}
	return encode(entries)
}

func (s *Server) namespaces(ctx context.Context, _ json.RawMessage) (string, error) {
	list, err := s.Dynamic.Resource(resources["namespace"].gvr).List(ctx, metav1.ListOptions{Limit: maxListLimit})
	if err != nil {
		return "", fmt.Errorf("list the namespaces: %w", err)
	}
	return encodeItems(list.Items, defaultListLimit)
}

type listArguments struct {
	Kind          string `json:"kind"`
	Namespace     string `json:"namespace"`
	LabelSelector string `json:"labelSelector"`
	FieldSelector string `json:"fieldSelector"`
	Limit         int    `json:"limit"`
}

func (s *Server) list(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments listArguments
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", errors.New("the list arguments are not valid")
	}
	kind, found := resources[strings.ToLower(strings.TrimSpace(arguments.Kind))]
	if !found {
		return "", fmt.Errorf("the kind %q is not admitted", arguments.Kind)
	}
	namespace, err := s.namespace(kind, arguments.Namespace)
	if err != nil {
		return "", err
	}
	limit := arguments.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	options := metav1.ListOptions{
		LabelSelector: arguments.LabelSelector, FieldSelector: arguments.FieldSelector, Limit: int64(limit),
	}
	var list *unstructured.UnstructuredList
	if kind.namespaced && namespace != "" {
		list, err = s.Dynamic.Resource(kind.gvr).Namespace(namespace).List(ctx, options)
	} else {
		list, err = s.Dynamic.Resource(kind.gvr).List(ctx, options)
	}
	if err != nil {
		return "", fmt.Errorf("list %s: %w", arguments.Kind, err)
	}
	return encodeItems(list.Items, limit)
}

type getArguments struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (s *Server) get(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments getArguments
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", errors.New("the get arguments are not valid")
	}
	kind, found := resources[strings.ToLower(strings.TrimSpace(arguments.Kind))]
	if !found {
		return "", fmt.Errorf("the kind %q is not admitted", arguments.Kind)
	}
	if strings.TrimSpace(arguments.Name) == "" {
		return "", errors.New("a name is required")
	}
	namespace, err := s.namespace(kind, arguments.Namespace)
	if err != nil {
		return "", err
	}
	var object *unstructured.Unstructured
	if kind.namespaced {
		if namespace == "" {
			return "", errors.New("a namespace is required")
		}
		object, err = s.Dynamic.Resource(kind.gvr).Namespace(namespace).Get(ctx, arguments.Name, metav1.GetOptions{})
	} else {
		object, err = s.Dynamic.Resource(kind.gvr).Get(ctx, arguments.Name, metav1.GetOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("read %s %s: %w", arguments.Kind, arguments.Name, err)
	}
	clean(object)
	encoded, err := json.Marshal(object.Object)
	if err != nil {
		return "", fmt.Errorf("encode the object: %w", err)
	}
	if len(encoded) > maxResultBytes {
		return "", fmt.Errorf("the object exceeds the %d byte evidence bound", maxResultBytes)
	}
	return string(encoded), nil
}

type eventArguments struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Limit     int    `json:"limit"`
}

func (s *Server) events(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments eventArguments
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", errors.New("the event arguments are not valid")
	}
	namespace, err := s.namespace(resources["event"], arguments.Namespace)
	if err != nil {
		return "", err
	}
	selectors := make([]string, 0, 2)
	if arguments.Kind != "" {
		selectors = append(selectors, "involvedObject.kind="+arguments.Kind)
	}
	if arguments.Name != "" {
		selectors = append(selectors, "involvedObject.name="+arguments.Name)
	}
	limit := arguments.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	options := metav1.ListOptions{FieldSelector: strings.Join(selectors, ","), Limit: int64(maxListLimit)}
	var list *unstructured.UnstructuredList
	if namespace != "" {
		list, err = s.Dynamic.Resource(resources["event"].gvr).Namespace(namespace).List(ctx, options)
	} else {
		list, err = s.Dynamic.Resource(resources["event"].gvr).List(ctx, options)
	}
	if err != nil {
		return "", fmt.Errorf("list the events: %w", err)
	}
	slices.SortStableFunc(list.Items, func(a, b unstructured.Unstructured) int {
		return eventTime(b).Compare(eventTime(a))
	})
	if len(list.Items) > limit {
		list.Items = list.Items[:limit]
	}
	return encodeItems(list.Items, limit)
}

func eventTime(item unstructured.Unstructured) time.Time {
	for _, path := range [][]string{{"lastTimestamp"}, {"eventTime"}, {"firstTimestamp"}} {
		value, found, _ := unstructured.NestedString(item.Object, path...)
		if !found || value == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

type logArguments struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Container string `json:"container"`
	TailLines int    `json:"tailLines"`
	Previous  bool   `json:"previous"`
}

func (s *Server) logs(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments logArguments
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", errors.New("the log arguments are not valid")
	}
	if _, err := s.namespace(resources["pod"], arguments.Namespace); err != nil {
		return "", err
	}
	if strings.TrimSpace(arguments.Namespace) == "" || strings.TrimSpace(arguments.Name) == "" {
		return "", errors.New("a namespace and a pod name are required")
	}
	tail := arguments.TailLines
	if tail <= 0 {
		tail = 100
	}
	if tail > 200 {
		tail = 200
	}
	tailLines := int64(tail)
	limit := int64(maxLogBytes)
	options := &corev1.PodLogOptions{
		Container: arguments.Container, Previous: arguments.Previous,
		TailLines: &tailLines, LimitBytes: &limit,
	}
	stream, err := s.Clientset.CoreV1().Pods(arguments.Namespace).GetLogs(arguments.Name, options).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("read the pod logs: %w", err)
	}
	defer func() { _ = stream.Close() }()
	payload, err := io.ReadAll(io.LimitReader(stream, maxLogBytes+1))
	if err != nil {
		return "", fmt.Errorf("read the pod logs: %w", err)
	}
	if len(payload) > maxLogBytes {
		payload = payload[:maxLogBytes]
		return string(payload) + "\n[truncated by the integration's log bound]", nil
	}
	return string(payload), nil
}

// namespace validates the namespace of a call.
func (s *Server) namespace(kind resource, requested string) (string, error) {
	namespace := strings.TrimSpace(requested)
	if !kind.namespaced {
		if namespace != "" {
			return "", errors.New("the kind is cluster-scoped")
		}
		return "", nil
	}
	if len(s.Namespaces) == 0 {
		return namespace, nil
	}
	if namespace == "" {
		return "", errors.New("the installation's namespace allowlist requires a namespace")
	}
	if slices.Contains(s.Namespaces, namespace) {
		return namespace, nil
	}
	return "", fmt.Errorf("the namespace %q is outside the installation's allowlist", namespace)
}

// encodeItems renders a bounded list.
func encodeItems(items []unstructured.Unstructured, limit int) (string, error) {
	encoded := make([]json.RawMessage, 0, len(items))
	total := 0
	truncated := false
	for index, item := range items {
		if index >= limit {
			truncated = true
			break
		}
		copied := item.DeepCopy()
		clean(copied)
		raw, err := json.Marshal(copied.Object)
		if err != nil {
			return "", fmt.Errorf("encode a result: %w", err)
		}
		total += len(raw)
		if total > maxResultBytes {
			truncated = true
			break
		}
		encoded = append(encoded, raw)
	}
	document := map[string]any{"items": encoded, "truncated": truncated}
	raw, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode the list: %w", err)
	}
	return string(raw), nil
}

// clean removes server-managed metadata that wastes the evidence budget.
func clean(object *unstructured.Unstructured) {
	unstructured.RemoveNestedField(object.Object, "metadata", "managedFields")
}

func encode(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode the result: %w", err)
	}
	if len(raw) > maxResultBytes {
		return "", fmt.Errorf("the result exceeds the %d byte evidence bound", maxResultBytes)
	}
	return string(raw), nil
}

// schemaOf renders an input schema.
func schemaOf(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic("kubemcp: encode a fixed schema: " + err.Error())
	}
	return raw
}
