package controller

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/internal/agentapi"
)

// NarrativeRecordingFileEnv configures the recording that a Narrative Job
// captures. It is a server setting, not part of the agent Job contract: the Job
// receives the file as PAGERLESS_RECORDING_FILE, like an investigation does.
const NarrativeRecordingFileEnv = "PAGERLESS_NARRATIVE_RECORDING_FILE"

// ValidateRecording refuses configurations whose output cannot be retrieved after a Job completes.
// The claim must already exist in the Job's namespace; provisioning it is an operator action.
func (c AgentRuntimeConfig) ValidateRecording() error {
	// A claim with no recording file at all is an unfinished investigation capture. A claim that only the
	// Narrative Job uses is valid, because its file is set separately.
	if c.RecordingFile != "" || (c.RecordingPVC != "" && c.NarrativeRecordingFile == "") {
		if err := validateRecording(c.RecordingFile, c.RecordingPVC, c.Provider, agentapi.EnvRecordingFile); err != nil {
			return err
		}
	}
	if c.NarrativeRecordingFile == "" {
		return nil
	}
	return validateRecording(c.NarrativeRecordingFile, c.RecordingPVC, c.Provider, NarrativeRecordingFileEnv)
}

// validateRecording checks one server-managed recording configuration.
func validateRecording(file, pvc, provider, fileVariable string) error {
	if file == "" && pvc == "" {
		return nil
	}
	if pvc == "" {
		return errors.New("PAGERLESS_RECORDING_PVC is required for a server-managed recording")
	}
	if file == "" {
		return fmt.Errorf("%s is required when PAGERLESS_RECORDING_PVC is set", fileVariable)
	}
	if provider != "anthropic" && provider != "bedrock" {
		return errors.New("a server-managed recording requires a live model provider: anthropic or bedrock")
	}
	if pvc != "pagerless-replay-capture" {
		return errors.New("PAGERLESS_RECORDING_PVC must name the dedicated " +
			"pagerless-replay-capture claim permitted by agent Job admission")
	}
	if !path.IsAbs(file) || path.Clean(file) != file ||
		!strings.HasPrefix(file, "/recordings/") || strings.ContainsRune(file, 0) {
		return fmt.Errorf("%s must name an absolute file strictly inside /recordings without traversal", fileVariable)
	}
	return nil
}

// ValidateGateway refuses configurations that cannot authenticate the agent to
// an identity provider or cannot address the gateway that fronts the MCP
// servers. Both are optional: a deployment that configures neither gives the
// agent Job no tool gateway and no identity-provider token, which is the
// standalone default. When one is configured, every field it needs is required,
// and the issuer and the token endpoint must describe the same realm, because
// the issuer is also the audience of the projected service account token.
func (c AgentRuntimeConfig) ValidateGateway() error {
	if c.GatewayURL == "" && c.KeycloakIssuer == "" && c.KeycloakTokenURL == "" && c.KeycloakClientID == "" {
		return nil
	}
	if c.GatewayURL == "" {
		return errors.New("PAGERLESS_MCP_GATEWAY_URL is required when an MCP gateway is used")
	}
	gateway, err := url.Parse(c.GatewayURL)
	if err != nil || gateway.Host == "" || (gateway.Scheme != "http" && gateway.Scheme != "https") ||
		gateway.User != nil || gateway.RawQuery != "" || gateway.Fragment != "" ||
		(gateway.Path != "" && gateway.Path != "/") {
		return errors.New("PAGERLESS_MCP_GATEWAY_URL must be an HTTP(S) base URL without a path or credentials")
	}
	if c.KeycloakIssuer == "" {
		return errors.New("PAGERLESS_KEYCLOAK_ISSUER is required")
	}
	issuer, err := url.Parse(c.KeycloakIssuer)
	if err != nil || issuer.Host == "" || (issuer.Scheme != "http" && issuer.Scheme != "https") ||
		issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" ||
		!strings.Contains(issuer.Path, "/realms/") || strings.HasSuffix(issuer.Path, "/") {
		return errors.New("PAGERLESS_KEYCLOAK_ISSUER must be an HTTP(S) realm URL without a trailing slash")
	}
	if c.KeycloakTokenURL == "" {
		return errors.New("PAGERLESS_KEYCLOAK_TOKEN_URL is required")
	}
	if c.KeycloakTokenURL != c.KeycloakIssuer+"/protocol/openid-connect/token" {
		return errors.New("PAGERLESS_KEYCLOAK_TOKEN_URL must be the issuer's token endpoint")
	}
	if c.KeycloakClientID == "" || strings.ContainsAny(c.KeycloakClientID, " \t\r\n/") {
		return errors.New("PAGERLESS_KEYCLOAK_CLIENT_ID must be a single non-empty client identifier")
	}
	return nil
}

