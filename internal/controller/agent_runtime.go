package controller

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/internal/agentapi"
)

// modelAuto is the automatic setting shared by the model options.
const modelAuto = "auto"

const (
	// DefaultCredentialsSecret is the Secret the chart creates with every
	// credential key present. The agent Job receives only its model key.
	DefaultCredentialsSecret = "pagerless-credentials"
	// ModelAPIKeyName is the credentials Secret key that holds the model API key.
	ModelAPIKeyName = "AI_API_KEY"
	// ModelBaseURLName is the credentials Secret key that holds the model endpoint.
	ModelBaseURLName = "AI_BASE_URL"
	// ModelNameName is the credentials Secret key that holds the model identifier.
	ModelNameName = "AI_MODEL"
)

// AgentRuntimeConfig configures the model endpoint and the budgets of an
// investigation. The Narrative Job uses the same endpoint. Credentials are
// referenced by Secret key, never copied into the Job specification.
type AgentRuntimeConfig struct {
	// ModelBaseURL is the OpenAI-compatible endpoint, such as https://api.openai.com/v1.
	ModelBaseURL string
	// ModelName is the model identifier the endpoint expects.
	ModelName string
	// ModelKeySecret is the Secret that holds the model API key.
	ModelKeySecret string
	// ModelKeyKey is the key inside that Secret.
	ModelKeyKey string
	// ModelHeaders are optional request headers as a JSON object.
	ModelHeaders string
	// ModelMaxOutputTokens caps one model call's output, when it is not zero.
	ModelMaxOutputTokens int
	// ModelContextTokens is the model's context window, when it is not zero.
	ModelContextTokens int
	// ModelStructuredOutput selects the first structured-output rung, or auto.
	ModelStructuredOutput string
	// ModelMaxTokensField selects max_tokens or max_completion_tokens, or auto.
	ModelMaxTokensField string
	// ModelSupportsTemperature states whether the endpoint accepts a temperature: auto, true, or false.
	ModelSupportsTemperature string
	// ModelConnectTimeout and ModelReadTimeout are whole seconds, when they are not zero.
	ModelConnectTimeout int
	ModelReadTimeout    int
	// AllowInsecureModelEndpoint permits a plain-HTTP endpoint for an in-cluster model.
	AllowInsecureModelEndpoint bool
	// ToolCallBudget, ModelCallBudget, InputTokenBudget, and WallClockSeconds are
	// the four budgets of one investigation. Zero selects the documented default.
	ToolCallBudget   int
	ModelCallBudget  int
	InputTokenBudget int64
	WallClockSeconds int
}

// ValidateModel refuses a model configuration that names only part of an
// endpoint or an endpoint the Job cannot reach safely. An installation with no
// model configured starts, so that onboarding can set it later; the first Page
// then fails with the reason the endpoint is missing.
func (c AgentRuntimeConfig) ValidateModel() error {
	// The credentials Secret is named even on an installation whose endpoint is
	// not configured yet, so only the endpoint and model decide whether a model
	// is configured at all.
	if c.ModelBaseURL == "" && c.ModelName == "" {
		return nil
	}
	if c.ModelBaseURL == "" {
		return errors.New("the model base URL is required when a model is configured")
	}
	parsed, err := url.Parse(c.ModelBaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("the model base URL must be an HTTP(S) URL without credentials or a query")
	}
	if parsed.Scheme == "http" && !c.AllowInsecureModelEndpoint {
		return errors.New("the model base URL must be https unless an insecure in-cluster endpoint is allowed")
	}
	if c.ModelName == "" {
		return errors.New("the model name is required when a model is configured")
	}
	if strings.ContainsAny(c.ModelName, "\r\n") || len(c.ModelName) > 255 {
		return errors.New("the model name must be a single identifier of at most 255 characters")
	}
	if c.ModelKeySecret == "" || c.ModelKeyKey == "" {
		return errors.New("the model API key must name a Secret and key")
	}
	switch c.ModelStructuredOutput {
	case "", modelAuto, "json_schema", "json_object", "tool", "text":
	default:
		return errors.New("the structured-output mode must be auto, json_schema, json_object, tool, or text")
	}
	switch c.ModelMaxTokensField {
	case "", modelAuto, "max_tokens", "max_completion_tokens":
	default:
		return errors.New("the max-tokens field must be auto, max_tokens, or max_completion_tokens")
	}
	switch c.ModelSupportsTemperature {
	case "", modelAuto, "true", "false":
	default:
		return errors.New("the temperature setting must be auto, true, or false")
	}
	return nil
}

