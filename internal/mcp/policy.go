package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrDenied means the policy refused the call before it reached the server.
var ErrDenied = errors.New("denied")

// JSON argument keys shared by the Kubernetes and GitHub policies.
const (
	keyKind       = "kind"
	keyName       = "name"
	keyNamespace  = "namespace"
	keyAPIVersion = "apiVersion"
	pathAppsV1    = "apps/v1"
)

// Tool budgets applied by the policies.
const (
	// MaxArgumentBytes bounds one tool argument document.
	MaxArgumentBytes = 16 << 10
	// MaxLogTailLines bounds requested log lines.
	MaxLogTailLines = 200
	// MaxListItems bounds one list request.
	MaxListItems = 100
)

// Registry combines the admitted tools of every integration. A model may name
// only a tool the registry advertises, and a name is owned by one integration.
type Registry struct {
	adapters map[string]Adapter
	tools    []Tool
	order    []string
}

// Adapter is one capability-filtered integration.
type Adapter interface {
	// Tools returns the tools the policy admits.
	Tools(ctx context.Context) ([]Tool, error)
	// Call validates and executes one admitted tool.
	Call(ctx context.Context, name string, arguments json.RawMessage) (Result, error)
}

// NewRegistry builds the admitted tool set. Duplicate names are an error: two
// integrations must never be able to answer for one name.
func NewRegistry(ctx context.Context, adapters map[string]Adapter) (*Registry, error) {
	fetched := make(map[string][]Tool, len(adapters))
	for label, adapter := range adapters {
		tools, err := adapter.Tools(ctx)
		if err != nil {
			return nil, fmt.Errorf("admit the %s tools: %w", label, err)
		}
		fetched[label] = tools
	}
	return newRegistry(adapters, fetched)
}

// newRegistry assembles a registry from tool lists that were already fetched.
func newRegistry(adapters map[string]Adapter, fetched map[string][]Tool) (*Registry, error) {
	registry := &Registry{adapters: make(map[string]Adapter)}
	seen := make(map[string]string)
	for _, label := range sortedKeys(adapters) {
		adapter := adapters[label]
		for _, tool := range fetched[label] {
			if previous, found := seen[tool.Name]; found {
				return nil, fmt.Errorf("the tool %q is advertised by %s and %s", tool.Name, previous, label)
			}
			seen[tool.Name] = label
			registry.adapters[tool.Name] = adapter
			registry.tools = append(registry.tools, tool)
			registry.order = append(registry.order, tool.Name)
		}
	}
	return registry, nil
}

// Tools returns the admitted tools in a stable order.
func (r *Registry) Tools() []Tool { return append([]Tool(nil), r.tools...) }

// Has reports whether a tool is admitted.
func (r *Registry) Has(name string) bool {
	_, found := r.adapters[name]
	return found
}

// Call executes an admitted tool. An unknown name is refused, never forwarded.
func (r *Registry) Call(ctx context.Context, name string, arguments json.RawMessage) (Result, error) {
	adapter, found := r.adapters[name]
	if !found {
		return Result{}, fmt.Errorf("%w: the tool %q is not admitted", ErrDenied, name)
	}
	return adapter.Call(ctx, name, arguments)
}

// Names returns the admitted tool names.
func (r *Registry) Names() []string { return append([]string(nil), r.order...) }

