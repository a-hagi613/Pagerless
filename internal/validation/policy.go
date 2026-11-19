package validation

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// allowedKinds are the kinds that a Proposal may change. The always-denied set
// is refused before this list is consulted, so a kind appears here only when a
// bounded change to it is a legitimate remediation.
var allowedKinds = map[string]bool{
	"Deployment": true, "StatefulSet": true, "DaemonSet": true, "Job": true,
	"CronJob": true, "Pod": true, "PersistentVolumeClaim": true, "Service": true,
	"ConfigMap": true, "Ingress": true, "HorizontalPodAutoscaler": true,
	"PodDisruptionBudget": true,
}

// deniedKinds are always refused, whatever the Proposal says.
var deniedKinds = map[string]bool{
	"Secret": true, "ClusterRole": true, "ClusterRoleBinding": true, "Role": true,
	"RoleBinding": true, "ServiceAccount": true, "CustomResourceDefinition": true,
	"Namespace": true, "Node": true, "PriorityClass": true, "StorageClass": true,
	"ValidatingWebhookConfiguration": true, "MutatingWebhookConfiguration": true,
	"APIService": true, "PodSecurityPolicy": true, "LimitRange": true,
	"ResourceQuota": true, "NetworkPolicy": true,
}

// The reasons that most refused paths share, so the policy table stays legible.
const (
	automationReason  = "Pagerless never edits repository automation"
	kustomizeReason   = "Kustomize wiring is never an edit target"
	chartWiringReason = "chart wiring is never an edit target"
)

// refusedPaths are the path patterns that Validation never edits.
var refusedPaths = []struct {
	pattern string
	reason  string
}{
	{".github/", automationReason},
	{".gitlab-ci.yml", automationReason},
	{".circleci/", automationReason},
	{".buildkite/", automationReason},
	{"jenkinsfile", automationReason},
	{"azure-pipelines.yml", automationReason},
	{".drone.yml", automationReason},
	{"bitbucket-pipelines.yml", automationReason},
	{"kustomization.yaml", kustomizeReason},
	{"kustomization.yml", kustomizeReason},
	{"chart.yaml", chartWiringReason},
	{"chart.lock", chartWiringReason},
	{"requirements.yaml", chartWiringReason},
	{"requirements.lock", chartWiringReason},
	{"values.schema.json", "a values schema is never an edit target"},
	{"templates/", "chart templates are never edit targets"},
	{"crds/", "CustomResourceDefinition manifests are never edit targets"},
	{".sops.yaml", "encryption configuration is never an edit target"},
	{"sealedsecret", "encrypted Secrets are never edit targets"},
	{"kubeconfig", "kubeconfig files are never edit targets"},
	{"encryption-config", "encryption configuration is never an edit target"},
}

// checkPathPolicy returns the refusal for a path, or an empty string.
func checkPathPolicy(p string) string {
	switch {
	case p == "":
		return "the path is empty"
	case strings.HasPrefix(p, "/"), strings.Contains(p, "\\"):
		return fmt.Sprintf("%s must be relative to the repository root", p)
	case strings.Contains(p, "://"):
		return fmt.Sprintf("%s names another repository", p)
	}
	cleaned := path.Clean(p)
	if cleaned != p || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Sprintf("%s is not a clean path inside the repository", p)
	}
	lower := strings.ToLower(cleaned)
	if !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
		return fmt.Sprintf("%s is not a YAML file", p)
	}
	for _, refused := range refusedPaths {
		pattern := refused.pattern
		switch {
		case strings.HasSuffix(pattern, "/"):
			if strings.Contains("/"+lower+"/", "/"+pattern) || strings.HasPrefix(lower, pattern) {
				return fmt.Sprintf("%s is refused: %s", p, refused.reason)
			}
		case strings.Contains(pattern, "/"):
			if strings.Contains("/"+lower+"/", "/"+pattern) {
				return fmt.Sprintf("%s is refused: %s", p, refused.reason)
			}
		default:
			if path.Base(lower) == pattern {
				return fmt.Sprintf("%s is refused: %s", p, refused.reason)
			}
		}
	}
	return ""
}

// builtInGroups are the API groups whose kinds the allowlist enumerates. A
// kind outside this list and outside the always-denied set is a custom
// resource, which a Target Repository may legitimately define.
var builtInGroups = map[string]bool{
	"": true, "apps": true, "batch": true, "autoscaling": true, "policy": true,
	"networking.k8s.io": true, "rbac.authorization.k8s.io": true,
	"admissionregistration.k8s.io": true, "apiextensions.k8s.io": true,
	"storage.k8s.io": true, "scheduling.k8s.io": true, "node.k8s.io": true,
	"certificates.k8s.io": true, "authentication.k8s.io": true,
	"authorization.k8s.io": true, "coordination.k8s.io": true,
	"apiregistration.k8s.io": true, "discovery.k8s.io": true,
}

