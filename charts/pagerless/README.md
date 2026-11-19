# charts/pagerless

A standalone Helm chart that installs the Pagerless control plane: the
`pagerless-server` operator, its dashboard, the agent's runtime configuration,
the admission policies and webhook configurations, and the cluster and
namespace RBAC they rely on.

The chart does not install an OIDC provider, an MCP gateway, Argo CD, Prometheus,
Grafana, or the model endpoint. Those are services the operator already runs, and
the chart references them through values. Credentials always arrive as Secret
references; none is set as a plain value.

## Install

```bash
helm upgrade --install pagerless ./charts/pagerless \
  --namespace pagerless --create-namespace \
  --values my-values.yaml
```

Helm installs the CRDs in `crds/` only when the release is created. Apply them
explicitly after an upgrade:

```bash
kubectl apply -f charts/pagerless/crds/pagerless.dev_triages.yaml
kubectl apply -f charts/pagerless/crds/pagerless.dev_triageapprovals.yaml
```

## What it installs

- `ServiceAccount`s `pagerless-server` and `pagerless-agent` (the agent receives
  no automatic API token).
- The release-namespace `Role` and `RoleBinding` for the server, its
  `ClusterRole` (TokenReview, webhook `caBundle` publication, Node reads), and
  their bindings.
- `Role pagerless-approver` and, when `approvers.subjects` is set, its binding.
- `Role pagerless-server-validation` and its binding in every Workload namespace.
- The `pagerless-server-agent-jobs` and `pagerless-server-dry-run-only`
  admission policies and their bindings.
- The mutating and validating webhook configurations.
- `ConfigMap pagerless-sandbox`, `ConfigMap pagerless-agent-config`, the
  `pagerless-history` claim, the `pagerless-server` Deployment and Service, and
  the `pagerless` Grafana dashboard.

## Values

| Value | Default | Meaning |
| --- | --- | --- |
| `images.server.repository` | `ghcr.io/a-hagi613/pagerless-server` | Server image. |
| `images.server.tag` | `""` | Empty uses `.Chart.AppVersion`. |
| `images.agent.repository` | `ghcr.io/a-hagi613/pagerless-agent` | Agent image. |
| `images.agent.tag` | `""` | Empty uses `.Chart.AppVersion`. |
| `workloadNamespaces` | `[default]` | Namespaces whose Workloads the server may read and propose changes for. |
| `createWorkloadNamespaces` | `false` | Render a Namespace for each entry that is not the release namespace. |
| `agent.provider` | `anthropic` | `anthropic`, `bedrock`, or `replay`. |
| `agent.awsRegion` | `""` | Region for the Bedrock provider. |
| `agent.serviceAccount.annotations` | `{}` | IRSA or Pod Identity binding for the model provider. |
| `agent.replayFile` | `/app/recordings/oom.json` | Recording replayed by an investigation. |
| `agent.narrativeReplayFile` | `/app/recordings/narrative.json` | Recording replayed by a Narrative Job. |
| `sandbox.url` | `""` | Target repository HTTPS URL; empty keeps pull-request Actions off. |
| `sandbox.branch` | `main` | Branch that pull requests target. |
| `sandbox.owner`, `sandbox.repo` | `""` | Parsed from `sandbox.url` when empty. |
| `sandbox.readSecretName` | `github-read` | Secret holding the read-scoped token in the `token` key. |
| `sandbox.writeSecretName` | `github-write` | Secret holding the write-scoped token in the `token` key. |
| `argocd.application` | `""` | Argo CD Application that deploys the Workloads. |
| `argocd.url` | `http://argocd-server.argocd.svc` | In-cluster Argo CD API base URL. |
| `argocd.secretName` | `argocd-read` | Secret holding the read-only Argo CD token in the `token` key. |
| `alertmanager.url` | `http://kube-prometheus-stack-alertmanager.monitoring.svc:9093` | Alertmanager base URL for opted-in alerts. |
| `alertmanager.secretName` | `pagerless-alertmanager` | Secret holding the webhook bearer token in the `token` key. |
| `grafana.browserUrl` | `http://localhost:3000` | Browser-reachable Grafana base URL for panel deep links. |
| `grafana.workloadDashboardUid`, `grafana.logsDashboardUid` | `""` | Dashboard UIDs for the embedded panels. |
| `dashboard.baseUrl` | `http://localhost:8080` | Browser-facing portal URL, also the OIDC redirect origin. |
| `dashboard.cookieSecure` | `false` | Mark session cookies `Secure`; requires HTTPS. |
| `dashboard.approversGroup` | `pagerless-approvers` | Group whose members may approve or reject a Proposal. |
| `oidc.issuer` | Keycloak realm URL | OIDC issuer that signs dashboard users in. |
| `oidc.tokenUrl` | `""` | Empty derives `<issuer>/protocol/openid-connect/token`. |
| `oidc.dashboardClientId` | `pagerless-dashboard` | OIDC client that completes the sign-in. |
| `oidc.secretName` | `pagerless-keycloak-dashboard` | Secret with `client-secret` and `server-client-secret`. |
| `mcp.gatewayUrl` | `http://mcp-gateway.agentgateway-system.svc.cluster.local` | MCP gateway base URL. |
| `mcp.servers` | `""` | Comma-separated servers; empty derives them from the integrations. |
| `persistence.enabled` | `true` | Keep the SQLite history on a PersistentVolumeClaim. |
| `persistence.size` | `1Gi` | Size of the history claim. |
| `secrets.createPlaceholders` | `false` | Create empty optional integration Secrets for a trial. |
| `approvers.subjects` | `[]` | Subjects bound to `Role pagerless-approver`. |

## Verify the rendering

```bash
helm lint charts/pagerless
helm template pagerless charts/pagerless --namespace pagerless
```

`make validate` runs `helm lint` and `kubeconform` over the live-only,
repository-configured, and installer dry-run renderings.
