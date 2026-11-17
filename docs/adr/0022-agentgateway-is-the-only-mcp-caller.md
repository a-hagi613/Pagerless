# ADR-0022: agentgateway is the only caller of the MCP servers

- **Status:** Accepted
- **Date:** 2026-11-17
- **Relates to:** [ADR-0007](0007-keycloak-scope.md)

## Context

The agent calls four MCP servers with different caller-authentication schemes. Grafana and Argo CD accept a static caller token, GitHub MCP forwards the caller's bearer token to GitHub, and the Kubernetes MCP server authorizes with its own read-only service account. Phase 1 gave the agent every endpoint and credential directly, so a tool call could bypass any single policy point, and the agent's Job held three MCP tokens.

Tool access must be authenticated, authorized per tool, and auditable on every call. agentgateway 1.7.0 can validate a Keycloak token at the Gateway and deny an MCP tool by name, or by an argument such as a Kubernetes `kind`, only in a route-level `AgentgatewayPolicy`.

## Decision

agentgateway fronts every MCP server. The agent has one setting, `PAGERLESS_MCP_GATEWAY_URL`, and four fixed route paths; it holds no MCP credential and no direct endpoint. It exchanges its projected service account token, whose only audience is the Keycloak realm issuer, for a Keycloak access token through a federated client, and presents that token on every request.

Each route has an allow-only `AgentgatewayPolicy` that lists the MCP methods and tools the agent needs. The Kubernetes route additionally refuses `resources_get` and `resources_list` when the `kind` argument is `Secret`, compared with `lowerAscii()`.

The gateway removes the Keycloak token before forwarding. For servers that still authenticate their caller, a route-level backend-authentication policy reads the Phase 1 token from a Secret in the gateway's namespace and sends it as the backend's bearer token. The Kubernetes MCP server keeps its own service account and receives no token from the gateway.

## Consequences

- One policy point controls every MCP tool call, and #25 can rely on its denials.
- The MCP caller tokens exist in two namespaces: the servers' Secret and the gateway's Secret copy. The agent Job no longer references either.
- A route-level denial is logged as an HTTP 403 without a tool name, because authorization runs before the MCP call is attributed. Allowed calls are logged with the method and tool, which is the audit record.
- The gateway reads a bare token from the Secret and adds the `Bearer` prefix; the Secret must not contain one.
- A new MCP tool needs an entry in `_ALLOWED_TOOLS` and in the route's policy. Adding only the former makes the tool invisible to the agent, not callable outside the allowlist.
