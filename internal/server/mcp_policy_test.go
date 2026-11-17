package server_test

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"

	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const mcpDeployDir = "../../deploy/mcp"

const (
	mcpReleaseNamespace = "pagerless"
	mcpServiceAccount   = "mcp-kubernetes"
	mcpRoleName         = "pagerless-mcp-read"
	appsAPIGroup        = "apps"
	deploymentsResource = "deployments"
)

// The only resources the Kubernetes MCP service may read, and the only verbs.
// StressChaos is the single custom resource, so that the Context Bundle can
// name an active experiment that targets the Workload.
var (
	readableResources = []string{
		"pods",
		"events",
		"pods/log",
		deploymentsResource,
		"statefulsets",
		"replicasets",
		"stresschaos",
	}
	readVerbs = []string{"get", "list", "watch"}
)

type objectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type renderedObject struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   objectMeta          `json:"metadata"`
	Data       map[string]string   `json:"data"`
	Rules      []rbacv1.PolicyRule `json:"rules"`
	RoleRef    rbacv1.RoleRef      `json:"roleRef"`
	Subjects   []rbacv1.Subject    `json:"subjects"`
}

// renderMCPTemplate renders one Helm template of the MCP chart with the chart's
// real values, so that a test reads the manifests Argo CD would apply.
func renderMCPTemplate(t *testing.T, name string) []renderedObject {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(mcpDeployDir, "templates", name))
	if err != nil {
		t.Fatal(err)
	}
	valuesRaw, err := os.ReadFile(filepath.Join(mcpDeployDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(valuesRaw, &values); err != nil {
		t.Fatal(err)
	}
	namespaces, ok := values["allowedNamespaces"].([]any)
	if !ok || len(namespaces) == 0 {
		t.Fatalf("values.yaml has no allowedNamespaces")
	}
	data := map[string]any{
		"Values":  values,
		"Release": map[string]any{"Namespace": mcpReleaseNamespace},
	}
	funcs := template.FuncMap{"quote": strconv.Quote}
	tmpl, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}

	var objects []renderedObject
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(rendered.Bytes())))
	for {
		document, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(bytes.TrimSpace(document)) == 0 {
			continue
		}
		var object renderedObject
		if err := yaml.UnmarshalStrict(document, &object); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		objects = append(objects, object)
	}
	if len(objects) == 0 {
		t.Fatalf("%s rendered no objects", name)
	}
	return objects
}

func TestTheMCPRoleGrantsOnlyReadsAndOnlyStressChaosAsACustomResource(t *testing.T) {
	t.Parallel()
	valuesRaw, err := os.ReadFile(filepath.Join(mcpDeployDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		AllowedNamespaces []string `json:"allowedNamespaces"`
	}
	if err := yaml.Unmarshal(valuesRaw, &values); err != nil {
		t.Fatal(err)
	}

	var roles, bindings int
	for _, object := range renderMCPTemplate(t, "rbac.yaml") {
		switch object.Kind {
		case "Role":
			roles++
			if !slices.Contains(values.AllowedNamespaces, object.Metadata.Namespace) {
				t.Errorf("a Role exists in %q, which is not allowlisted", object.Metadata.Namespace)
			}
			stressChaos := 0
			for _, rule := range object.Rules {
				for _, verb := range rule.Verbs {
					if !slices.Contains(readVerbs, verb) {
						t.Errorf("the MCP Role grants %q, which is not a read verb", verb)
					}
				}
				for _, resource := range rule.Resources {
					if !slices.Contains(readableResources, resource) {
						t.Errorf("the MCP Role grants %q, which is not an allowlisted read", resource)
					}
				}
				for _, group := range rule.APIGroups {
					if group == "chaos-mesh.org" {
						stressChaos++
						if !slices.Equal(rule.Resources, []string{"stresschaos"}) {
							t.Errorf("the Chaos Mesh rule grants %v, want only [stresschaos]", rule.Resources)
						}
						if !slices.Equal(rule.Verbs, readVerbs) {
							t.Errorf("the Chaos Mesh rule grants %v, want %v", rule.Verbs, readVerbs)
						}
						continue
					}
					if group != "" && group != appsAPIGroup && group != "events.k8s.io" {
						t.Errorf("the MCP Role grants the API group %q", group)
					}
				}
				if slices.Contains(rule.Resources, "secrets") {
					t.Errorf("the MCP Role grants Secrets")
				}
			}
			if stressChaos != 1 {
				t.Errorf("the Role in %q has %d Chaos Mesh rules, want exactly one", object.Metadata.Namespace, stressChaos)
			}
		case "RoleBinding":
			bindings++
			if object.RoleRef.Name != mcpRoleName {
				t.Errorf("a RoleBinding refers to %q", object.RoleRef.Name)
			}
			for _, subject := range object.Subjects {
				if subject.Kind != "ServiceAccount" || subject.Name != mcpServiceAccount ||
					subject.Namespace != mcpReleaseNamespace {
					t.Errorf("the MCP Role is bound to %+v, want the mcp-kubernetes service account", subject)
				}
			}
		default:
			t.Errorf("rbac.yaml rendered an unexpected %q", object.Kind)
		}
	}
	if roles != len(values.AllowedNamespaces) || bindings != len(values.AllowedNamespaces) {
		t.Errorf("rendered %d Roles and %d RoleBindings for %d namespaces", roles, bindings, len(values.AllowedNamespaces))
	}
}

func TestTheMCPConfigKeepsTheServerReadOnlyAndHidesSecrets(t *testing.T) {
	t.Parallel()
	var config string
	for _, object := range renderMCPTemplate(t, "config.yaml") {
		if object.Kind == "ConfigMap" {
			config = object.Data["config.toml"]
		}
	}
	if config == "" {
		t.Fatal("the chart renders no kubernetes MCP config")
	}
	for _, want := range []string{
		"read_only = true",
		`enabled_tools = ["resources_get", "resources_list", "pods_get", ` +
			`"pods_list_in_namespace", "pods_log", "events_list"]`,
		"[[denied_resources]]",
		`kind = "Secret"`,
	} {
		if !strings.Contains(config, want) {
			t.Errorf("the kubernetes MCP config does not contain %q", want)
		}
	}
}
