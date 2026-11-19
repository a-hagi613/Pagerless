package server_test

import (
	"net"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// The model endpoint the Job tests configure.
const (
	testModelName    = "model"
	testModelURL     = "https://model.test/v1"
	testModelHTTPURL = "http://model.test/v1"
)

// TestTheAgentJobMountsExactlyTheReportTokenAndModelKey is the Job's credential
// contract: two mounts, no automatic token, no kubeconfig, and no Connector or
// GitHub credential.
func TestTheAgentJobMountsExactlyTheReportTokenAndModelKey(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	page := env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)
	job, err := env.GetJob(t.Context(), page.Status.AgentJobName)
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("the agent must not mount the API-server-audience token")
	}
	if pod.ServiceAccountName != server.DefaultAgentServiceAccount {
		t.Errorf("service account = %q, want the zero-RBAC agent account", pod.ServiceAccountName)
	}

	projected, secrets, empty := credentialVolumes(pod)
	if len(projected) != 1 {
		t.Fatalf("projected token volumes = %v, want exactly report-token", projected)
	}
	wantAudience := agentapi.AudienceForPage(string(page.UID))
	if projected["report-token"] != wantAudience {
		t.Errorf("report-token audience = %q, want %q", projected["report-token"], wantAudience)
	}
	if len(secrets) != 1 {
		t.Fatalf("Secret volumes = %v, want exactly model-key", secrets)
	}
	modelKey := secrets["model-key"]
	if modelKey.SecretName != controller.DefaultCredentialsSecret {
		t.Errorf("model-key Secret = %q, want %q", modelKey.SecretName, controller.DefaultCredentialsSecret)
	}
	if len(modelKey.Items) != 1 || modelKey.Items[0].Key != controller.ModelAPIKeyName ||
		modelKey.Items[0].Path != "api-key" {
		t.Errorf("model-key items = %+v, want only %s at api-key", modelKey.Items, controller.ModelAPIKeyName)
	}
	if len(empty) != 1 || empty[0] != "temporary" {
		t.Errorf("non-credential volumes = %v, want only the temporary emptyDir", empty)
	}

	container := pod.Containers[0]
	mounts := map[string]string{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount.MountPath
	}
	if mounts["report-token"] != "/var/run/pagerless/report-token" ||
		mounts["model-key"] != "/var/run/pagerless/model-key" {
		t.Errorf("mounts = %v, want the report-token and model-key mounts", mounts)
	}
	if len(mounts) != 3 {
		t.Errorf("volume mounts = %v, want exactly the two credentials and /tmp", mounts)
	}

	variables := map[string]corev1.EnvVar{}
	for _, variable := range container.Env {
		variables[variable.Name] = variable
		if variable.ValueFrom != nil {
			t.Errorf("%s is sourced from a Secret or field, but the agent reads only mounted files", variable.Name)
		}
	}
	for name, want := range map[string]string{
		agentapi.EnvPageUID:         string(page.UID),
		agentapi.EnvTokenFile:       "/var/run/pagerless/report-token/token",
		agentapi.EnvModelAPIKeyFile: "/var/run/pagerless/model-key/api-key",
		agentapi.EnvTask:            agentapi.TaskInvestigate,
	} {
		if variables[name].Value != want {
			t.Errorf("%s = %q, want %q", name, variables[name].Value, want)
		}
	}
	for _, forbidden := range []string{
		"PAGERLESS_MCP_GATEWAY_URL", "PAGERLESS_REPLAY_FILE",
		"PAGERLESS_RECORDING_FILE", "PAGERLESS_RECORDING_PVC", "PAGERLESS_SANDBOX_OWNER",
		"PAGERLESS_SANDBOX_REPO", "PAGERLESS_ALLOWED_NAMESPACES", "PAGERLESS_TARGET_KIND",
		"GITHUB_TOKEN", "GITHUB_WRITE_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"ANTHROPIC_API_KEY", "KUBECONFIG",
	} {
		if _, exists := variables[forbidden]; exists {
			t.Errorf("the agent Job must not receive %s", forbidden)
		}
	}
	for _, volume := range pod.Volumes {
		if volume.ConfigMap != nil {
			t.Errorf("volume %q mounts a ConfigMap, which could carry a kubeconfig", volume.Name)
		}
	}
}

// TestTheNarrativeJobMountsTheSameTwoCredentials keeps the Narrative Job on the
// same credential contract as the investigation.
func TestTheNarrativeJobMountsTheSameTwoCredentials(t *testing.T) {
	t.Parallel()
	page := &v1alpha1.Page{}
	page.Name, page.Namespace = "checkout-page", testenv.Namespace
	page.UID = "2b6b3e0e-1111-4222-8333-444455556666"
	job := controller.NarrativeJob(page, controller.AgentJobConfig{
		Image: "agent:test", ServiceAccount: server.DefaultAgentServiceAccount,
		ServerURL: "http://server", Deadline: 120,
		Runtime: controller.AgentRuntimeConfig{
			ModelBaseURL: testModelURL, ModelName: testModelName,
			ModelKeySecret: controller.DefaultCredentialsSecret, ModelKeyKey: controller.ModelAPIKeyName,
		},
	})
	pod := job.Spec.Template.Spec
	projected, secrets, _ := credentialVolumes(pod)
	if len(projected) != 1 || projected["report-token"] != agentapi.AudienceForPage(string(page.UID)) {
		t.Errorf("projected tokens = %v, want the Page's report token", projected)
	}
	if len(secrets) != 1 || secrets["model-key"].SecretName != controller.DefaultCredentialsSecret {
		t.Errorf("Secret volumes = %v, want exactly model-key", secrets)
	}
}

