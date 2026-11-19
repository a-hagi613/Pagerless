// Package charttest renders charts/pagerless and asserts the product's
// install contract: one read-only shell, no admission machinery, and an RBAC
// grant that fails the build when it widens.
package charttest

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/connectors"
	"github.com/a-hagi613/pagerless/internal/controller"
)

// releaseNamespace is the namespace every namespaced object is installed into.
const releaseNamespace = "pagerless"

// credentialsSecretName is the Secret the chart pre-creates for credentials.
const credentialsSecretName = "pagerless-credentials"

// forbiddenNames lists the objects the standalone chart must never render.
var forbiddenNames = []string{
	"MutatingWebhookConfiguration",
	"ValidatingWebhookConfiguration",
	"ValidatingAdmissionPolicy",
	"ValidatingAdmissionPolicyBinding",
}

// document is one rendered Kubernetes object.
type document struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Raw map[string]any `json:"-"`
}

// render runs helm template for the chart and returns its objects.
func render(t *testing.T, sets ...string) []document {
	t.Helper()
	root := repoRoot(t)
	args := make([]string, 0, 6+2*len(sets))
	args = append(args, "template", "pagerless", filepath.Join(root, "charts", "pagerless"),
		"--namespace", releaseNamespace, "--include-crds")
	for _, set := range sets {
		args = append(args, "--set", set)
	}
	command := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}
	return decode(t, stdout.Bytes())
}

// renderError runs helm template and returns the error output, failing when the
// render succeeds.
func renderError(t *testing.T, sets ...string) string {
	t.Helper()
	root := repoRoot(t)
	args := make([]string, 0, 6+2*len(sets))
	args = append(args, "template", "pagerless", filepath.Join(root, "charts", "pagerless"),
		"--namespace", releaseNamespace, "--include-crds")
	for _, set := range sets {
		args = append(args, "--set", set)
	}
	command := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err == nil {
		t.Fatalf("helm template succeeded with %v, want a render error", sets)
	}
	return stderr.String()
}

// decode splits a helm rendering into documents.
func decode(t *testing.T, rendered []byte) []document {
	t.Helper()
	var documents []document
	for chunk := range strings.SplitSeq(string(rendered), "\n---") {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		var raw map[string]any
		if err := yaml.Unmarshal([]byte(chunk), &raw); err != nil {
			t.Fatalf("decode a rendered document: %v\n%s", err, chunk)
		}
		if len(raw) == 0 {
			continue
		}
		var doc document
		if err := yaml.Unmarshal([]byte(chunk), &doc); err != nil {
			t.Fatalf("decode a rendered document: %v\n%s", err, chunk)
		}
		doc.Raw = raw
		documents = append(documents, doc)
	}
	return documents
}

// repoRoot returns the repository root from the test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// find returns the first object of the kind and name.
func find(t *testing.T, documents []document, kind, name string) document {
	t.Helper()
	for _, doc := range documents {
		if doc.Kind == kind && doc.Metadata.Name == name {
			return doc
		}
	}
	t.Fatalf("the chart renders no %s %q", kind, name)
	return document{}
}

// inventory returns "Kind/name" for every rendered object, sorted.
func inventory(documents []document) []string {
	names := make([]string, 0, len(documents))
	for _, doc := range documents {
		names = append(names, doc.Kind+"/"+doc.Metadata.Name)
	}
	slices.Sort(names)
	return names
}

func TestTheDefaultRenderIsOneReadOnlyShell(t *testing.T) {
	t.Parallel()
	documents := render(t)

	want := []string{
		"ClusterRole/pagerless-pagerless-read",
		"ClusterRoleBinding/pagerless-pagerless-read",
		"ConfigMap/pagerless-config",
		"ConfigMap/pagerless-connectors",
		"CustomResourceDefinition/pages.pagerless.dev",
		"Deployment/pagerless-server",
		"NetworkPolicy/pagerless-agent-egress",
		"PersistentVolumeClaim/pagerless-data",
		"Role/pagerless-approver",
		"Role/pagerless-server",
		"RoleBinding/pagerless-server",
		"Secret/pagerless-admin-secret",
		"Secret/pagerless-credentials",
		"Secret/pagerless-session-key",
		"Service/pagerless-server",
		"ServiceAccount/pagerless-agent",
		"ServiceAccount/pagerless-server",
	}
	if got := inventory(documents); !slices.Equal(got, want) {
		t.Errorf("chart inventory = %v, want %v", inventory(documents), want)
	}
	for _, doc := range documents {
		if slices.Contains(forbiddenNames, doc.Kind) {
			t.Errorf("the chart renders %s %q, want no admission machinery", doc.Kind, doc.Metadata.Name)
		}
	}
	if deployments := countKind(documents, "Deployment"); deployments != 1 {
		t.Errorf("Deployments = %d, want 1", deployments)
	}
	if services := countKind(documents, "Service"); services != 1 {
		t.Errorf("Services = %d, want 1", services)
	}
	if claims := countKind(documents, "PersistentVolumeClaim"); claims != 1 {
		t.Errorf("PersistentVolumeClaims = %d, want 1", claims)
	}
	// Nothing is namespaced outside the release namespace.
	for _, doc := range documents {
		if doc.Metadata.Namespace != "" && doc.Metadata.Namespace != releaseNamespace {
			t.Errorf("%s %q is in namespace %q, want only %q",
				doc.Kind, doc.Metadata.Name, doc.Metadata.Namespace, releaseNamespace)
		}
	}
	// The only cluster-scoped objects are the CRD and the read role and binding.
	for _, doc := range documents {
		switch doc.Kind {
		case "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding":
		default:
			if doc.Metadata.Namespace == "" {
				t.Errorf("%s %q is cluster-scoped, want it in %q", doc.Kind, doc.Metadata.Name, releaseNamespace)
			}
		}
	}
}