const (
	// AgentCredentialsSecret holds only the selected provider's inference credentials.
	AgentCredentialsSecret = "pagerless-agent-credentials"
	// DefaultKeycloakClientID is the confidential client that authenticates the agent
	// with its projected service account token.
	DefaultKeycloakClientID = "pagerless-agent"
)

// AgentRuntimeConfig configures the model provider, the MCP gateway, and the
// Keycloak client that authenticates every tool call. The Narrative Job needs
// none of the gateway settings and receives none of them.
// Credentials are referenced by Secret key, never copied into the Job specification.
type AgentRuntimeConfig struct {
	Provider string
	// GatewayURL is the base URL of agentgateway. The agent derives one route per MCP server.
	GatewayURL string
	// KeycloakIssuer is the realm issuer that agentgateway validates and the audience of the
	// projected service account token.
	KeycloakIssuer string
	// KeycloakTokenURL is the realm's token endpoint.
	KeycloakTokenURL string
	// KeycloakClientID is the federated client the agent authenticates as.
	KeycloakClientID  string
	ReplayFile        string
	RecordingFile     string
	SandboxOwner      string
	SandboxRepo       string
	ArgoCDApplication string
	AllowedNamespaces string
	AWSRegion         string
	// RecordingPVC names the dedicated existing pagerless-replay-capture claim, mounted at /recordings.
	RecordingPVC string
	// NarrativeReplayFile names the Narrative Job's Replay recording.
	NarrativeReplayFile string
	// NarrativeRecordingFile is the destination of an explicitly requested live Narrative recording.
	NarrativeRecordingFile string
}

// environment is the investigation Job's environment: the MCP gateway, the
// Keycloak client, the Sandbox scope, the selected provider, and its credentials.
func (c AgentRuntimeConfig) environment() []corev1.EnvVar {
	clientID := c.KeycloakClientID
	if clientID == "" {
		clientID = DefaultKeycloakClientID
	}
	env := c.providerEnvironment()
	for _, setting := range []struct{ name, value string }{
		{agentapi.EnvMCPGatewayURL, c.GatewayURL},
		{agentapi.EnvKeycloakIssuer, c.KeycloakIssuer},
		{agentapi.EnvKeycloakTokenURL, c.KeycloakTokenURL},
		{agentapi.EnvKeycloakClientID, clientID},
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
	return env
}

// narrativeEnvironment is the Narrative Job's environment: the selected provider,
// its credentials, and the Narrative's Replay and recording files.
func (c AgentRuntimeConfig) narrativeEnvironment() []corev1.EnvVar {
	env := c.providerEnvironment()
	for _, setting := range []struct{ name, value string }{
		{agentapi.EnvNarrativeReplayFile, c.NarrativeReplayFile},
		{agentapi.EnvRecordingFile, c.NarrativeRecordingFile},
	} {
		if setting.value != "" {
			env = append(env, corev1.EnvVar{Name: setting.name, Value: setting.value})
		}
	}
	return env
}

// providerEnvironment selects the provider and references only its credentials.
func (c AgentRuntimeConfig) providerEnvironment() []corev1.EnvVar {
	provider := c.Provider
	if provider == "" {
		provider = "replay"
	}
	env := []corev1.EnvVar{{Name: agentapi.EnvModelProvider, Value: provider}}
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