// TestTheModelConfigurationIsRefusedWhenItIsUnsafe covers the endpoint checks
// that keep a Job from reaching an unexpected destination.
func TestTheModelConfigurationIsRefusedWhenItIsUnsafe(t *testing.T) {
	t.Parallel()
	valid := controller.AgentRuntimeConfig{
		ModelBaseURL: "https://api.example.com/v1", ModelName: testModelName,
		ModelKeySecret: controller.DefaultCredentialsSecret, ModelKeyKey: controller.ModelAPIKeyName,
	}
	if err := valid.ValidateModel(); err != nil {
		t.Fatalf("a valid model configuration was refused: %v", err)
	}
	if err := (controller.AgentRuntimeConfig{}).ValidateModel(); err != nil {
		t.Fatalf("an unconfigured model must let the server start: %v", err)
	}
	cases := []struct {
		name   string
		change func(*controller.AgentRuntimeConfig)
		want   string
	}{
		{"plain HTTP", func(c *controller.AgentRuntimeConfig) { c.ModelBaseURL = testModelHTTPURL }, "https"},
		{"credentials in the URL", func(c *controller.AgentRuntimeConfig) {
			c.ModelBaseURL = "https://user:secret@model.test/v1"
		}, "without credentials"},
		{"a query", func(c *controller.AgentRuntimeConfig) { c.ModelBaseURL += "?key=x" }, "without credentials"},
		{"no model", func(c *controller.AgentRuntimeConfig) { c.ModelName = "" }, "model name"},
		{"no key Secret", func(c *controller.AgentRuntimeConfig) { c.ModelKeySecret = "" }, "Secret"},
		{"unknown structured output", func(c *controller.AgentRuntimeConfig) {
			c.ModelStructuredOutput = "yaml"
		}, "structured-output"},
		{"unknown tokens field", func(c *controller.AgentRuntimeConfig) { c.ModelMaxTokensField = "tokens" }, "max-tokens"},
		{"unknown temperature", func(c *controller.AgentRuntimeConfig) {
			c.ModelSupportsTemperature = "maybe"
		}, "temperature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := valid
			tc.change(&config)
			if err := config.ValidateModel(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("configuration error = %v, want %q", err, tc.want)
			}
		})
	}
	insecure := valid
	insecure.ModelBaseURL = testModelHTTPURL
	insecure.AllowInsecureModelEndpoint = true
	if err := insecure.ValidateModel(); err != nil {
		t.Errorf("an explicitly allowed in-cluster HTTP endpoint was refused: %v", err)
	}
}

// TestTheAgentDeadlineLeavesTimeToPostTheOutcome keeps the Job's deadline above
// the investigation's wall-clock budget.
func TestTheAgentDeadlineLeavesTimeToPostTheOutcome(t *testing.T) {
	t.Parallel()
	if server.DefaultAgentDeadline <= 300*1e9 {
		t.Errorf("agent deadline = %s, want more than the 300-second budget", server.DefaultAgentDeadline)
	}
}

// TestAnUnsafeModelConfigurationStopsTheServer checks that the server refuses to
// start with a configuration its Jobs could not use.
func TestAnUnsafeModelConfigurationStopsTheServer(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, err = server.New(&rest.Config{Host: "https://127.0.0.1:1"}, server.Config{
		Namespace: testenv.Namespace, AgentImage: testenv.AgentImage, AgentServerURL: "http://server",
		ReportListener: listener,
		AgentRuntime: controller.AgentRuntimeConfig{
			ModelBaseURL: testModelHTTPURL, ModelName: testModelName,
			ModelKeySecret: controller.DefaultCredentialsSecret, ModelKeyKey: controller.ModelAPIKeyName,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("configuration error = %v, want the plain-HTTP refusal", err)
	}
}

// credentialVolumes splits the Job's volumes into projected tokens, Secret
// mounts, and everything else.
func credentialVolumes(pod corev1.PodSpec) (map[string]string, map[string]*corev1.SecretVolumeSource, []string) {
	projected := map[string]string{}
	secrets := map[string]*corev1.SecretVolumeSource{}
	var other []string
	for _, volume := range pod.Volumes {
		switch {
		case volume.Projected != nil:
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken != nil {
					projected[volume.Name] = source.ServiceAccountToken.Audience
				}
			}
		case volume.Secret != nil:
			secrets[volume.Name] = volume.Secret
		default:
			other = append(other, volume.Name)
		}
	}
	return projected, secrets, other
}
