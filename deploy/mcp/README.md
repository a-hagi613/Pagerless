# Read-only MCP services

The `mcp` Argo CD Application renders this chart into `pagerless`. agentgateway, in [`deploy/gateway`](../gateway/README.md), fronts every server; the agent connects only to the gateway, which authenticates each call with a Keycloak token and authorizes it by tool. No Service is exposed outside the cluster.

Grafana MCP validates the HTTP `Host` header against the configured Service address, `mcp-grafana.pagerless.svc:8000`. Its default loopback-only allowlist would reject the agent's requests even while the TCP readiness probe passes.

Versions were checked against each project's release API and the registry manifests on 2026-11-17. Every image is pinned by both release tag and manifest digest in `values.yaml`:

- [kubernetes-mcp-server v0.1.3](https://github.com/containers/kubernetes-mcp-server/releases/tag/v0.1.3), at `http://mcp-kubernetes.pagerless.svc:8080/mcp`.
- [mcp-grafana v2.3.0](https://github.com/grafana/mcp-grafana/releases/tag/v2.3.0), at `http://mcp-grafana.pagerless.svc:8000/mcp`.
- [mcp-for-argocd v0.9.0](https://github.com/argoproj-labs/mcp-for-argocd/releases/tag/v0.9.0), at `http://mcp-argocd.pagerless.svc:3000/mcp`.
- [github-mcp-server v2.2.0](https://github.com/github/github-mcp-server/releases/tag/v2.2.0), at `http://mcp-github.pagerless.svc:8082/readonly`. This release's HTTP endpoint is `/readonly`, not `/mcp`.

## Permissions and credentials

Only `mcp-kubernetes` mounts a Kubernetes API token. Roles in `allowedNamespaces` permit get, list, and watch on pods, events, Deployments, StatefulSets and ReplicaSets, plus get on `pods/log`. They also permit get, list and watch on `chaos-mesh.org/v1alpha1` `StressChaos`, so that the Context Bundle can name an active experiment that targets the Workload. They grant no access to Secrets, exec, port-forward, proxy or cluster-scoped objects, and no other Chaos Mesh kind or group. The server also hides write tools and exposes an explicit tool allowlist. The agent service account retains no Kubernetes API permissions and mounts only its report-audience token.

`hack/provision-mcp.py`, run by `make up` after the observability stack is ready, provisions a Grafana Viewer service account and an Argo CD account allowed only to get `default/sandbox`. Grafana's backend token is stored in `mcp-backend-tokens`; Argo CD's is stored in `argocd-read` and is also used by the server's deterministic Verification. Separate random caller tokens in `mcp-client-tokens` authenticate the gateway to Grafana MCP and Argo CD MCP.

GitHub MCP HTTP forwards its caller's bearer token to GitHub. The gateway is that caller, so `mcp-client-tokens` also holds the read-only fine-grained Sandbox token from `.env`, and `hack/provision-mcp.py` copies the Secret into `agentgateway-system` where the gateway reads it. The token is limited to the Sandbox fork by GitHub permissions, and the Python registry additionally checks the configured owner and repository. The write token is never supplied to an agent Job, MCP server, or gateway.

The Kubernetes MCP server relies on its restricted service account for authorization. It exposes only read operations in the allowlisted namespaces; agentgateway authenticates every caller in front of it and denies Secret reads by argument. Argo CD uses internal HTTP for this local kind deployment (`server.insecure`); it has no ingress or externally published Service.

## Runtime configuration

The bootstrap writes non-secret settings to `pagerless-agent-config`. The server copies an explicit set of those settings into each Job. Credential references select only the active provider's keys from `pagerless-agent-credentials`; Replay Jobs receive no model credentials. The MCP caller tokens are referenced by individual Secret keys. No Secret is imported wholesale into a Job environment.

Replay remains the default. Set `MODEL_PROVIDER=anthropic` and `ANTHROPIC_API_KEY` in the git-ignored `.env` to select the Anthropic API. For Bedrock, set `MODEL_PROVIDER=bedrock`, `AWS_REGION`, and inference-scoped AWS credentials; temporary credentials also require `AWS_SESSION_TOKEN`. Host AWS profile files are not mounted into Jobs. EKS Pod Identity can supply credentials through the SDK chain without static keys.

`PAGERLESS_REPLAY_FILE` selects a captured recording; its image default is `/app/recordings/oom.json`. A real OOMKilled recording must be captured before that image can provide the complete Replay loop.

For a server-managed live recording, create a dedicated PersistentVolumeClaim named `pagerless-replay-capture` in `pagerless`, then set `PAGERLESS_RECORDING_PVC=pagerless-replay-capture` and `PAGERLESS_RECORDING_FILE=/recordings/oom.json` alongside the live provider settings. The server mounts the existing claim at `/recordings` and sets filesystem group 65532 for the non-root agent. The storage provider must support filesystem group ownership. Pagerless neither provisions nor deletes the claim. Independent Job admission and server validation allow only this claim name, so changing it requires an operator to update both boundaries. Never store unrelated data or credentials on the capture claim. After the agent Job completes, mount the same claim in a retrieval pod to copy the normalized recording. Clear both recording settings after capture. Replay and ordinary nonrecording Jobs mount no recording claim.

The server rejects a recording file without a claim, a claim without an output file, a path outside `/recordings`, and recording configuration with Replay. The agent container exits after saving its report and recording, so an ephemeral `/tmp` output would not provide a supported way to retrieve a completed Job's capture. The standalone Python runtime can still record to host-accessible paths when run outside Kubernetes. Model responses are normalized by `RecordingProvider` before they are saved.

The bootstrap uses uv 0.13.8 and managed Python 3.14.10, matching the repository's pinned agent toolchain. `PAGERLESS_ENV_FILE` selects an alternative setup file for isolated tests; its default is the repository's `.env`. Explicit `PAGERLESS_SANDBOX_OWNER` and `PAGERLESS_SANDBOX_REPO` select the logical Sandbox when its Git URL is provided by a local test adapter. `PAGERLESS_MCP_GITHUB_HOST` is passed to the official GitHub MCP server as `GITHUB_HOST`; upstream accepts HTTP only for exact loopback hosts and requires HTTPS for other hosts. These overrides do not change the pinned image or read-only tool allowlist.

To add an allowed namespace, update this chart's `allowedNamespaces` and the bootstrap's `PAGERLESS_ALLOWED_NAMESPACES` together. The local Phase 1 stack allows `sandbox` only.
