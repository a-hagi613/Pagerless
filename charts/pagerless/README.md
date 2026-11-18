# charts/pagerless

A standalone Helm chart for Pagerless: the `pagerless-server` operator and dashboard, the `pagerless-executor` write identity, the agent's runtime configuration, and the bundled read-only MCP servers.

The chart does **not** install the OpenTelemetry shopping-cart demo (`pagerless-sandbox`), Chaos Mesh, Argo CD, Keycloak, agentgateway, or a Prometheus and Grafana stack. Those are platform services or the reference environment; see [`docs/install.md`](../../docs/install.md) for the bootstrap that adds them.

## What the chart installs

- `Triage` and `TriageApproval` CRDs from `crds/`.
- `ServiceAccount`s `pagerless-server`, `pagerless-agent` (no automatic API token), and `pagerless-executor`.
- The release-namespace `Role`s and `RoleBinding`s for the server and the Executor, their `ClusterRole`s (TokenReview, webhook `caBundle` publication, Node reads), and `Role pagerless-approver`.
- `pagerless-server-validation`, `pagerless-executor-restart`, and `pagerless-mcp-read` `Role`s and `RoleBinding`s in every Workload namespace.
- Admission policies `pagerless-server-dry-run-only` and `pagerless-server-agent-jobs`, plus the `pagerless-server` mutating and validating webhook configurations.
- `ConfigMap`s `pagerless-sandbox` and `pagerless-agent-config`.
- The `pagerless-server` and `pagerless-executor` `Service`s and `Deployment`s, and the `pagerless-history` claim.
- With `mcp.bundled.enabled`, digest-pinned `mcp-kubernetes`, `mcp-grafana`, `mcp-argocd`, and `mcp-github` `Deployment`s.

## Prerequisites

| Requirement | Why |
| --- | --- |
| Kubernetes 1.30 or newer | `ValidatingAdmissionPolicy` is enabled by default from 1.30. |
| An OIDC provider for the dashboard | `pagerless-server` completes sign-in and reads approver group membership through Keycloak's admin API. |
| Secret `oidc.secretName` | Keys `client-secret` (dashboard client) and `server-client-secret` (admin client). |
| Secret `alertmanager.secretName` | Key `token`, the bearer token every Alertmanager delivery must carry. |
| Secret `pagerless-agent-credentials` | The selected provider's inference credential, referenced by key from the agent Job. |
| An MCP gateway at `mcp.gatewayUrl` | The agent reaches every MCP server through one authenticated gateway (ADR-0022). |
| Cluster-admin for the first install | The chart creates `ClusterRole`s, admission policies, and webhook configurations. |

The chart does not create Keycloak, the gateway, or the Secret values. `hack/install.sh` provisions all of them, including the Keycloak realm and the agentgateway charts, and then runs this chart with the same values.

## Install

Add the published repository, or install from the checkout:

```bash
helm upgrade --install pagerless oci://ghcr.io/a-hagi613/charts/pagerless \
  --version 1.0.0 \
  --namespace pagerless \
  --create-namespace
```

```bash
helm upgrade --install pagerless charts/pagerless \
  --namespace pagerless \
  --create-namespace \
  --values my-values.yaml
```

Helm installs the CRDs in `crds/` only when the release is first created. After a chart upgrade, apply them explicitly:

```bash
kubectl apply -f charts/pagerless/crds/pagerless.dev_triages.yaml
kubectl apply -f charts/pagerless/crds/pagerless.dev_triageapprovals.yaml
```

Install into the `pagerless` namespace unless you also change the platform manifests that address it: the gateway in `deploy/gateway` routes to `mcp-*.pagerless.svc`, and its backend-authentication policy reads `mcp-client-tokens` from that namespace. The cluster-scoped names (`pagerless-server`, `pagerless-executor`) are fixed because the admission policies and webhook configurations refer to them.

## Operating modes

Pagerless adapts its Context Bundle, MCP tool discovery, and Action menu to the integrations present in the cluster.

**Live-only mode.** With `sandbox.url` empty, the agent gathers Workload status, pod states, events, container logs, and Grafana telemetry, and the Action menu is `RestartWorkload` and `NoAction`. A human approves, the Executor replaces the Workload's pods one at a time, and Verification observes the Workload for five minutes. The derived MCP servers are `k8s,grafana`.

