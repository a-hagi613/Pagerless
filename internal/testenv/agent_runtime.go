package testenv

import "github.com/a-hagi613/pagerless/internal/controller"

// WithAgentRuntime configures the model provider and MCP endpoints of created agent Jobs.
func WithAgentRuntime(config controller.AgentRuntimeConfig) Option {
	return func(o *options) { o.agentRuntime = config }
}
