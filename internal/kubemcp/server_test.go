package kubemcp

import (
	"testing"
)

func TestTheAdvertisedToolsAreReadOnly(t *testing.T) {
	t.Parallel()
	server := &Server{}
	names := make(map[string]bool)
	for _, tool := range server.Tools() {
		names[tool.Name] = true
	}
	forbiddenTools := []string{
		"secrets_get", "pods_exec", "pods_attach", "pods_portforward",
		"resources_create", "resources_delete",
	}
	for _, forbidden := range forbiddenTools {
		if names[forbidden] {
			t.Fatalf("the provider advertises %q", forbidden)
		}
	}
	requiredTools := []string{
		"resources_list", "resources_get", "events_list", "pods_log",
		"namespaces_list", "api_resources",
	}
	for _, required := range requiredTools {
		if !names[required] {
			t.Fatalf("the provider is missing %q", required)
		}
	}
}

func TestTheResourceMapCannotNameSecrets(t *testing.T) {
	t.Parallel()
	if _, found := resources["secret"]; found {
		t.Fatal("the read map admits Secret objects")
	}
	for kind := range resources {
		if kind == "secret" {
			t.Fatal("the read map admits Secret objects")
		}
	}
}

func TestNamespacePolicy(t *testing.T) {
	t.Parallel()
	restricted := &Server{Namespaces: []string{"shop"}}
	if _, err := restricted.namespace(resources["pod"], "other"); err == nil {
		t.Fatal("the namespace allowlist admitted another namespace")
	}
	if _, err := restricted.namespace(resources["pod"], ""); err == nil {
		t.Fatal("the namespace allowlist admitted a cluster-wide read")
	}
	if namespace, err := restricted.namespace(resources["pod"], "shop"); err != nil || namespace != "shop" {
		t.Fatalf("the allowed namespace was refused: %v", err)
	}
	if _, err := restricted.namespace(resources["node"], ""); err != nil {
		t.Fatalf("a cluster-scoped kind was refused: %v", err)
	}
}