**GitOps mode.** With `sandbox.url` set to the HTTPS URL of the GitHub repository that holds the Workload manifests, the Context Bundle adds recent commits and, when `argocd.application` is set, Argo CD sync and health status. `Scale`, `PatchResources`, and `Rollback` open a pull request in that repository; `RestartWorkload` still executes live. Set `sandbox.owner` and `sandbox.repo` when the URL is not exactly `https://github.com/<owner>/<repo>`, or `sandbox.githubRepository` for a Git service whose path is longer. The derived MCP servers are `k8s,grafana`, plus `argocd` and `github` when configured.

Set `mcp.servers` to override either derivation, for example `k8s,grafana,github`.

## Secrets for a standalone trial

`secrets.createPlaceholders=true` renders empty `Secret`s `github-read`, `github-write`, `argocd-read`, `mcp-backend-tokens`, and `mcp-client-tokens`. Without them, the server and the MCP pods cannot be scheduled before those integrations have credentials. The Secrets stay empty until real values replace them, and Helm owns them from then on, so a trial is the only intended use. `hack/install.sh` writes the real Secrets first and leaves this option `false`.

External secret managers should create the Secrets directly; the expected names and keys are the defaults of `sandbox.readSecretName`, `sandbox.writeSecretName`, `argocd.secretName`, `mcp.backendTokensSecretName`, `mcp.clientTokensSecretName`, `alertmanager.secretName`, and `oidc.secretName`.

The gateway reads its caller tokens from `mcp-client-tokens` in its own namespace, which is not part of this release. When the gateway runs in `agentgateway-system`, create the same Secret there as well; `hack/install.sh` writes both.

## EKS Pod Identity and IRSA

The agent Job receives no Kubernetes API token. On Amazon EKS, associate the `pagerless` namespace's `pagerless-agent` service account with an IAM role that may call Bedrock, and set `agent.provider=bedrock` and `agent.awsRegion`. The EKS control plane injects the credential provider configuration and the `eks-pod-identity-token` volume before admission, and the `pagerless-server-agent-jobs` policy admits exactly that volume (ADR-0025).

For IRSA, set the role annotation instead:

```yaml
agent:
  provider: bedrock
  awsRegion: us-east-1
  serviceAccount:
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::111122223333:role/pagerless-agent
```