func sortedKeys(adapters map[string]Adapter) []string {
	keys := make([]string, 0, len(adapters))
	for key := range adapters {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// readableKinds is the complete set of resource kinds the cluster identity may
// read through the Kubernetes integration. Discovery never extends it: a kind
// absent here can be listed by the discovery API and still be unreadable.
var readableKinds = map[string]string{
	"pod":                     "v1",
	"service":                 "v1",
	"configmap":               "v1",
	"event":                   "v1",
	keyNamespace:              "v1",
	"node":                    "v1",
	"persistentvolumeclaim":   "v1",
	"serviceaccount":          "v1",
	"deployment":              pathAppsV1,
	"replicaset":              pathAppsV1,
	"statefulset":             pathAppsV1,
	"daemonset":               pathAppsV1,
	"job":                     "batch/v1",
	"cronjob":                 "batch/v1",
	"ingress":                 "networking.k8s.io/v1",
	"networkpolicy":           "networking.k8s.io/v1",
	"horizontalpodautoscaler": "autoscaling/v2",
	"poddisruptionbudget":     "policy/v1",
}

// forbiddenKinds are named explicitly so a future allowlist edit cannot admit
// them by accident and so the refusal reason is stable.
var forbiddenKinds = map[string]bool{
	"secret": true,
}

// Kubernetes is the read-only capability policy over the Kubernetes MCP server.
type Kubernetes struct {
	Client *Client
	// Namespaces restricts reads to these namespaces. Empty means every
	// namespace, which is what a cluster-scoped read-only grant supports.
	Namespaces []string
}

// Tools lists the server's tools and keeps only the admitted read verbs.
func (k *Kubernetes) Tools(ctx context.Context) ([]Tool, error) {
	tools, err := k.Client.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	admitted := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if !admittedKubernetesTool(tool.Name) {
			continue
		}
		admitted = append(admitted, tool)
	}
	if len(admitted) == 0 {
		return nil, fmt.Errorf("the kubernetes MCP server advertises none of the admitted read tools")
	}
	return admitted, nil
}

// Call validates one read and forwards only a canonical argument set.
func (k *Kubernetes) Call(ctx context.Context, name string, arguments json.RawMessage) (Result, error) {
	if !admittedKubernetesTool(name) {
		return Result{}, fmt.Errorf("%w: %q is not a read tool", ErrDenied, name)
	}
	if len(arguments) > MaxArgumentBytes {
		return Result{}, fmt.Errorf("%w: the arguments exceed %d bytes", ErrDenied, MaxArgumentBytes)
	}
	var raw map[string]any
	if err := json.Unmarshal(arguments, &raw); err != nil {
		return Result{}, fmt.Errorf("%w: the arguments are not a JSON object", ErrDenied)
	}
	canonical, err := k.canonical(name, raw)
	if err != nil {
		return Result{}, err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return Result{}, fmt.Errorf("encode the %s arguments: %w", name, err)
	}
	return k.Client.CallTool(ctx, name, encoded)
}

// canonical validates one call and returns only the fields the tool accepts.
func (k *Kubernetes) canonical(name string, raw map[string]any) (map[string]any, error) {
	switch name {
	case "api_resources", "namespaces_list":
		return map[string]any{}, nil
	case "resources_list":
		kind, err := k.kind(raw)
		if err != nil {
			return nil, err
		}
		namespace, err := k.namespace(raw, kind, false)
		if err != nil {
			return nil, err
		}
		out := map[string]any{keyKind: kind}
		if namespace != "" {
			out[keyNamespace] = namespace
		}
		if selector := boundedString(raw, "labelSelector", 512); selector != "" {
			out["labelSelector"] = selector
		}
		if selector := boundedString(raw, "fieldSelector", 512); selector != "" {
			out["fieldSelector"] = selector
		}
		if limit := boundedInt(raw, "limit", MaxListItems); limit > 0 {
			out["limit"] = limit
		}
		if apiVersion := boundedString(raw, keyAPIVersion, 64); apiVersion != "" {
			out[keyAPIVersion] = apiVersion
		}
		return out, nil
	case "resources_get":
		kind, err := k.kind(raw)
		if err != nil {
			return nil, err
		}
		namespace, err := k.namespace(raw, kind, true)
		if err != nil {
			return nil, err
		}
		resourceName := boundedString(raw, keyName, 253)
		if resourceName == "" {
			return nil, fmt.Errorf("%w: resources_get requires a name", ErrDenied)
		}
		out := map[string]any{keyKind: kind, keyName: resourceName}
		if namespace != "" {
			out[keyNamespace] = namespace
		}
		return out, nil
	case "events_list":
		namespace, err := k.namespace(raw, "event", false)
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		if namespace != "" {
			out[keyNamespace] = namespace
		}
		if kind := boundedString(raw, keyKind, 64); kind != "" {
			if _, err := k.kind(map[string]any{keyKind: kind}); err != nil {
				return nil, err
			}
			out[keyKind] = kind
		}
		if target := boundedString(raw, keyName, 253); target != "" {
			out[keyName] = target
		}
		if limit := boundedInt(raw, "limit", MaxListItems); limit > 0 {
			out["limit"] = limit
		}
		return out, nil
	case "pods_log":
		namespace, err := k.namespace(raw, "pod", true)
		if err != nil {
			return nil, err
		}
		if namespace == "" {
			return nil, fmt.Errorf("%w: pods_log requires a namespace", ErrDenied)
		}
		pod := boundedString(raw, keyName, 253)
		if pod == "" {
			return nil, fmt.Errorf("%w: pods_log requires a pod name", ErrDenied)
		}
		out := map[string]any{keyNamespace: namespace, keyName: pod}
		if container := boundedString(raw, "container", 253); container != "" {
			out["container"] = container
		}
		tail := boundedInt(raw, "tailLines", MaxLogTailLines)
		if tail <= 0 {
			tail = 100
		}
		out["tailLines"] = tail
		if previous, ok := raw["previous"].(bool); ok && previous {
			out["previous"] = true
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: %q is not admitted", ErrDenied, name)
	}
}

func (k *Kubernetes) kind(raw map[string]any) (string, error) {
	kind := boundedString(raw, keyKind, 64)
	if kind == "" {
		return "", fmt.Errorf("%w: the call requires a kind", ErrDenied)
	}
	lower := strings.ToLower(kind)
	if forbiddenKinds[lower] {
		return "", fmt.Errorf("%w: Secret objects are outside the read grant", ErrDenied)
	}
	canonical, found := readableKinds[lower]
	if !found {
		return "", fmt.Errorf("%w: the kind %q is not in the read allowlist", ErrDenied, kind)
	}
	if requested := boundedString(raw, keyAPIVersion, 64); requested != "" && requested != canonical {
		return "", fmt.Errorf("%w: the kind %q is served by %s, not %s", ErrDenied, kind, canonical, requested)
	}
	return lower, nil
}

// namespace validates the namespace of a call. A cluster-scoped kind carries no
// namespace. A namespaced get or log requires one. A namespaced list may be
// cluster-wide only when the installation has no namespace allowlist, so an
// omission can never widen a restricted installation's read.
func (k *Kubernetes) namespace(raw map[string]any, kind string, require bool) (string, error) {
	namespace := boundedString(raw, keyNamespace, 253)
	if clusterScoped(kind) {
		if namespace != "" {
			return "", fmt.Errorf("%w: the kind %q is cluster-scoped", ErrDenied, kind)
		}
		return "", nil
	}
	if namespace == "" {
		if require || len(k.Namespaces) > 0 {
			return "", fmt.Errorf("%w: the namespace is required for %q", ErrDenied, kind)
		}
		return "", nil
	}
	if len(k.Namespaces) == 0 {
		return namespace, nil
	}
	if slices.Contains(k.Namespaces, namespace) {
		return namespace, nil
	}
	return "", fmt.Errorf("%w: the namespace %q is outside the installation's allowlist", ErrDenied, namespace)
}

// ParseNamespaces reads a comma-separated namespace allowlist.
func ParseNamespaces(value string) []string {
	namespaces := make([]string, 0, 4)
	for part := range strings.SplitSeq(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			namespaces = append(namespaces, trimmed)
		}
	}
	return namespaces
}

func clusterScoped(kind string) bool {
	switch kind {
	case "node", "namespace":
		return true
	default:
		return false
	}
}

func admittedKubernetesTool(name string) bool {
	switch name {
	case "resources_list", "resources_get", "events_list", "pods_log", "namespaces_list", "api_resources":
		return true
	default:
		return false
	}
}

// GitHub is the read-only capability policy over the GitHub MCP server. The
// repository and path prefix are fixed by the installation, so the model
// cannot aim a read at another repository.
type GitHub struct {
	Client *Client
	// Repository is the configured owner/name, used for diagnostics.
	Repository string
	// PathPrefix, when set, bounds every read to that repository directory.
	PathPrefix string
}

// Tools lists the admitted repository read tools.
func (g *GitHub) Tools(ctx context.Context) ([]Tool, error) {
	tools, err := g.Client.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	admitted := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if admittedGitHubTool(tool.Name) {
			admitted = append(admitted, tool)
		}
	}
	if len(admitted) == 0 {
		return nil, fmt.Errorf("the GitHub MCP server advertises none of the admitted read tools")
	}
	return admitted, nil
}