// checkKindPolicy returns the refusal for a document kind, or an empty string.
func checkKindPolicy(apiVersion, kind string) string {
	if deniedKinds[kind] {
		return fmt.Sprintf("kind %s is always refused: Pagerless never changes credentials or cluster-wide authority", kind)
	}
	if allowedKinds[kind] {
		return ""
	}
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		group = ""
	}
	if !builtInGroups[group] {
		return ""
	}
	return fmt.Sprintf("kind %s in group %q is not in the allowlist of kinds a Proposal may change", kind, group)
}

// serverManagedFields are the fields that the API server owns. A Proposal that
// carries one is refused, because the Proposal is a desired state, not a
// snapshot.
var serverManagedFields = []string{
	"metadata.creationTimestamp", "metadata.deletionGracePeriodSeconds",
	"metadata.deletionTimestamp", "metadata.generation", "metadata.managedFields",
	"metadata.resourceVersion", "metadata.selfLink", "metadata.uid", "status",
}

// compareIdentity returns the ways a proposed document is not the base
// document. Identity is preserved and server-managed fields do not appear.
func compareIdentity(base, proposed map[string]any) []string {
	var refusals []string
	for _, field := range []string{"apiVersion", "kind"} {
		if base[field] != proposed[field] {
			refusals = append(refusals, fmt.Sprintf("%s changed from %v to %v", field, base[field], proposed[field]))
		}
	}
	baseMetadata, _ := base["metadata"].(map[string]any)
	proposedMetadata, _ := proposed["metadata"].(map[string]any)
	for _, field := range []string{"name", "namespace"} {
		if baseMetadata[field] != proposedMetadata[field] {
			refusals = append(refusals, fmt.Sprintf(
				"metadata.%s changed from %v to %v", field, baseMetadata[field], proposedMetadata[field]))
		}
	}
	for _, field := range serverManagedFields {
		if _, present := fieldValue(proposed, field); present {
			refusals = append(refusals, fmt.Sprintf("%s is server-managed and must not appear", field))
		}
	}
	return refusals
}

// fieldValue returns the value at a dotted path, and whether it is present.
func fieldValue(object map[string]any, dotted string) (any, bool) {
	current := any(object)
	for key := range strings.SplitSeq(dotted, ".") {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, present := mapping[key]
		if !present {
			return nil, false
		}
		current = value
	}
	return current, true
}

// objectAt returns the mapping at a dotted path, or nil.
func objectAt(object map[string]any, dotted string) map[string]any {
	value, _ := fieldValue(object, dotted)
	mapping, _ := value.(map[string]any)
	return mapping
}

// podSpec returns the PodSpec of a workload, or nil.
func podSpec(doc map[string]any) map[string]any {
	kind, _ := doc["kind"].(string)
	spec := objectAt(doc, "spec")
	if spec == nil {
		return nil
	}
	switch kind {
	case "Pod":
		return spec
	case "CronJob":
		return objectAt(doc, "spec.jobTemplate.spec.template.spec")
	default:
		return objectAt(doc, "spec.template.spec")
	}
}

// containers returns a PodSpec's containers by kind and name, with both the
// regular and the init containers.
func containers(spec map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, group := range []string{"containers", "initContainers"} {
		list, _ := spec[group].([]any)
		for _, entry := range list {
			container, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			name, _ := container["name"].(string)
			out[group+"/"+name] = container
		}
	}
	return out
}

// checkDifferences records the forbidden differences between a base and a
// proposed document, and the permitted ones as warnings.
func (r *Result) checkDifferences(filePath string, base, proposed map[string]any) {
	baseSpec, proposedSpec := podSpec(base), podSpec(proposed)
	if proposedSpec != nil {
		for _, field := range []string{"hostNetwork", "hostPID", "hostIPC"} {
			enabled, _ := proposedSpec[field].(bool)
			baseEnabled, _ := baseSpec[field].(bool)
			if enabled && !baseEnabled {
				r.refuse("files", "%s: %s is forbidden", filePath, field)
			}
		}
		r.checkVolumes(filePath, baseSpec, proposedSpec)
		r.checkContainers(filePath, baseSpec, proposedSpec)
		r.checkServiceAccount(filePath, baseSpec, proposedSpec)
		r.checkNodePlacement(filePath, baseSpec, proposedSpec)
	}
	r.checkSelector(filePath, base, proposed)
	r.checkReplicas(filePath, base, proposed)
}