// countKind counts the rendered objects of a kind.
func countKind(documents []document, kind string) int {
	count := 0
	for _, doc := range documents {
		if doc.Kind == kind {
			count++
		}
	}
	return count
}

func TestTheClusterReadRoleIsExactlyTheCheckedInAllowlist(t *testing.T) {
	t.Parallel()
	documents := render(t)
	role := find(t, documents, "ClusterRole", "pagerless-pagerless-read")
	got := ruleSet(t, role)
	want := []string{
		`group="" resources=[] verbs=[get] names=[] urls=[/openapi/v3 /openapi/v3/*]`,
		`group="" resources=[configmaps endpoints persistentvolumeclaims pods services] verbs=[get list watch] names=[]`,
		`group="" resources=[events] verbs=[get list watch] names=[]`,
		`group="" resources=[namespaces nodes] verbs=[get list watch] names=[]`,
		`group="" resources=[pods/log] verbs=[get] names=[]`,
		`group="apps" resources=[daemonsets deployments replicasets statefulsets] verbs=[get list watch] names=[]`,
		`group="authentication.k8s.io" resources=[tokenreviews] verbs=[create] names=[]`,
		`group="batch" resources=[cronjobs jobs] verbs=[get list watch] names=[]`,
		`group="discovery.k8s.io" resources=[endpointslices] verbs=[get list watch] names=[]`,
		`group="events.k8s.io" resources=[events] verbs=[get list watch] names=[]`,
		`group="networking.k8s.io" resources=[ingresses] verbs=[get list watch] names=[]`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("ClusterRole rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheReleaseRoleMatchesTheCheckedInAllowlist(t *testing.T) {
	t.Parallel()
	documents := render(t)
	role := find(t, documents, "Role", "pagerless-server")
	got := ruleSet(t, role)
	want := []string{
		`group="" resources=[pods] verbs=[get list watch] names=[]`,
		`group="" resources=[secrets] verbs=[get patch update] names=[pagerless-credentials]`,
		`group="batch" resources=[jobs] verbs=[create delete get list watch] names=[]`,
		`group="pagerless.dev" resources=[pages/finalizers] verbs=[update] names=[]`,
		`group="pagerless.dev" resources=[pages/status] verbs=[get patch update] names=[]`,
		`group="pagerless.dev" resources=[pages] verbs=[create delete get list patch watch] names=[]`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("Role rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	approver := ruleSet(t, find(t, documents, "Role", "pagerless-approver"))
	wantApprover := []string{
		`group="pagerless.dev" resources=[pages] verbs=[get list watch] names=[]`,
		`group="pagerless.dev" resources=[pages] verbs=[patch] names=[]`,
	}
	if !slices.Equal(approver, wantApprover) {
		t.Errorf("approver rules:\n%s\nwant:\n%s", strings.Join(approver, "\n"), strings.Join(wantApprover, "\n"))
	}
}

// rule is one parsed RBAC rule.
type rule struct {
	group     string
	resources []string
	verbs     []string
	names     []string
	urls      []string
}

// String renders a rule in a stable, comparable form.
func (r rule) String() string {
	text := "group=\"" + r.group + "\" resources=[" + strings.Join(r.resources, " ") + "] verbs=["
	text += strings.Join(r.verbs, " ") + "] names=[" + strings.Join(r.names, " ") + "]"
	if len(r.urls) > 0 {
		text += " urls=[" + strings.Join(r.urls, " ") + "]"
	}
	return text
}

// ruleSet parses and sorts a rules list.
func ruleSet(t *testing.T, doc document) []string {
	t.Helper()
	rules := parseRules(t, doc)
	rendered := make([]string, 0, len(rules))
	for _, r := range rules {
		rendered = append(rendered, r.String())
	}
	return rendered
}

// parseRules parses a rules list.
func parseRules(t *testing.T, doc document) []rule {
	t.Helper()
	raw, ok := doc.Raw["rules"].([]any)
	if !ok {
		t.Fatalf("%s %q has no rules", doc.Kind, doc.Metadata.Name)
	}
	rules := make([]rule, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("a rule of %s %q is not an object", doc.Kind, doc.Metadata.Name)
		}
		parsed := rule{
			group:     strings.Join(stringsOf(object["apiGroups"]), " "),
			resources: sorted(stringsOf(object["resources"])),
			verbs:     sorted(stringsOf(object["verbs"])),
			names:     sorted(stringsOf(object["resourceNames"])),
			urls:      sorted(stringsOf(object["nonResourceURLs"])),
		}
		rules = append(rules, parsed)
	}
	slices.SortFunc(rules, func(a, b rule) int { return strings.Compare(a.String(), b.String()) })
	return rules
}

// sorted returns a sorted copy.
func sorted(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

// stringsOf reads a rule's string list.
func stringsOf(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestAllowlistModeRendersPerNamespaceReadRolesWithoutClusterRules(t *testing.T) {
	t.Parallel()
	documents := render(t, "rbac.watchAllNamespaces=false", "rbac.watchNamespaces={shop,payments}")

	if clusters := countKind(documents, "ClusterRole") + countKind(documents, "ClusterRoleBinding"); clusters != 0 {
		t.Errorf("allowlist mode renders %d cluster-scoped RBAC objects, want none", clusters)
	}
	namespaced := []string{"shop", "payments"}
	for _, namespace := range namespaced {
		role := findIn(t, documents, "Role", "pagerless-pagerless-read", namespace)
		got := ruleSet(t, role)
		want := []string{
			`group="" resources=[configmaps endpoints persistentvolumeclaims pods services] verbs=[get list watch] names=[]`,
			`group="" resources=[events] verbs=[get list watch] names=[]`,
			`group="" resources=[pods/log] verbs=[get] names=[]`,
			`group="apps" resources=[daemonsets deployments replicasets statefulsets] verbs=[get list watch] names=[]`,
			`group="batch" resources=[cronjobs jobs] verbs=[get list watch] names=[]`,
			`group="discovery.k8s.io" resources=[endpointslices] verbs=[get list watch] names=[]`,
			`group="events.k8s.io" resources=[events] verbs=[get list watch] names=[]`,
			`group="networking.k8s.io" resources=[ingresses] verbs=[get list watch] names=[]`,
		}
		if !slices.Equal(got, want) {
			t.Errorf("Role in %s:\n%s\nwant:\n%s", namespace, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		findIn(t, documents, "RoleBinding", "pagerless-pagerless-read", namespace)
	}
}

// findIn returns the object of the kind, name, and namespace.
func findIn(t *testing.T, documents []document, kind, name, namespace string) document {
	t.Helper()
	for _, doc := range documents {
		if doc.Kind == kind && doc.Metadata.Name == name && doc.Metadata.Namespace == namespace {
			return doc
		}
	}
	t.Fatalf("the chart renders no %s %q in %q", kind, name, namespace)
	return document{}
}

func TestTheWatchModesAreMutuallyExclusiveAndValidated(t *testing.T) {
	t.Parallel()
	t.Run("both modes set", func(t *testing.T) {
		t.Parallel()
		stderr := renderError(t, "rbac.watchNamespaces={shop}")
		if !strings.Contains(stderr, "mutually exclusive") {
			t.Errorf("error = %q, want it to explain the mutual exclusion", stderr)
		}
	})
	t.Run("an empty allowlist", func(t *testing.T) {
		t.Parallel()
		stderr := renderError(t, "rbac.watchAllNamespaces=false")
		if !strings.Contains(stderr, "at least one namespace") {
			t.Errorf("error = %q, want it to require a list entry", stderr)
		}
	})
}

func TestTheCredentialsSecretIsPrecreatedEmptyOrOmitted(t *testing.T) {
	t.Parallel()
	documents := render(t)
	secret := find(t, documents, "Secret", credentialsSecretName)
	data, ok := secret.Raw["stringData"].(map[string]any)
	if !ok {
		t.Fatalf("the credentials Secret has no stringData: %+v", secret.Raw)
	}
	want := []string{
		"AI_API_KEY", "AI_BASE_URL", "AI_MODEL", "ALERTMANAGER_WEBHOOK_TOKEN", "DATADOG_WEBHOOK_TOKEN",
		"GENERIC_WEBHOOK_TOKEN", "GITHUB_API_URL", "GITHUB_REPO", "GITHUB_TOKEN", "GRAFANA_WEBHOOK_SECRET",
	}
	keys := make([]string, 0, len(data))
	for key, value := range data {
		keys = append(keys, key)
		if value != "" {
			t.Errorf("key %q = %v, want an empty value", key, value)
		}
	}
	slices.Sort(keys)
	if !slices.Equal(keys, want) {
		t.Errorf("credentials keys = %v, want %v", keys, want)
	}

	t.Run("an externally managed Secret is mounted and not rendered", func(t *testing.T) {
		t.Parallel()
		external := render(t, "credentials.existingSecret=team-credentials")
		for _, doc := range external {
			if doc.Kind == "Secret" && (doc.Metadata.Name == credentialsSecretName || doc.Metadata.Name == "team-credentials") {
				t.Fatalf("the chart renders credentials Secret %q, want none when existingSecret is set", doc.Metadata.Name)
			}
		}
		deployment := find(t, external, "Deployment", "pagerless-server")
		if !strings.Contains(renderedString(deployment.Raw), "team-credentials") {
			t.Error("the Deployment does not mount the externally managed Secret")
		}
		role := find(t, external, "Role", "pagerless-server")
		for _, parsed := range parseRules(t, role) {
			if slices.Contains(parsed.resources, "secrets") {
				t.Errorf("Role pagerless-server still grants Secrets when existingSecret is set: %s", parsed)
			}
		}
	})
}

// renderedString serialises an object for a substring assertion.
func renderedString(raw map[string]any) string {
	encoded, err := yaml.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func TestNoForbiddenGrantIsRendered(t *testing.T) {
	t.Parallel()
	modes := map[string][]string{
		"cluster-wide": nil,
		"allowlist":    {"rbac.watchAllNamespaces=false", "rbac.watchNamespaces={shop}"},
	}
	for name, sets := range modes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, doc := range render(t, sets...) {
				if doc.Kind != "ClusterRole" && doc.Kind != "Role" {
					continue
				}
				for _, parsed := range parseRules(t, doc) {
					assertNoWidening(t, doc, parsed)
				}
			}
		})
	}
}

// assertNoWidening fails when one rendered rule grants something the product
// must never hold.
func assertNoWidening(t *testing.T, doc document, r rule) {
	t.Helper()
	for _, forbidden := range []string{
		"pods/exec", "pods/attach", "pods/portforward", "pods/ephemeralcontainers",
		"serviceaccounts/token", "nodes/proxy",
	} {
		if slices.Contains(r.resources, forbidden) {
			t.Errorf("%s %q grants %s: %s", doc.Kind, doc.Metadata.Name, forbidden, r)
		}
	}
	for _, resource := range r.resources {
		if strings.HasSuffix(resource, "/scale") {
			t.Errorf("%s %q grants %s: %s", doc.Kind, doc.Metadata.Name, resource, r)
		}
	}
	// The release-namespace Role creates and deletes the agent Jobs, which are the
	// investigations themselves, so only cluster-scoped and per-namespace read
	// roles are subject to the workload-write prohibition.
	workload := (r.group == "apps" || r.group == "batch" || r.group == "events.k8s.io" ||
		(r.group == "" && slices.Contains(r.resources, "events")) ||
		(r.group == "" && slices.Contains(r.resources, "nodes"))) &&
		(doc.Kind != "Role" || doc.Metadata.Name != "pagerless-server")
	for _, verb := range r.verbs {
		if workload && verb != "get" && verb != "list" && verb != "watch" {
			t.Errorf("%s %q grants %s on %s: %s", doc.Kind, doc.Metadata.Name, verb, r.resources, r)
		}
		switch verb {
		case "impersonate", "escalate", "bind":
			t.Errorf("%s %q grants %s: %s", doc.Kind, doc.Metadata.Name, verb, r)
		}
	}
	if slices.Contains(r.resources, "secrets") {
		if len(r.names) == 0 {
			t.Errorf("%s %q grants Secrets without resourceNames: %s", doc.Kind, doc.Metadata.Name, r)
		}
		for _, allowed := range r.names {
			if allowed != credentialsSecretName {
				t.Errorf("%s %q grants Secret %q: %s", doc.Kind, doc.Metadata.Name, allowed, r)
			}
		}
		for _, verb := range r.verbs {
			if verb != "get" && verb != "update" && verb != "patch" {
				t.Errorf("%s %q grants %s on Secrets: %s", doc.Kind, doc.Metadata.Name, verb, r)
			}
		}
	}
}

func TestThePageCRDIsServedWithTheV2Contract(t *testing.T) {
	t.Parallel()
	path := filepath.Join(repoRoot(t), "charts", "pagerless", "crds", "pagerless.dev_pages.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the Page CRD: %v", err)
	}
	for _, removed := range []string{"pagerless.dev_triages.yaml", "pagerless.dev_triageapprovals.yaml"} {
		if _, err := os.Stat(filepath.Join(repoRoot(t), "charts", "pagerless", "crds", removed)); err == nil {
			t.Errorf("charts/pagerless/crds still holds %s", removed)
		}
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("decode the Page CRD: %v", err)
	}
	metadata := crd["metadata"].(map[string]any)
	if metadata["name"] != "pages.pagerless.dev" {
		t.Errorf("CRD name = %v, want pages.pagerless.dev", metadata["name"])
	}
	spec := crd["spec"].(map[string]any)
	if spec["scope"] != "Namespaced" {
		t.Errorf("scope = %v, want Namespaced", spec["scope"])
	}
	names := spec["names"].(map[string]any)
	if names["plural"] != "pages" || names["singular"] != "page" || names["kind"] != "Page" {
		t.Errorf("names = %v, want Page/pages/page", names)
	}
	if !slices.Contains(stringsOf(names["shortNames"]), "pg") {
		t.Errorf("shortNames = %v, want pg", names["shortNames"])
	}
	if !slices.Contains(stringsOf(names["categories"]), "pagerless") {
		t.Errorf("categories = %v, want pagerless", names["categories"])
	}
	versions := spec["versions"].([]any)
	version := versions[0].(map[string]any)
	if version["name"] != "v1alpha1" {
		t.Errorf("version = %v, want v1alpha1", version["name"])
	}
	if version["subresources"] == nil {
		t.Error("the Page CRD has no status subresource")
	}
	columns := version["additionalPrinterColumns"].([]any)
	columnNames := make([]string, 0, len(columns))
	for _, column := range columns {
		columnNames = append(columnNames, column.(map[string]any)["name"].(string))
	}
	if want := []string{"Target", "Phase", "Trigger", "PR", "Age", "Kind", "Namespace"}; !slices.Equal(columnNames, want) {
		t.Errorf("printer columns = %v, want %v", columnNames, want)
	}
	schema := version["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	specSchema := properties["spec"].(map[string]any)
	targetSchema := specSchema["properties"].(map[string]any)["target"].(map[string]any)
	if targetSchema["x-kubernetes-validations"] == nil {
		t.Error("the Target reference has no CEL rule for the namespace")
	}
	kindEnum := targetSchema["properties"].(map[string]any)["kind"].(map[string]any)["enum"].([]any)
	wantKinds := []string{
		"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod", "PersistentVolumeClaim", "Node",
	}
	gotKinds := make([]string, 0, len(kindEnum))
	for _, kind := range kindEnum {
		gotKinds = append(gotKinds, kind.(string))
	}
	if !slices.Equal(gotKinds, wantKinds) {
		t.Errorf("TargetKind enum = %v, want %v", gotKinds, wantKinds)
	}
	statusPhase := properties["status"].(map[string]any)["properties"].(map[string]any)["phase"].(map[string]any)
	phaseEnum := stringsOf(statusPhase["enum"])
	for _, phase := range []string{"PullRequestOpen", "Superseded"} {
		if !slices.Contains(phaseEnum, phase) {
			t.Errorf("Phase enum lacks %s: %v", phase, phaseEnum)
		}
	}
	for _, retired := range []string{"Executing"} {
		if slices.Contains(phaseEnum, retired) {
			t.Errorf("Phase enum still holds the retired phase %s", retired)
		}
	}
}

func TestValuesSchemaEnforcesAdditionalPropertiesFalseAndRejectsSecrets(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	schemaPath := filepath.Join(root, "charts", "pagerless", "values.schema.json")
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read values.schema.json: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse values.schema.json: %v", err)
	}

	var objectCount int
	var checkObjects func(path string, node any)
	checkObjects = func(path string, node any) {
		switch typed := node.(type) {
		case map[string]any:
			if typed["type"] == "object" {
				objectCount++
				// A boolean true, or an absent value, admits unknown fields. An object
				// schema is a typed free-form map whose entries are checked below; the
				// Connector query maps are the only such map.
				if ap, ok := typed["additionalProperties"].(bool); !ok || ap {
					if _, typedMap := typed["additionalProperties"].(map[string]any); !typedMap {
						t.Errorf("schema object at %s has additionalProperties=%v, want false", path, typed["additionalProperties"])
					}
				}
			}
			for key, child := range typed {
				checkObjects(path+"."+key, child)
			}
		case []any:
			for i, child := range typed {
				checkObjects(path+"["+string(rune('0'+i))+"]", child)
			}
		}
	}
	checkObjects("$", schema)
	if objectCount == 0 {
		t.Fatal("values.schema.json defines no objects")
	}

	for _, forbiddenSet := range []string{
		"auth.adminPassword=hunter2",
		"credentials.apiKey=sk-secret",
		"credentials.aiApiKey=sk-secret",
		"agent.apiKey=sk-secret",
		"github.token=ghp_secret",
		"unknownTopLevelKey=true",
	} {
		stderr := renderError(t, forbiddenSet)
		if !strings.Contains(stderr, "additionalProperties") &&
			!strings.Contains(stderr, "values don't meet the specifications") {
			t.Errorf("helm template --set %s error = %q, want schema rejection", forbiddenSet, stderr)
		}
	}
}

func TestValuesYAMLLeavesContainNoSecretBearingFields(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "charts", "pagerless", "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("decode values.yaml: %v", err)
	}

	secretPattern := regexp.MustCompile(`(?i)(credential|apikey|api_key|token|password|secret|key)s?$`)
	allowedLeaves := map[string]bool{
		"images.pullSecrets":                          true,
		"serviceAccount.automountServiceAccountToken": true,
		"credentials.existingSecret":                  true,
		"credentials.secretName":                      true,
		"portal.writeCredentials":                     true,
		"auth.adminSecret.create":                     true,
		"auth.adminSecret.name":                       true,
		"auth.adminSecret.key":                        true,
		"auth.adminSecret.length":                     true,
		"auth.adminSecret.keep":                       true,
		"auth.session.secretName":                     true,
		"auth.session.key":                            true,
		"auth.bearerToken":                            true,
	}

	var leafCount int
	var walk func(prefix string, node any)
	walk = func(prefix string, node any) {
		if m, ok := node.(map[string]any); ok && len(m) > 0 {
			for k, v := range m {
				next := k
				if prefix != "" {
					next = prefix + "." + k
				}
				walk(next, v)
			}
			return
		}
		leafCount++
		if secretPattern.MatchString(prefix) && !allowedLeaves[prefix] {
			t.Errorf("values.yaml leaf %q matches secret pattern and is not an allowed reference/boolean", prefix)
		}
	}
	walk("", values)
	if leafCount == 0 {
		t.Fatal("enumerated 0 leaves in values.yaml")
	}
}

func TestAuthModeAndSecretsContract(t *testing.T) {
	t.Parallel()
	docs := render(t)
	adminSecret := find(t, docs, "Secret", "pagerless-admin-secret")
	annotations := adminSecret.Raw["metadata"].(map[string]any)["annotations"].(map[string]any)
	if got := annotations["helm.sh/resource-policy"]; got != "keep" {
		t.Errorf("pagerless-admin-secret resource-policy = %v, want keep", got)
	}
	tokenVal, _ := adminSecret.Raw["stringData"].(map[string]any)["token"].(string)
	if len(tokenVal) != 48 {
		t.Errorf("generated bootstrap token length = %d, want 48", len(tokenVal))
	}

	sessionSecret := find(t, docs, "Secret", "pagerless-session-key")
	annotations = sessionSecret.Raw["metadata"].(map[string]any)["annotations"].(map[string]any)
	if got := annotations["helm.sh/resource-policy"]; got != "keep" {
		t.Errorf("pagerless-session-key resource-policy = %v, want keep", got)
	}
	keyVal, _ := sessionSecret.Raw["stringData"].(map[string]any)["key"].(string)
	if len(keyVal) < 32 {
		t.Errorf("generated session key length = %d, want >= 32", len(keyVal))
	}

	t.Run("disabled mode renders no auth secrets", func(t *testing.T) {
		t.Parallel()
		disabled := render(t, "auth.mode=disabled")
		for _, doc := range disabled {
			if doc.Kind == "Secret" && doc.Metadata.Name != "pagerless-credentials" {
				t.Errorf("disabled mode rendered Secret %q", doc.Metadata.Name)
			}
		}
	})

	t.Run("disabled mode with ingress and no identityHeader fails render", func(t *testing.T) {
		t.Parallel()
		stderr := renderError(t, "auth.mode=disabled", "ingress.enabled=true")
		if !strings.Contains(stderr, "identityHeader") {
			t.Errorf("error = %q, want it to require auth.proxy.identityHeader", stderr)
		}
	})

	t.Run("disabled mode with ingress and identityHeader renders Ingress", func(t *testing.T) {
		t.Parallel()
		proxied := render(t, "auth.mode=disabled", "ingress.enabled=true", "auth.proxy.identityHeader=X-Auth-Request-User")
		find(t, proxied, "Ingress", "pagerless-server")
	})

	t.Run("token mode with create=false and empty name fails render", func(t *testing.T) {
		t.Parallel()
		stderr := renderError(t, "auth.adminSecret.create=false", "auth.adminSecret.name=")
		if !strings.Contains(stderr, "adminSecret") {
			t.Errorf("error = %q, want it to mention auth.adminSecret", stderr)
		}
	})
}

func TestNotesTxtCoversTokenDisabledAndExistingSecretModes(t *testing.T) {
	t.Parallel()
	renderNotes := func(t *testing.T, sets ...string) string {
		t.Helper()
		root := repoRoot(t)
		// Render NOTES.txt via helm install --dry-run=client so .Release.Namespace and notes are printed.
		installArgs := make([]string, 0, 6+2*len(sets))
		installArgs = append(installArgs,
			"install", "pagerless", filepath.Join(root, "charts", "pagerless"),
			"--namespace", releaseNamespace, "--dry-run=client")
		for _, set := range sets {
			installArgs = append(installArgs, "--set", set)
		}
		cmd := exec.Command("helm", installArgs...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("helm install --dry-run=client: %v: %s", err, stderr.String())
		}
		_, notes, ok := strings.Cut(stdout.String(), "NOTES:\n")
		if !ok {
			t.Fatalf("helm dry-run output contained no NOTES section:\n%s", stdout.String())
		}
		return notes
	}

	t.Run("default token mode", func(t *testing.T) {
		t.Parallel()
		notes := renderNotes(t)
		for _, want := range []string{
			"kubectl -n pagerless port-forward svc/pagerless-server 8080:8080",
			"kubectl -n pagerless get secret pagerless-admin-secret",
			"base64decode",
			"kubectl -n pagerless delete secret pagerless-admin-secret",
			"/settings",
		} {
			if !strings.Contains(notes, want) {
				t.Errorf("default NOTES.txt lacks %q:\n%s", want, notes)
			}
		}
	})

	t.Run("disabled mode", func(t *testing.T) {
		t.Parallel()
		notes := renderNotes(t, "auth.mode=disabled")
		if !strings.Contains(notes, "WARNING") || !strings.Contains(notes, "disabled") {
			t.Errorf("disabled NOTES.txt lacks warning block:\n%s", notes)
		}
	})

	t.Run("existingSecret mode", func(t *testing.T) {
		t.Parallel()
		notes := renderNotes(t, "credentials.existingSecret=team-credentials")
		for _, want := range []string{
			"team-credentials",
			"read-only",
			"AI_BASE_URL",
			"AI_API_KEY",
			"AI_MODEL",
			"GITHUB_TOKEN",
			"GITHUB_REPO",
			"CONNECTOR_<NAME>_TOKEN",
		} {
			if !strings.Contains(notes, want) {
				t.Errorf("existingSecret NOTES.txt lacks %q:\n%s", want, notes)
			}
		}
	})
}

func TestRepoWideOnlyOneScopedRoleGrantsSecrets(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	var hits []string
	err := filepath.WalkDir(filepath.Join(root, "charts"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".tpl") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "resources: [secrets]") || strings.Contains(string(raw), `resources: ["secrets"]`) {
			rel, _ := filepath.Rel(root, path)
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantHits := []string{filepath.Join("charts", "pagerless", "templates", "rbac.yaml")}
	if !slices.Equal(hits, wantHits) {
		t.Errorf("files granting secrets RBAC = %v, want only %v", hits, wantHits)
	}
}

func TestNativeLogCollectionIsMandatory(t *testing.T) {
	t.Parallel()
	stderr := renderError(t, "connectors.logs.nativeKubernetes.enabled=false")
	if !strings.Contains(stderr, "native Kubernetes logs are the only evidence source") {
		t.Errorf("error = %q, want it to explain why native logs cannot be disabled", stderr)
	}

	documents := render(t)
	connectorsConfigMap := find(t, documents, "ConfigMap", "pagerless-connectors")
	data, ok := connectorsConfigMap.Raw["data"].(map[string]any)
	if !ok {
		t.Fatalf("the connectors ConfigMap has no data: %+v", connectorsConfigMap.Raw)
	}
	rendered, ok := data["connectors.yaml"].(string)
	if !ok {
		t.Fatalf("the connectors ConfigMap has no connectors.yaml: %+v", data)
	}
	for _, want := range []string{"nativeKubernetes:", "enabled: true", "maxAlerts: 50"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("connectors.yaml lacks %q:\n%s", want, rendered)
		}
	}
	if _, err := connectors.ParseConfig([]byte(rendered)); err != nil {
		t.Errorf("the rendered connectors.yaml does not parse: %v\n%s", err, rendered)
	}
	deployment := find(t, documents, "Deployment", "pagerless-server")
	server := renderedString(deployment.Raw)
	for _, want := range []string{
		"--connectors-config=/etc/pagerless/connectors/connectors.yaml",
		"--webhook-alertmanager-token-file=/var/run/pagerless/credentials/ALERTMANAGER_WEBHOOK_TOKEN",
		"name: connectors",
	} {
		if !strings.Contains(server, want) {
			t.Errorf("the Deployment lacks %q", want)
		}
	}
}

// TestTheAgentEgressPolicyHasTwoDestinationsAndExcludesLinkLocal renders the
// agent Job's egress policy: the server (with cluster DNS) and the model
// endpoint, and nothing else.
func TestTheAgentEgressPolicyHasTwoDestinationsAndExcludesLinkLocal(t *testing.T) {
	t.Parallel()
	documents := render(t)
	policy := find(t, documents, "NetworkPolicy", "pagerless-agent-egress")

	spec, ok := policy.Raw["spec"].(map[string]any)
	if !ok {
		t.Fatal("the policy has no spec")
	}
	selector, _ := spec["podSelector"].(map[string]any)
	labels, _ := selector["matchLabels"].(map[string]any)
	if labels["app.kubernetes.io/name"] != "pagerless-agent" {
		t.Errorf("pod selector = %v, want the agent Job pods", selector)
	}
	types, _ := spec["policyTypes"].([]any)
	if len(types) != 1 || types[0] != "Egress" {
		t.Errorf("policy types = %v, want Egress only", types)
	}
	rules, _ := spec["egress"].([]any)
	if len(rules) != 2 {
		t.Fatalf("egress rules = %d, want exactly two destinations", len(rules))
	}
	server, _ := rules[0].(map[string]any)
	serverPeers, _ := server["to"].([]any)
	if len(serverPeers) != 2 {
		t.Errorf("the first rule has %d peers, want the server and cluster DNS", len(serverPeers))
	}
	model, _ := rules[1].(map[string]any)
	modelPeers, _ := model["to"].([]any)
	if len(modelPeers) != 2 {
		t.Fatalf("the model rule has %d peers, want the IPv4 and IPv6 default routes", len(modelPeers))
	}
	var ipv4, ipv6 bool
	for _, peer := range modelPeers {
		entry, _ := peer.(map[string]any)
		block, _ := entry["ipBlock"].(map[string]any)
		cidr, _ := block["cidr"].(string)
		except, _ := block["except"].([]any)
		switch cidr {
		case "0.0.0.0/0":
			ipv4 = true
			if !containsValue(except, "169.254.0.0/16") {
				t.Errorf("the IPv4 model route does not exclude link-local: %v", except)
			}
		case "::/0":
			ipv6 = true
			if !containsValue(except, "fe80::/10") {
				t.Errorf("the IPv6 model route does not exclude link-local: %v", except)
			}
		default:
			t.Errorf("unexpected model destination %q", cidr)
		}
	}
	if !ipv4 || !ipv6 {
		t.Errorf("model routes = %v, want both the IPv4 and IPv6 default routes", modelPeers)
	}
	// Nothing in the policy allows the metadata service or another namespace.
	for _, rule := range rules {
		entry, _ := rule.(map[string]any)
		encoded, err := yaml.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "169.254.169.254") {
			t.Errorf("the policy names the metadata address directly: %s", encoded)
		}
	}
}

// containsValue reports whether a rendered list holds a string.
func containsValue(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestTheRenderedAgentJobMountsOnlyTheReportTokenAndModelKey pins the agent
// Job's credential contract next to the chart that constrains its egress. The
// Job itself is rendered by the server for each Page, so this test builds it
// with the production builder rather than a Helm template.
func TestTheRenderedAgentJobMountsOnlyTheReportTokenAndModelKey(t *testing.T) {
	t.Parallel()
	page := &v1alpha1.Page{}
	page.Name, page.Namespace = "checkout-oom", releaseNamespace
	page.UID = "0f3a1b2c-1111-4222-8333-444455556666"
	job := controller.AgentJob(page, controller.AgentJobConfig{
		Image: "agent:test", ServiceAccount: "pagerless-agent", ServerURL: "http://server", Deadline: 420,
		Runtime: controller.AgentRuntimeConfig{
			ModelBaseURL: "https://model.test/v1", ModelName: "model",
			ModelKeySecret: controller.DefaultCredentialsSecret, ModelKeyKey: controller.ModelAPIKeyName,
		},
	})
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the agent Job must not mount the API-server-audience token")
	}
	projected := map[string]string{}
	secrets := map[string]string{}
	var others []string
	for _, volume := range pod.Volumes {
		switch {
		case volume.Projected != nil:
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken != nil {
					projected[volume.Name] = source.ServiceAccountToken.Audience
				}
			}
		case volume.Secret != nil:
			secrets[volume.Name] = volume.Secret.SecretName
		default:
			others = append(others, volume.Name)
		}
	}
	want := agentapi.AudienceForPage(string(page.UID))
	if len(projected) != 1 || projected["report-token"] != want {
		t.Errorf("projected tokens = %v, want only report-token with audience %q", projected, want)
	}
	if len(secrets) != 1 || secrets["model-key"] != controller.DefaultCredentialsSecret {
		t.Errorf("Secret volumes = %v, want only model-key", secrets)
	}
	if len(others) != 1 || others[0] != "temporary" {
		t.Errorf("other volumes = %v, want only the temporary emptyDir", others)
	}
	for _, variable := range pod.Containers[0].Env {
		if variable.ValueFrom != nil {
			t.Errorf("%s is sourced from a Secret or field, want mounted files only", variable.Name)
		}
		for _, forbidden := range []string{"GITHUB", "CONNECTOR", "KUBECONFIG"} {
			if strings.Contains(variable.Name, forbidden) {
				t.Errorf("the agent Job receives %s", variable.Name)
			}
		}
	}
}
