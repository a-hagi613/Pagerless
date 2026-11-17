# MCP gateway

The `mcp-gateway` Argo CD Application renders this chart into
`agentgateway-system`. It owns the agentgateway `Gateway`, one
`AgentgatewayBackend` and one `HTTPRoute` per MCP server, and the
`AgentgatewayPolicy` resources that authenticate and authorize every MCP call.
The agentgateway control plane and its CRDs are installed by the `agentgateway`
and `agentgateway-crds` Applications at chart version `v1.7.0`.

## Endpoints

The Gateway listens on port 80 and is exposed as a `ClusterIP` Service named
`mcp-gateway`, so the only caller is the agent Job inside the cluster.

| Server | Gateway URL | Upstream |
| --- | --- | --- |
| Kubernetes reads | `http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/kubernetes` | `mcp-kubernetes.pagerless.svc:8080/mcp` |
| Grafana reads | `http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/grafana` | `mcp-grafana.pagerless.svc:8000/mcp` |
| Argo CD reads | `http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/argocd` | `mcp-argocd.pagerless.svc:3000/mcp` |
| GitHub reads | `http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/github` | `mcp-github.pagerless.svc:8082/readonly` |

The agent derives these four URLs from `PAGERLESS_MCP_GATEWAY_URL`
(`agent/src/pagerless_agent/mcp.py`, `GATEWAY_ENDPOINTS`); it has no setting for
a direct MCP endpoint.

## Authentication

`AgentgatewayPolicy/mcp-authentication` applies `jwtAuthentication` in `Strict`
mode to the Gateway. It accepts only tokens whose `iss` is
`http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless` and whose
`aud` contains `account`, and it fetches the signing keys from the Keycloak
realm JWKS through the `keycloak` Service. The gateway removes the token before
forwarding, so no MCP server receives the agent's credential.

The agent obtains that token with its projected service account token through
Keycloak client authentication with a federated JWT. The token's only audience
is the realm issuer. `deploy/pagerless-server/agent-job-policy.yaml` admits only
a Keycloak-audience projected token volume alongside the report-audience one.

## Authorization

Each route has an allow-only `AgentgatewayPolicy`. A method or tool that no
expression matches is denied. The method allowlist covers the MCP session setup
(`initialize`, `notifications/initialized`, `tools/list`) plus `ping` and
`notifications/cancelled`, because the policy runs on every HTTP request, not
only on tool calls. It also allows `GET` and `DELETE`, which the MCP client uses
for the optional server-sent-events stream and session termination; neither can
carry a tool call.

The tool lists mirror `_ALLOWED_TOOLS` in `agent/src/pagerless_agent/mcp.py`.
On the Kubernetes route, `resources_get` and `resources_list` are allowed only
when the call names a `kind` other than `Secret` (compared with
`lowerAscii()`), which is the only argument-level control agentgateway 1.7.0
offers. A denied call returns HTTP 403.

`AgentgatewayPolicy` resources named `mcp-<server>-backend-auth` keep the Phase
1 caller credentials in place for Grafana, Argo CD and GitHub. The gateway reads
each bare token from the `mcp-client-tokens` Secret in `agentgateway-system`
and sends it to the server; `hack/provision-mcp.py` writes that Secret. The
Kubernetes MCP server keeps its own read-only service account, so the gateway
sends it no credential.

## Access logs

The proxy writes an access log entry to stdout for every request, including the
MCP method and, for a tool call, `gen_ai.tool.name`. The
`AgentgatewayPolicy/mcp-access-log` adds `agent.subject`, the Keycloak `sub`
claim of the federated client's service-account user (a Keycloak user UUID, not
the Kubernetes service account name). The OpenTelemetry Collector reads every
pod's stdout and exports it to Loki, so no extra pipeline is needed.

With the Grafana port-forward from the root README, select the Loki data source
and query:

```logql
{k8s_namespace_name="agentgateway-system"} |= "gen_ai.tool.name"
{k8s_namespace_name="agentgateway-system"} |= "gen_ai.tool.name=resources_get"
{k8s_namespace_name="agentgateway-system"} |= "agent.subject="
{k8s_namespace_name="agentgateway-system"} |= "authorization failed"
```

The last query finds a denied call. A route-level denial is logged as a
transport-level HTTP 403 with no tool name or arguments, because authorization
runs before the MCP call is attributed. An allowed tool call is fully
attributed, which is what the audit trail records.

## Verification

The path was verified on a disposable kind v0.33.0 cluster (Kubernetes 1.37.0)
with Keycloak 26.8.2, agentgateway 1.7.0, the pinned Kubernetes MCP server, and
the repository's Loki and OpenTelemetry Collector values. A pod holding only a
projected service account token with the realm issuer as its audience exchanged
it for an access token (`iss` and `azp` matched, `aud` was `account`). Through the
gateway, `initialize` and `tools/list` succeeded and exposed exactly the six
allowed tools; a ConfigMap read and `pods_list_in_namespace` returned 200; a
`resources_get` for a Secret returned HTTP 403, as did the uppercase `SECRET`
kind and a tool outside the allowlist; a request without a token returned 401.
The allowed call and the denial were both found in Loki with the queries above.

## Versions

| Component | Version | Pin |
| --- | --- | --- |
| Agentgateway control plane | `v1.7.0` | OCI chart `agentgateway` `v1.7.0`, digest `sha256:40340d639e35206317c934e9af3e29575cf7c5377b2940679732baac6e108c15` |
| Agentgateway CRDs | `v1.7.0` | OCI chart `agentgateway-crds` `v1.7.0`, digest `sha256:946b739c0fcda4d47a72d1589d184b6fd507772e733cbedf02789bafd0468064` |
| Agentgateway proxy image | `v1.7.0` | `cr.agentgateway.dev/agentgateway@sha256:60216aafb03d37f94e952ff3d51d2f630a65b10ecaebdc0a864882f92610c45b` |
| Gateway API CRDs | `v1.6.3` | Vendored in [`deploy/gateway-api-crds`](../gateway-api-crds/README.md) |