// checkVolumes refuses a new hostPath volume.
func (r *Result) checkVolumes(filePath string, baseSpec, proposedSpec map[string]any) {
	baseHostPaths := hostPaths(baseSpec)
	list, _ := proposedSpec["volumes"].([]any)
	for _, entry := range list {
		volume, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		hostPath, ok := volume["hostPath"].(map[string]any)
		if !ok {
			continue
		}
		encoded, _ := json.Marshal(hostPath)
		if !baseHostPaths[string(encoded)] {
			name, _ := volume["name"].(string)
			r.refuse("files", "%s: volume %s mounts a hostPath, which is forbidden", filePath, name)
		}
	}
}

// hostPaths returns the hostPath volumes of a PodSpec.
func hostPaths(spec map[string]any) map[string]bool {
	out := map[string]bool{}
	if spec == nil {
		return out
	}
	list, _ := spec["volumes"].([]any)
	for _, entry := range list {
		volume, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if hostPath, ok := volume["hostPath"].(map[string]any); ok {
			encoded, _ := json.Marshal(hostPath)
			out[string(encoded)] = true
		}
	}
	return out
}

// checkContainers refuses privileged containers, capability additions, and
// service-account identity changes, and records image and resource changes.
func (r *Result) checkContainers(filePath string, baseSpec, proposedSpec map[string]any) {
	baseContainers := containers(baseSpec)
	for name, container := range containers(proposedSpec) {
		baseContainer := baseContainers[name]
		field := fmt.Sprintf("%s.containers[%s]", filePath, name)
		security := objectAt(container, "securityContext")
		baseSecurity := objectAt(baseContainer, "securityContext")
		if privileged, _ := security["privileged"].(bool); privileged {
			basePrivileged, _ := baseSecurity["privileged"].(bool)
			if !basePrivileged {
				r.refuse("files", "%s: securityContext.privileged is forbidden", field)
			}
		}
		r.checkCapabilities(field, baseSecurity, security)
		if image, _ := container["image"].(string); image != "" {
			if baseImage, _ := baseContainer["image"].(string); baseImage != image {
				r.warn(v1alpha1.WarningImage, field+".image",
					"the image changes from %s to %s", baseImage, image)
			}
		}
		r.checkResources(field, baseContainer, container)
	}
}

// checkCapabilities refuses a capability addition outside the small allowlist.
func (r *Result) checkCapabilities(field string, baseSecurity, security map[string]any) {
	added := capabilitiesOf(security, "add")
	baseAdded := capabilitiesOf(baseSecurity, "add")
	for capability := range added {
		if baseAdded[capability] {
			continue
		}
		if capability != "NET_BIND_SERVICE" {
			r.refuse("files", "%s: capability %s is outside the allowlist", field, capability)
		}
	}
}

// capabilitiesOf returns the set of capabilities in one list.
func capabilitiesOf(security map[string]any, list string) map[string]bool {
	out := map[string]bool{}
	capabilities := objectAt(security, "capabilities")
	entries, _ := capabilities[list].([]any)
	for _, entry := range entries {
		if value, ok := entry.(string); ok {
			out[value] = true
		}
	}
	return out
}

// checkServiceAccount refuses any change to the service account identity.
func (r *Result) checkServiceAccount(filePath string, baseSpec, proposedSpec map[string]any) {
	for _, field := range []string{"serviceAccountName", "automountServiceAccountToken"} {
		if !reflect.DeepEqual(baseSpec[field], proposedSpec[field]) {
			r.refuse("files", "%s: %s may not change", filePath, field)
		}
	}
	if !reflect.DeepEqual(baseSpec["imagePullSecrets"], proposedSpec["imagePullSecrets"]) {
		r.refuse("files", "%s: imagePullSecrets may not change", filePath)
	}
}

// checkNodePlacement records a node placement change as a warning.
func (r *Result) checkNodePlacement(filePath string, baseSpec, proposedSpec map[string]any) {
	for _, field := range []string{"nodeSelector", "affinity", "tolerations", "topologySpreadConstraints", "nodeName"} {
		if reflect.DeepEqual(baseSpec[field], proposedSpec[field]) {
			continue
		}
		r.warn(v1alpha1.WarningNodePlacement, filePath+": spec."+field,
			"node placement changes through %s", field)
	}
}

