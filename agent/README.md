# Investigation agent

`python -m pagerless_agent` runs one investigation Job. The graph reads evidence through agentgateway, which fronts the four MCP services, then sends step events and its report to `pagerless-server`. The agent never writes Kubernetes resources or the Sandbox.

The Job receives its Triage, Workload and projected report-token settings through the environment defined in `internal/agentapi/contract.go`. Its additional runtime settings are:

- `PAGERLESS_TASK`: `investigate` (the default) or `narrative`. The server sets it explicitly on both kinds of Job.

- `PAGERLESS_MODEL_PROVIDER`: `replay` (the default), `anthropic`, or `bedrock`.
- `PAGERLESS_MCP_GATEWAY_URL`: the base URL of agentgateway. The four Streamable HTTP endpoints are fixed routes of that gateway (`/mcp/kubernetes`, `/mcp/grafana`, `/mcp/argocd`, `/mcp/github`); there is no setting for a direct MCP endpoint.
- `PAGERLESS_KEYCLOAK_ISSUER`, `PAGERLESS_KEYCLOAK_TOKEN_URL`, and `PAGERLESS_KEYCLOAK_CLIENT_ID`: the realm issuer that agentgateway validates, its token endpoint, and the federated client the agent authenticates as.
- `PAGERLESS_KEYCLOAK_TOKEN_FILE`: the projected service account token whose only audience is the realm issuer. It is exchanged for an access token on every request; the agent holds no MCP credential.
- `PAGERLESS_SANDBOX_OWNER` and `PAGERLESS_SANDBOX_REPO`: the only GitHub repository the tools may read.
- `PAGERLESS_ARGOCD_APPLICATION`: the Sandbox application name, default `sandbox`.
- `PAGERLESS_ALLOWED_NAMESPACES`: a comma-separated allowlist. It defaults to the target Workload's namespace, which must appear in any explicit list. Tool calls remain restricted to that target namespace.
- `PAGERLESS_REPLAY_FILE`: the recording to play, default `/app/recordings/oom.json`.
- `PAGERLESS_NARRATIVE_REPLAY_FILE`: the Narrative recording to play, default `/app/recordings/narrative.json`.
- `PAGERLESS_RECORDING_FILE`: an optional destination for a recording of a live investigation or Narrative. It is refused in Replay.

The Anthropic provider reads `ANTHROPIC_API_KEY` and uses Claude Haiku 5.5. The Bedrock provider uses Amazon Nova 2 Lite through the Converse API and the `us.amazon.nova-2-lite-v1:0` inference profile, which requires a US region, with the AWS SDK credential chain and optional `AWS_REGION` and `AWS_PROFILE`. These credentials are never included in the model prompt or the Context Bundle.

Both live providers cache their static prefix. The Anthropic provider marks the system prompt and the last tool definition. Nova 2 Lite accepts cache checkpoints only in `system` and `messages`, so the Bedrock provider places one `cachePoint` after the system prompt, which caches the tool definitions with it. Nova 2 Lite does not support Bedrock structured outputs, so a Diagnosis or Proposal travels as one forced tool call on both providers. [ADR-0023](../docs/adr/0023-bedrock-uses-amazon-nova-2-lite.md) records the decision.

## Evidence and tool policy

The deterministic Context Bundle reads the Workload's status and selector, its pods in creation order, associated events, bounded recent logs, Argo CD status, the active Chaos Mesh experiments that select its pods, and the newest Sandbox commits. The pinned Kubernetes MCP service supplies structured resource objects and event rows; the agent does not parse rendered tables or YAML.

Tools are named with service prefixes (`k8s__`, `grafana__`, `argocd__`, and `github__`). Only explicitly allowlisted read tools are advertised or callable. Kubernetes calls cannot select another namespace or cluster, execute commands, proxy, or port-forward. The client does not filter the resource kind: agentgateway's route policy is the argument-level boundary that denies a `resources_get` or `resources_list` call whose `kind` is `Secret`, so the denial is logged there, and the Kubernetes MCP server's own denied-resource configuration and read-only Role are the final boundaries. GitHub calls are restricted to the configured Sandbox and Argo CD calls to its application. agentgateway independently authenticates every call with a Keycloak token and allowlists the same tools. Scoped service credentials and read-only server modes remain further independent boundaries.

## Replay capture

A live provider can be wrapped with `RecordingProvider` by setting `PAGERLESS_RECORDING_FILE`. After the server accepts the report, the runtime validates the normalized recording and replaces the destination atomically. Failed investigations leave the previous artifact intact. Replay continues to execute real MCP tool calls.

The deployed Replay artifact must be captured from a live investigation, as required by ADR-0010, ADR-0016 and ADR-0023. A capture for that artifact uses the Anthropic provider and Claude Haiku 5.5. The synthetic recordings in the test suite exercise the format and runtime; they are not live captures and must not be promoted as demonstration recordings.

## Narrative

`pagerless-server` starts a second, short-lived Job from the same image once a Triage reaches a terminal Verification outcome. `PAGERLESS_TASK=narrative` selects it. The Job reads the Triage's record from the server's agent API, makes exactly one model call capped at `NARRATIVE_MAX_OUTPUT_TOKENS` (2,000) output tokens, and posts the Narrative back. It has no tools, no Context Bundle, and no MCP endpoints, and it never writes a phase, a check, or a Finding.

Because the call has no Context Bundle, Replay cannot resolve an identifier in the Narrative's recorded response. The recording therefore carries no placeholders: the prompt asks for prose without pod names, timestamps, commit identifiers, hashes, or alert fingerprints, and the recording format refuses a response that still names a volatile identifier. `PAGERLESS_NARRATIVE_REPLAY_FILE` selects the recording, default `/app/recordings/narrative.json`. A genuine capture uses `PAGERLESS_RECORDING_FILE` exactly as an investigation does; see [evals/replay](../evals/replay/README.md).