// budget returns the four budgets, with the documented defaults filled in.
func (c AgentRuntimeConfig) budget() (toolCalls, modelCalls int, inputTokens int64, seconds int) {
	toolCalls = c.ToolCallBudget
	if toolCalls == 0 {
		toolCalls = agentapi.DefaultToolCallBudget
	}
	modelCalls = c.ModelCallBudget
	if modelCalls == 0 {
		modelCalls = agentapi.DefaultModelCallBudget
	}
	inputTokens = c.InputTokenBudget
	if inputTokens == 0 {
		inputTokens = agentapi.DefaultInputTokenBudget
	}
	seconds = c.WallClockSeconds
	if seconds == 0 {
		seconds = agentapi.DefaultWallClockBudget
	}
	return toolCalls, modelCalls, inputTokens, seconds
}

// environment is the investigation Job's environment: the two base URLs, the
// Page, the model settings, and the four budgets. It carries no credential
// value: the model key and the report token are mounted as files.
func (c AgentRuntimeConfig) environment() []corev1.EnvVar {
	env := c.modelEnvironment()
	toolCalls, modelCalls, inputTokens, seconds := c.budget()
	env = append(env,
		corev1.EnvVar{Name: agentapi.EnvBudgetToolCalls, Value: fmt.Sprint(toolCalls)},
		corev1.EnvVar{Name: agentapi.EnvBudgetModelCalls, Value: fmt.Sprint(modelCalls)},
		corev1.EnvVar{Name: agentapi.EnvBudgetInputTokens, Value: fmt.Sprint(inputTokens)},
		corev1.EnvVar{Name: agentapi.EnvBudgetSeconds, Value: fmt.Sprint(seconds)},
	)
	return env
}

// modelEnvironment is the model endpoint's configuration, shared by the
// investigation and Narrative Jobs.
func (c AgentRuntimeConfig) modelEnvironment() []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: agentapi.EnvModelBaseURL, Value: c.ModelBaseURL},
		{Name: agentapi.EnvModelName, Value: c.ModelName},
		{Name: agentapi.EnvModelAPIKeyFile, Value: modelKeyMountPath + "/" + modelKeyFileName},
	}
	for _, setting := range []struct{ name, value string }{
		{agentapi.EnvModelHeaders, c.ModelHeaders},
		{agentapi.EnvModelStructuredOutput, c.ModelStructuredOutput},
		{agentapi.EnvModelMaxTokensField, c.ModelMaxTokensField},
		{agentapi.EnvModelSupportsTemperature, c.ModelSupportsTemperature},
	} {
		if setting.value != "" {
			env = append(env, corev1.EnvVar{Name: setting.name, Value: setting.value})
		}
	}
	for _, setting := range []struct {
		name  string
		value int
	}{
		{agentapi.EnvModelMaxOutputTokens, c.ModelMaxOutputTokens},
		{agentapi.EnvModelContextTokens, c.ModelContextTokens},
		{agentapi.EnvModelConnectTimeout, c.ModelConnectTimeout},
		{agentapi.EnvModelReadTimeout, c.ModelReadTimeout},
	} {
		if setting.value > 0 {
			env = append(env, corev1.EnvVar{Name: setting.name, Value: fmt.Sprint(setting.value)})
		}
	}
	if c.AllowInsecureModelEndpoint {
		env = append(env, corev1.EnvVar{Name: agentapi.EnvAllowInsecureModelEndpoint, Value: "1"})
	}
	return env
}

// secretName returns the credentials Secret that holds the model key.
func (c AgentRuntimeConfig) secretName() string {
	if c.ModelKeySecret == "" {
		return DefaultCredentialsSecret
	}
	return c.ModelKeySecret
}

// secretKey returns the key inside the credentials Secret.
func (c AgentRuntimeConfig) secretKey() string {
	if c.ModelKeyKey == "" {
		return ModelAPIKeyName
	}
	return c.ModelKeyKey
}