// checkSelector refuses a selector change.
func (r *Result) checkSelector(filePath string, base, proposed map[string]any) {
	baseSelector, basePresent := fieldValue(base, "spec.selector")
	proposedSelector, proposedPresent := fieldValue(proposed, "spec.selector")
	if basePresent != proposedPresent || !reflect.DeepEqual(baseSelector, proposedSelector) {
		r.refuse("files", "%s: spec.selector may not change", filePath)
	}
}

// checkReplicas bounds the replica count and records its change.
func (r *Result) checkReplicas(filePath string, base, proposed map[string]any) {
	value, present := fieldValue(proposed, "spec.replicas")
	if !present {
		return
	}
	replicas, ok := asInt(value)
	if !ok {
		r.refuse("files", "%s: spec.replicas is not an integer", filePath)
		return
	}
	if replicas < 0 || replicas > MaxReplicas {
		r.refuse("files", "%s: spec.replicas is %d, outside 0 to %d", filePath, replicas, MaxReplicas)
		return
	}
	baseValue, basePresent := fieldValue(base, "spec.replicas")
	baseReplicas, _ := asInt(baseValue)
	if basePresent && baseReplicas == replicas {
		return
	}
	r.warn(v1alpha1.WarningReplicas, filePath+": spec.replicas",
		"the replica count changes from %d to %d", baseReplicas, replicas)
	if replicas == 0 {
		r.warn(v1alpha1.WarningReplicasZero, filePath+": spec.replicas",
			"the replica count is set to zero, which stops the Target")
	}
}

// resourceBounds are the permitted cpu and memory ranges of one resource.
var resourceBounds = map[string]struct {
	min, max resource.Quantity
}{
	"cpu":    {min: resource.MustParse("10m"), max: resource.MustParse("4")},
	"memory": {min: resource.MustParse("16Mi"), max: resource.MustParse("8Gi")},
}

// checkResources refuses the removal of a resource bounds block, bounds each
// cpu and memory value, and records a removed resource as a warning.
func (r *Result) checkResources(field string, baseContainer, container map[string]any) {
	baseResources := objectAt(baseContainer, "resources")
	resources := objectAt(container, "resources")
	if baseResources != nil && resources == nil {
		r.refuse("files", "%s: resources may not be removed", field)
		return
	}
	for _, list := range []string{"requests", "limits"} {
		baseList, _ := baseResources[list].(map[string]any)
		proposedList, _ := resources[list].(map[string]any)
		for name, raw := range proposedList {
			quantity, err := resource.ParseQuantity(fmt.Sprint(raw))
			if err != nil {
				r.refuse("files", "%s: resources.%s.%s is not a quantity", field, list, name)
				continue
			}
			bounds, known := resourceBounds[strings.ToLower(name)]
			if !known {
				if _, changed := baseList[name]; !changed {
					r.refuse("files", "%s: resources.%s.%s is not a supported resource", field, list, name)
				}
				continue
			}
			if quantity.Cmp(bounds.min) < 0 || quantity.Cmp(bounds.max) > 0 {
				r.refuse("files", "%s: resources.%s.%s is %s, outside %s to %s",
					field, list, name, quantity.String(), bounds.min.String(), bounds.max.String())
			}
		}
		for name := range baseList {
			if _, kept := proposedList[name]; !kept {
				r.warn(v1alpha1.WarningResourceRemoval, fmt.Sprintf("%s.resources.%s.%s", field, list, name),
					"the %s bound for %s is removed", list, name)
			}
		}
	}
	requests, _ := resources["requests"].(map[string]any)
	limits, _ := resources["limits"].(map[string]any)
	for name, raw := range requests {
		limit, present := limits[name]
		if !present {
			continue
		}
		request, err := resource.ParseQuantity(fmt.Sprint(raw))
		if err != nil {
			continue
		}
		ceiling, err := resource.ParseQuantity(fmt.Sprint(limit))
		if err != nil {
			continue
		}
		if request.Cmp(ceiling) > 0 {
			r.refuse("files", "%s: the %s request is above its limit", field, name)
		}
	}
}

// asInt reports the value as an int64.
func asInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case uint64:
		return int64(typed), true
	default:
		return 0, false
	}
}

// SortedWarnings returns the warnings in a stable order, for a Pull Request body.
func SortedWarnings(warnings []v1alpha1.ProposalWarning) []v1alpha1.ProposalWarning {
	out := append([]v1alpha1.ProposalWarning(nil), warnings...)
	slices.SortStableFunc(out, func(a, b v1alpha1.ProposalWarning) int {
		if a.Class != b.Class {
			if a.Class < b.Class {
				return -1
			}
			return 1
		}
		switch {
		case a.Field < b.Field:
			return -1
		case a.Field > b.Field:
			return 1
		default:
			return 0
		}
	})
	return out
}