Either way, no model credential is stored in a Secret or in Git. With an API key instead, put it in `pagerless-agent-credentials` under `ANTHROPIC_API_KEY`.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `images.server.repository` | `ghcr.io/a-hagi613/pagerless-server` | Server and dashboard image. |
| `images.server.tag` | `""` | Empty uses `.Chart.AppVersion`. |
| `images.server.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `images.executor.repository` | `ghcr.io/a-hagi613/pagerless-executor` | Executor image. |
| `images.executor.tag` | `""` | Empty uses `.Chart.AppVersion`. |
| `images.executor.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `images.agent.repository` | `ghcr.io/a-hagi613/pagerless-agent` | Agent image, passed to the server as `--agent-image`. |
| `images.agent.tag` | `""` | Empty uses `.Chart.AppVersion`. |
| `images.agent.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `workloadNamespaces` | `[default]` | Workloads the server may read and the Executor may restart. |
| `createWorkloadNamespaces` | `false` | Render a `Namespace` for each entry that is not the release namespace. |
| `agent.provider` | `anthropic` | `anthropic`, `bedrock`, or `replay`. |
| `agent.awsRegion` | `""` | `AWS_REGION` for Bedrock. |
| `agent.serviceAccount.annotations` | `{}` | Pod Identity or IRSA annotations on `pagerless-agent`. |
| `agent.replayFile` | `/app/recordings/oom.json` | Recording replayed when the provider is `replay`. |
| `agent.narrativeReplayFile` | `/app/recordings/narrative.json` | Narrative recording replayed when the provider is `replay`. |
| `sandbox.url` | `""` | GitOps repository URL. Empty selects live-only mode. |
| `sandbox.branch` | `main` | Branch that pull requests target. |
| `sandbox.owner` | `""` | Repository owner; parsed from `sandbox.url` when empty. |
| `sandbox.repo` | `""` | Repository name; parsed from `sandbox.url` when empty. |
| `sandbox.githubRepository` | `""` | Explicit `owner/name` for `PAGERLESS_GITHUB_REPOSITORY`. |
| `sandbox.readSecretName` | `github-read` | Secret with the read-scoped token in key `token`. |
| `sandbox.writeSecretName` | `github-write` | Secret with the write-scoped token in key `token`. |
| `argocd.application` | `""` | Argo CD Application for Verification and the `argocd` MCP server. |
| `argocd.url` | `http://argocd-server.argocd.svc` | In-cluster Argo CD API URL. |
| `argocd.secretName` | `argocd-read` | Secret with the read-only Argo CD token in key `token`. |
| `alertmanager.url` | `http://kube-prometheus-stack-alertmanager.monitoring.svc:9093` | Alertmanager that delivers opted-in alerts. |
| `alertmanager.secretName` | `pagerless-alertmanager` | Secret with the webhook token in key `token`. |
| `grafana.browserUrl` | `http://localhost:3000` | Browser-reachable Grafana URL for panel deep links. |
| `grafana.mcpUrl` | `http://kube-prometheus-stack-grafana.monitoring.svc` | In-cluster Grafana URL queried by `mcp-grafana`. |
| `grafana.workloadDashboardUid` | `""` | Workload metrics dashboard UID; empty keeps the server default. |
| `grafana.logsDashboardUid` | `""` | Workload logs dashboard UID; empty keeps the server default. |
| `dashboard.baseUrl` | `http://localhost:8080` | Browser-facing dashboard URL and OIDC redirect origin. |
| `dashboard.cookieSecure` | `false` | Mark session and flow cookies `Secure`; requires HTTPS. |
| `dashboard.approversGroup` | `pagerless-approvers` | Keycloak group that may approve. |
| `oidc.issuer` | `http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless` | Realm issuer and audience of the agent's projected token. |
| `oidc.tokenUrl` | `""` | Empty derives `<issuer>/protocol/openid-connect/token`. |
| `oidc.keycloakBaseUrl` | `http://keycloak.keycloak.svc.cluster.local:8080` | Keycloak base URL for group membership. |
| `oidc.dashboardClientId` | `pagerless-dashboard` | Dashboard OIDC client. |
| `oidc.serverClientId` | `pagerless-server` | Keycloak admin client. |
| `oidc.agentClientId` | `pagerless-agent` | Federated client the agent authenticates as. |
| `oidc.secretName` | `pagerless-keycloak-dashboard` | Secret with the dashboard and admin client secrets. |
| `mcp.gatewayUrl` | `http://mcp-gateway.agentgateway-system.svc.cluster.local` | MCP gateway base URL. |
| `mcp.servers` | `""` | Explicit server CSV; empty derives it from the integrations. |
| `mcp.bundled.enabled` | `true` | Deploy the four read-only MCP servers. |
| `mcp.githubHost` | `""` | GitHub Enterprise host for `mcp-github`. |
| `mcp.backendTokensSecretName` | `mcp-backend-tokens` | Secret with the Grafana backend token. |
| `mcp.clientTokensSecretName` | `mcp-client-tokens` | Secret with the caller tokens. |
| `persistence.enabled` | `true` | Use a PVC for the history database. |
| `persistence.size` | `1Gi` | Claim size, or the `emptyDir` bound when disabled. |
| `persistence.storageClassName` | `""` | Claim storage class; empty uses the cluster default. |
| `secrets.createPlaceholders` | `false` | Render empty optional integration Secrets. |
| `approvers.subjects` | `[]` | Subjects bound to `Role pagerless-approver`. |

## After installation

```bash
kubectl --namespace pagerless port-forward svc/pagerless-server 8080:8082
kubectl triage --namespace default deploy/<workload-name>
```

The dashboard opens at `dashboard.baseUrl`. Role `pagerless-approver` in the release namespace grants exactly the permission to approve or reject a Proposal; bind it to the people who may decide.
