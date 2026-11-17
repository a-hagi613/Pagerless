package server_test

import (
	"net"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	anthropicProvider    = "anthropic"
	bedrockProvider      = "bedrock"
	recordingPVCVariable = "PAGERLESS_RECORDING_PVC"
	recordingFile        = "/recordings/oom.json"
	recordingClaimName   = "pagerless-replay-capture"
)

func TestAgentJobsReceiveOnlyTheirSelectedProviderAndReadOnlyCredentials(t *testing.T) {
	for _, provider := range []string{"replay", anthropicProvider, bedrockProvider} {
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
			waitForAgentJobPolicy(t, env, serverAdmissionClient(t, env), job)
			pod := job.Spec.Template.Spec
			for _, volume := range pod.Volumes {
				if volume.PersistentVolumeClaim != nil {
					t.Error("a Job without recording configuration must not mount a claim")
				}
			}
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
			if provider == anthropicProvider {
				requireSecretKey(t, variables["ANTHROPIC_API_KEY"], controller.AgentCredentialsSecret, "ANTHROPIC_API_KEY", false)
			} else if _, exists := variables["ANTHROPIC_API_KEY"]; exists {
				t.Errorf("%s Jobs must not receive an Anthropic API key", provider)
			}
			for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
				if provider == bedrockProvider {
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

func TestAgentRecordingUsesOnlyTheExistingClaimAndPreservesTokenIsolation(t *testing.T) {
	for _, provider := range []string{anthropicProvider, bedrockProvider} {
		t.Run(provider, func(t *testing.T) { requireAgentRecordingStorage(t, provider) })
	}
}

func requireAgentRecordingStorage(t *testing.T, provider string) {
	t.Helper()
	env := testenv.Start(t, testenv.WithAgentRuntime(controller.AgentRuntimeConfig{
		Provider: provider, RecordingPVC: recordingClaimName, RecordingFile: recordingFile,
	}))
	claim := &corev1.PersistentVolumeClaim{
		Name: recordingClaimName, Namespace: testenv.Namespace,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("128Mi")},
			},
		},
	}
	if err := env.Client.Create(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)
	job, err := env.GetJob(t.Context(), tr.Status.JobName)
	if err != nil {
		t.Fatal(err)
	}
	waitForAgentJobPolicy(t, env, serverAdmissionClient(t, env), job)
	pod := job.Spec.Template.Spec
	if pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65532 {
		t.Fatal("the recording claim must be writable by filesystem group 65532")
	}
	requireRecordingTokenIsolation(t, pod)
	container := pod.Containers[0]
	if container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("recording must not make the root filesystem writable")
	}
	var recordingVolume string
	for _, volume := range pod.Volumes {
		if volume.PersistentVolumeClaim != nil {
			if volume.PersistentVolumeClaim.ClaimName != recordingClaimName || volume.PersistentVolumeClaim.ReadOnly {
				t.Errorf("unexpected recording claim reference: %+v", volume.PersistentVolumeClaim)
			}
			recordingVolume = volume.Name
		}
	}
	if recordingVolume == "" {
		t.Fatal("the recording Job must reference its configured claim")
	}
	foundMount := false
	for _, mount := range container.VolumeMounts {
		if mount.Name == recordingVolume {
			foundMount = true
			if mount.MountPath != "/recordings" || mount.ReadOnly {
				t.Errorf("recording mount = %+v, want writable /recordings", mount)
			}
		}
	}
	if !foundMount {
		t.Error("the agent container must mount the recording claim")
	}
	foundPath := false
	for _, variable := range container.Env {
		if variable.Name == agentapi.EnvRecordingFile && variable.Value == recordingFile {
			foundPath = true
		}
		if variable.Name == recordingPVCVariable {
			t.Error("the server's storage setting is not part of the agent environment contract")
		}
	}
	if !foundPath {
		t.Error("the agent must receive the configured recording file")
	}
	var claims corev1.PersistentVolumeClaimList
	if err := env.Client.List(t.Context(), &claims, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 1 || claims.Items[0].Name != recordingClaimName {
		t.Error("recording must reuse the existing claim without provisioning more claims")
	}
}

func TestAgentRecordingRejectsIncompleteOrInaccessibleStorageConfiguration(t *testing.T) {
	cases := []struct {
		name, provider, claim, file, want string
	}{
		{"no claim", anthropicProvider, "", "/tmp/oom.json", recordingPVCVariable},
		{"no file", anthropicProvider, recordingClaimName, "", agentapi.EnvRecordingFile},
		{"Replay", "replay", recordingClaimName, recordingFile, "live model provider"},
		{"default Replay", "", recordingClaimName, recordingFile, "live model provider"},
		{"invalid claim", anthropicProvider, "NotValid", recordingFile, recordingPVCVariable},
		{"relative path", anthropicProvider, recordingClaimName, "oom.json", agentapi.EnvRecordingFile},
		{"outside claim", anthropicProvider, recordingClaimName, "/tmp/oom.json", agentapi.EnvRecordingFile},
		{"traversal", anthropicProvider, recordingClaimName, "/recordings/../oom.json", agentapi.EnvRecordingFile},
		{"unrelated claim", anthropicProvider, "private-data", recordingFile, recordingPVCVariable},
		{"directory", anthropicProvider, recordingClaimName, "/recordings", agentapi.EnvRecordingFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			token := func() (string, error) { return "unused", nil }
			_, err = server.New(&rest.Config{Host: "https://127.0.0.1:1"}, server.Config{
				Namespace: testenv.Namespace, AgentImage: testenv.AgentImage, AgentServerURL: "http://server",
				ReportListener: listener, ExecutorURL: "http://executor", ExecutorToken: token,
				ArgoCDURL: "http://argocd", ArgoCDToken: token, AlertmanagerURL: "http://alerts",
				AlertmanagerToken: token,
				AgentRuntime: controller.AgentRuntimeConfig{
					Provider: tc.provider, RecordingPVC: tc.claim, RecordingFile: tc.file,
				},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("configuration error = %v, want %q", err, tc.want)
			}
		})
	}
}

func requireRecordingTokenIsolation(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("recording must not enable the default API token")
	}
	var audiences []string
	for _, volume := range pod.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				audiences = append(audiences, source.ServiceAccountToken.Audience)
			}
		}
	}
	if len(audiences) != 1 || audiences[0] != agentapi.Audience {
		t.Errorf("recording token audiences = %v, want only the report API audience", audiences)
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
