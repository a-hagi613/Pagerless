// Package charttest renders charts/pagerless and asserts the product's
// install contract: one read-only shell, no admission machinery, and an RBAC
// grant that fails the build when it widens.
package charttest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// releaseNamespace is the namespace every namespaced object is installed into.
const releaseNamespace = "pagerless"

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
		"PersistentVolumeClaim/pagerless-data",
		"Role/pagerless-approver",
		"Role/pagerless-server",
		"RoleBinding/pagerless-server",
		"Secret/pagerless-credentials",
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
		`group="" resources=[secrets] verbs=[get] names=[pagerless-admin-secret pagerless-credentials]`,
		`group="" resources=[secrets] verbs=[patch update] names=[pagerless-credentials]`,
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
	secret := find(t, documents, "Secret", "pagerless-credentials")
	data, ok := secret.Raw["stringData"].(map[string]any)
	if !ok {
		t.Fatalf("the credentials Secret has no stringData: %+v", secret.Raw)
	}
	want := []string{"AI_API_KEY", "AI_BASE_URL", "AI_MODEL", "GITHUB_API_URL", "GITHUB_REPO", "GITHUB_TOKEN"}
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
			if doc.Kind == "Secret" {
				t.Fatalf("the chart renders Secret %q, want none", doc.Metadata.Name)
			}
		}
		deployment := find(t, external, "Deployment", "pagerless-server")
		if !strings.Contains(renderedString(deployment.Raw), "team-credentials") {
			t.Error("the Deployment does not mount the externally managed Secret")
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
			if allowed != "pagerless-credentials" && allowed != "pagerless-admin-secret" {
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
