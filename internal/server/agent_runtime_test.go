package server_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

func TestAgentJobsReceiveOnlyTheirSelectedProviderAndReadOnlyCredentials(t *testing.T) {
	for _, provider := range []string{"replay", "anthropic", "bedrock"} {
		t.Run(provider, func(t *testing.T) {
			runtime := controller.AgentRuntimeConfig{
				Provider: provider, KubernetesURL: "http://mcp-kubernetes:8080/mcp",
				GrafanaURL: "http://mcp-grafana:8000/mcp", ArgoCDURL: "http://mcp-argocd:3000/mcp",
				GitHubURL: "http://mcp-github:8082/readonly", SandboxOwner: "example", SandboxRepo: sandboxNamespace,
				ArgoCDApplication: sandboxNamespace, AllowedNamespaces: sandboxNamespace, AWSRegion: "us-east-1",
				ReplayFile: "/app/recordings/oom.json",
			}
			env := testenv.Start(t, testenv.WithAgentRuntime(runtime))
			created := env.CreateTriage(t, testenv.NewTriage(checkout))
			tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)
			job, err := env.GetJob(t.Context(), tr.Status.JobName)
			if err != nil {
				t.Fatal(err)
			}
			pod := job.Spec.Template.Spec
			if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
				t.Fatal("the agent must not mount an API-server-audience token")
			}
			container := pod.Containers[0]
			if len(container.EnvFrom) != 0 {
				t.Fatal("the agent must receive explicit keys, never every key of a Secret")
			}
			variables := make(map[string]corev1.EnvVar, len(container.Env))
			for _, variable := range container.Env {
				variables[variable.Name] = variable
				if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil {
					secret := variable.ValueFrom.SecretKeyRef.Name
					if secret != controller.AgentCredentialsSecret && secret != controller.MCPClientTokensSecret {
						t.Errorf("the agent references the unexpected Secret %q", secret)
					}
				}
			}
			for name, value := range map[string]string{
				agentapi.EnvModelProvider: provider, agentapi.EnvMCPKubernetesURL: runtime.KubernetesURL,
				agentapi.EnvMCPGrafanaURL: runtime.GrafanaURL, agentapi.EnvMCPArgoCDURL: runtime.ArgoCDURL,
				agentapi.EnvMCPGitHubURL: runtime.GitHubURL, agentapi.EnvSandboxOwner: runtime.SandboxOwner,
				agentapi.EnvSandboxRepo: runtime.SandboxRepo, agentapi.EnvAllowedNamespaces: sandboxNamespace,
			} {
				if variables[name].Value != value {
					t.Errorf("%s = %q, want %q", name, variables[name].Value, value)
				}
			}
			for name, key := range map[string]string{
				agentapi.EnvMCPGrafanaToken: "grafana", agentapi.EnvMCPArgoCDToken: "argocd",
				agentapi.EnvMCPGitHubToken: "github",
			} {
				requireSecretKey(t, variables[name], controller.MCPClientTokensSecret, key, false)
			}
			if provider == "anthropic" {
				requireSecretKey(t, variables["ANTHROPIC_API_KEY"], controller.AgentCredentialsSecret, "ANTHROPIC_API_KEY", false)
			} else if _, exists := variables["ANTHROPIC_API_KEY"]; exists {
				t.Errorf("%s Jobs must not receive an Anthropic API key", provider)
			}
			for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
				if provider == "bedrock" {
					requireSecretKey(t, variables[name], controller.AgentCredentialsSecret, name, true)
				} else if _, exists := variables[name]; exists {
					t.Errorf("%s Jobs must not receive %s", provider, name)
				}
			}
			for _, name := range []string{"GITHUB_WRITE_TOKEN", "GITHUB_TOKEN", "AWS_PROFILE"} {
				if _, exists := variables[name]; exists {
					t.Errorf("agent Job unexpectedly received %s", name)
				}
			}
		})
	}
}

func requireSecretKey(t *testing.T, variable corev1.EnvVar, secret, key string, optional bool) {
	t.Helper()
	if variable.Value != "" || variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s must reference a Secret key without embedding its value", variable.Name)
	}
	ref := variable.ValueFrom.SecretKeyRef
	if ref.Name != secret || ref.Key != key || ref.Optional == nil || *ref.Optional != optional {
		t.Errorf("%s Secret key = %+v, want %s/%s optional=%v", variable.Name, ref, secret, key, optional)
	}
}
