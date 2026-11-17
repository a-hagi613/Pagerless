package controller

import (
	"errors"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/internal/agentapi"
)

// ValidateRecording refuses configurations whose output cannot be retrieved after a Job completes.
// The claim must already exist in the Job's namespace; provisioning it is an operator action.
func (c AgentRuntimeConfig) ValidateRecording() error {
	if c.RecordingFile == "" && c.RecordingPVC == "" {
		return nil
	}
	if c.RecordingPVC == "" {
		return errors.New("PAGERLESS_RECORDING_PVC is required for a server-managed recording")
	}
	if c.RecordingFile == "" {
		return errors.New("PAGERLESS_RECORDING_FILE is required when PAGERLESS_RECORDING_PVC is set")
	}
	if c.Provider != "anthropic" && c.Provider != "bedrock" {
		return errors.New("a server-managed recording requires a live model provider: anthropic or bedrock")
	}
	if c.RecordingPVC != "pagerless-replay-capture" {
		return errors.New("PAGERLESS_RECORDING_PVC must name the dedicated " +
			"pagerless-replay-capture claim permitted by agent Job admission")
	}
	if !path.IsAbs(c.RecordingFile) || path.Clean(c.RecordingFile) != c.RecordingFile ||
		!strings.HasPrefix(c.RecordingFile, "/recordings/") || strings.ContainsRune(c.RecordingFile, 0) {
		return errors.New("PAGERLESS_RECORDING_FILE must name an absolute file strictly inside /recordings without traversal")
	}
	return nil
}

const (
	// AgentCredentialsSecret holds only the selected provider's inference credentials.
	AgentCredentialsSecret = "pagerless-agent-credentials"
	// MCPClientTokensSecret holds credentials that authorize read-only MCP calls.
	MCPClientTokensSecret = "mcp-client-tokens"
)

// AgentRuntimeConfig configures a Job's model provider and read-only evidence sources.
// Credentials are referenced by Secret key, never copied into the Job specification.
type AgentRuntimeConfig struct {
	Provider          string
	KubernetesURL     string
	GrafanaURL        string
	ArgoCDURL         string
	GitHubURL         string
	ReplayFile        string
	RecordingFile     string
	SandboxOwner      string
	SandboxRepo       string
	ArgoCDApplication string
	AllowedNamespaces string
	AWSRegion         string
	// RecordingPVC names the dedicated existing pagerless-replay-capture claim, mounted at /recordings.
	RecordingPVC string
}

func (c AgentRuntimeConfig) environment() []corev1.EnvVar {
	provider := c.Provider
	if provider == "" {
		provider = "replay"
	}
	env := []corev1.EnvVar{{Name: agentapi.EnvModelProvider, Value: provider}}
	for _, setting := range []struct{ name, value string }{
		{agentapi.EnvMCPKubernetesURL, c.KubernetesURL},
		{agentapi.EnvMCPGrafanaURL, c.GrafanaURL},
		{agentapi.EnvMCPArgoCDURL, c.ArgoCDURL},
		{agentapi.EnvMCPGitHubURL, c.GitHubURL},
		{agentapi.EnvReplayFile, c.ReplayFile},
		{agentapi.EnvRecordingFile, c.RecordingFile},
		{agentapi.EnvSandboxOwner, c.SandboxOwner},
		{agentapi.EnvSandboxRepo, c.SandboxRepo},
		{agentapi.EnvArgoCDApplication, c.ArgoCDApplication},
		{agentapi.EnvAllowedNamespaces, c.AllowedNamespaces},
	} {
		if setting.value != "" {
			env = append(env, corev1.EnvVar{Name: setting.name, Value: setting.value})
		}
	}
	for _, endpoint := range []struct{ url, name, key string }{
		{c.GrafanaURL, agentapi.EnvMCPGrafanaToken, "grafana"},
		{c.ArgoCDURL, agentapi.EnvMCPArgoCDToken, "argocd"},
		{c.GitHubURL, agentapi.EnvMCPGitHubToken, "github"},
	} {
		if endpoint.url != "" {
			env = append(env, secretEnvironment(endpoint.name, MCPClientTokensSecret, endpoint.key, false))
		}
	}
	switch provider {
	case "anthropic":
		env = append(env, secretEnvironment("ANTHROPIC_API_KEY", AgentCredentialsSecret, "ANTHROPIC_API_KEY", false))
	case "bedrock":
		if c.AWSRegion != "" {
			env = append(env, corev1.EnvVar{Name: "AWS_REGION", Value: c.AWSRegion})
		}
		// These are optional so an EKS Pod Identity can supply the SDK's credentials instead.
		for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
			env = append(env, secretEnvironment(name, AgentCredentialsSecret, name, true))
		}
	}
	return env
}

func secretEnvironment(name, secret, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			Name: secret, Key: key, Optional: new(optional),
		},
	}}
}