// Call validates one repository read.
func (g *GitHub) Call(ctx context.Context, name string, arguments json.RawMessage) (Result, error) {
	if !admittedGitHubTool(name) {
		return Result{}, fmt.Errorf("%w: %q is not a repository read", ErrDenied, name)
	}
	if len(arguments) > MaxArgumentBytes {
		return Result{}, fmt.Errorf("%w: the arguments exceed %d bytes", ErrDenied, MaxArgumentBytes)
	}
	var raw map[string]any
	if err := json.Unmarshal(arguments, &raw); err != nil {
		return Result{}, fmt.Errorf("%w: the arguments are not a JSON object", ErrDenied)
	}
	canonical := map[string]any{}
	if name != "github_repository" {
		path, err := g.path(boundedString(raw, "path", 512))
		if err != nil {
			return Result{}, err
		}
		canonical["path"] = path
		if ref := boundedString(raw, "ref", 128); ref != "" {
			canonical["ref"] = ref
		}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return Result{}, fmt.Errorf("encode the %s arguments: %w", name, err)
	}
	return g.Client.CallTool(ctx, name, encoded)
}

func (g *GitHub) path(requested string) (string, error) {
	path := strings.TrimPrefix(strings.TrimSpace(requested), "/")
	if path == "" || strings.Contains(path, "..") || strings.Contains(path, `\`) || strings.Contains(path, "\x00") {
		return "", fmt.Errorf("%w: the repository path is not a relative path", ErrDenied)
	}
	if g.PathPrefix == "" {
		return path, nil
	}
	prefix := strings.Trim(strings.TrimSpace(g.PathPrefix), "/")
	if prefix == "" {
		return path, nil
	}
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		return "", fmt.Errorf("%w: the path %q is outside %q", ErrDenied, path, prefix)
	}
	return path, nil
}

func admittedGitHubTool(name string) bool {
	switch name {
	case "github_repository", "github_get_file", "github_list_directory":
		return true
	default:
		return false
	}
}

func boundedString(raw map[string]any, key string, limit int) string {
	value, ok := raw[key].(string)
	if !ok {
		return ""
	}
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return ""
	}
	return value
}

func boundedInt(raw map[string]any, key string, max int) int {
	value, ok := raw[key].(float64)
	if !ok {
		return 0
	}
	if value < 0 {
		return 0
	}
	if value > float64(max) {
		return max
	}
	return int(value)
}
