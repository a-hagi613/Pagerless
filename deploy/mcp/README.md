# Read-only MCP services

The `mcp` Argo CD Application renders this chart into `pagerless`. The agent connects directly over Streamable HTTP in Phase 1. No Service is exposed outside the cluster.

Versions were checked against each project's release API and the registry manifests on 2026-11-17. Every image is pinned by both release tag and manifest digest in `values.yaml`:

- [kubernetes-mcp-server v0.1.3](https://github.com/containers/kubernetes-mcp-server/releases/tag/v0.1.3), at `http://mcp-kubernetes.pagerless.svc:8080/mcp`.
- [mcp-grafana v2.3.0](https://github.com/grafana/mcp-grafana/releases/tag/v2.3.0), at `http://mcp-grafana.pagerless.svc:8000/mcp`.
- [mcp-for-argocd v0.9.0](https://github.com/argoproj-labs/mcp-for-argocd/releases/tag/v0.9.0), at `http://mcp-argocd.pagerless.svc:3000/mcp`.
- [github-mcp-server v2.2.0](https://github.com/github/github-mcp-server/releases/tag/v2.2.0), at `http://mcp-github.pagerless.svc:8082/readonly`. This release's HTTP endpoint is `/readonly`, not `/mcp`.

## Permissions and credentials

Only `mcp-kubernetes` mounts a Kubernetes API token. Roles in `allowedNamespaces` permit get, list, and watch on pods, events, Deployments, StatefulSets and ReplicaSets, plus get on `pods/log`. They grant no access to Secrets, exec, port-forward, proxy or cluster-scoped objects. The server also hides write tools and exposes an explicit tool allowlist. The agent service account retains no Kubernetes API permissions and mounts only its report-audience token.

`hack/provision-mcp.py`, run by `make up` after the observability stack is ready, provisions a Grafana Viewer service account and an Argo CD account allowed only to get `default/sandbox`. Grafana's backend token is stored in `mcp-backend-tokens`; Argo CD's is stored in `argocd-read` and is also used by the server's deterministic Verification. Separate random caller tokens in `mcp-client-tokens` authenticate the agent to Grafana MCP and Argo CD MCP.

GitHub MCP HTTP forwards its caller's bearer token to GitHub. Its caller token is therefore the read-only fine-grained Sandbox token from `.env`, rather than a separate random token. The token is limited to the Sandbox fork by GitHub permissions, and the Python registry additionally checks the configured owner and repository. The write token is never supplied to an agent Job or MCP server.

The Kubernetes MCP listener relies on its restricted service account for authorization in Phase 1. It exposes only read operations in the allowlisted namespaces; agentgateway authentication is planned for Phase 2. Argo CD uses internal HTTP for this local kind deployment (`server.insecure`); it has no ingress or externally published Service.

## Runtime configuration

The bootstrap writes non-secret settings to `pagerless-agent-config`. The server copies an explicit set of those settings into each Job. Credential references select only the active provider's keys from `pagerless-agent-credentials`; Replay Jobs receive no model credentials. The MCP caller tokens are referenced by individual Secret keys. No Secret is imported wholesale into a Job environment.

Replay remains the default. Set `MODEL_PROVIDER=anthropic` and `ANTHROPIC_API_KEY` in the git-ignored `.env` to select the Anthropic API. For Bedrock, set `MODEL_PROVIDER=bedrock`, `AWS_REGION`, and inference-scoped AWS credentials; temporary credentials also require `AWS_SESSION_TOKEN`. Host AWS profile files are not mounted into Jobs. EKS Pod Identity can supply credentials through the SDK chain without static keys.

`PAGERLESS_REPLAY_FILE` selects a captured recording; its image default is `/app/recordings/oom.json`. A real OOMKilled recording must be captured before that image can provide the complete Replay loop. `PAGERLESS_RECORDING_FILE=/tmp/oom.json` requests recording during an explicitly selected live run. The Job has a writable, ephemeral `/tmp`; retrieve the recording before deleting the Job. Model responses are normalized by `RecordingProvider` before they are saved.

The bootstrap uses uv 0.13.8 and managed Python 3.14.10, matching the repository's pinned agent toolchain. `PAGERLESS_ENV_FILE` selects an alternative setup file for isolated tests; its default is the repository's `.env`. Explicit `PAGERLESS_SANDBOX_OWNER` and `PAGERLESS_SANDBOX_REPO` select the logical Sandbox when its Git URL is provided by a local test adapter. `PAGERLESS_MCP_GITHUB_URL` can override the MCP endpoint. `PAGERLESS_MCP_GITHUB_HOST` is passed to the official GitHub MCP server as `GITHUB_HOST`; upstream accepts HTTP only for exact loopback hosts and requires HTTPS for other hosts. These overrides do not change the pinned image or read-only tool allowlist.

To add an allowed namespace, update this chart's `allowedNamespaces` and the bootstrap's `PAGERLESS_ALLOWED_NAMESPACES` together. The local Phase 1 stack allows `sandbox` only.
