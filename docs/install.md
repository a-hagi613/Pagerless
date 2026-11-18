# Standalone Cluster Installation

Pagerless installs into any Kubernetes cluster (kind, Amazon EKS, k3s, bare metal, Google Kubernetes Engine, or Azure Kubernetes Service) as an operator, an investigation agent, a web dashboard, and a CLI. Neither installation path deploys the OpenTelemetry shopping-cart demo (`pagerless-sandbox`), Chaos Mesh, or the Argo CD app-of-apps bootstrap used by `make up` and `make cloud-up`.

Choose one of two paths:

- **Option A — direct Helm chart (`charts/pagerless`).** Install the Pagerless release into a cluster that already has an OIDC provider, an MCP gateway, and the integration Secrets. The chart reference is in [`charts/pagerless/README.md`](../charts/pagerless/README.md).
- **Option B — cluster bootstrap (`make install` or `hack/install.sh`).** Provision Keycloak, agentgateway, the gateway routes, the integration Secrets, and then the same chart. This is the path for a cluster that has none of those services yet.

For the self-contained reference environment that includes the OpenTelemetry Demo, Chaos Mesh, and the ten evaluation Scenarios, see the local and cloud bootstrap instructions in [`README.md`](../README.md).

## Operating modes

Pagerless adapts its Context Bundle gathering, MCP tool discovery, and remediation menu to the integrations present in your cluster:

1. **Live-only mode (no GitOps repository required).** When `sandbox.url` (or `--sandbox-url`) is omitted, Pagerless enables the `k8s` and `grafana` MCP servers (`PAGERLESS_MCP_SERVERS=k8s,grafana`) and creates empty placeholder Secrets for GitHub and Argo CD so every control-plane pod starts cleanly. The agent gathers Workload status, pod states, events, container logs, and Grafana telemetry. `RestartWorkload` and `NoAction` Proposals run end-to-end: `pagerless-server` runs server-side dry-run Validation, a human records an Approval via the CLI or dashboard, `pagerless-executor` replaces the Workload's pods one at a time, Verification observes the Workload for five minutes, and the Narrative Job records the post-Verification Narrative.
2. **GitOps mode (with a Sandbox URL and optional Argo CD Application).** When the Sandbox URL points to the GitHub repository that holds your Workload manifests, Pagerless enables the `github` MCP server (and `argocd` when an Application is set). The Context Bundle includes recent commits and Argo CD sync/health status, and the full Action menu (`RestartWorkload`, `Scale`, `PatchResources`, `Rollback`, and `NoAction`) is available.

Missing Chaos Mesh CRDs (`chaos-mesh.org/v1alpha1` `StressChaos`) and unmanaged Argo CD applications are handled gracefully during Context Bundle gathering and never fail an investigation.

## Option A — direct Helm chart

The chart installs the CRDs, the server, the Executor, the dashboard, the admission policies, the webhooks, and the bundled read-only MCP servers. It assumes the platform services below already exist, because they outlive one Pagerless release:

| Prerequisite | Detail |
| --- | --- |
| OIDC provider | A Keycloak realm whose issuer is `oidc.issuer`, with the `pagerless-dashboard` OIDC client, the `pagerless-server` admin client, and the `pagerless-approvers` group. |
| Secret `oidc.secretName` | Keys `client-secret` and `server-client-secret`. |
| Secret `alertmanager.secretName` | Key `token`, the bearer token Alertmanager presents. |
| Secret `pagerless-agent-credentials` | The provider's inference credential. Omit it when EKS Pod Identity or IRSA supplies Bedrock credentials. |
| MCP gateway | An agentgateway Gateway at `mcp.gatewayUrl` fronting the MCP servers, with the Keycloak realm as its JWT issuer. |
| MCP caller and backend tokens | Secrets `mcp-client-tokens` and `mcp-backend-tokens`, unless integrations are disabled. |

Install the published chart, or the chart from a checkout:

```bash
helm upgrade --install pagerless oci://ghcr.io/a-hagi613/charts/pagerless \
  --version 0.2.1 \
  --namespace pagerless \
  --create-namespace \
  --values my-values.yaml
```

```bash
helm upgrade --install pagerless charts/pagerless \
  --namespace pagerless \
  --create-namespace \
  --values my-values.yaml
```

A standalone trial can set `secrets.createPlaceholders=true` to render empty `Secret`s for the optional integrations that are not configured yet. The OIDC client secrets, the Alertmanager token, and the inference credentials are never placeholders. See the [chart reference](../charts/pagerless/README.md) for every value.

Helm installs the CRDs in `charts/pagerless/crds/` only when the release is created. After a chart upgrade, apply them explicitly:

```bash
kubectl apply -f charts/pagerless/crds/pagerless.dev_triages.yaml
kubectl apply -f charts/pagerless/crds/pagerless.dev_triageapprovals.yaml
```

## Option B — cluster bootstrap

`hack/install.sh` (and `make install`, which calls it) adds the platform services to a cluster that has none, then installs the same `charts/pagerless` release:

1. **Namespaces and CustomResourceDefinitions:** creates `pagerless`, `keycloak`, `agentgateway-system`, and every namespace listed in `--namespaces`; the `sandbox` namespace is created only when it is one of them. Applies the `Triage` and `TriageApproval` CRDs from `charts/pagerless/crds/` and the pinned Gateway API `v1.6.3` CRDs (`deploy/gateway-api-crds/standard-install.yaml`).
2. **Keycloak (`deploy/keycloak`):** deploys PostgreSQL `17.12-alpine` and Keycloak `26.8.2`, generates and preserves `Secret keycloak/keycloak-credentials`, and runs `hack/provision-keycloak.py` to configure the `pagerless` realm, the `pagerless-approvers` group, the federated `pagerless-agent` client, the `pagerless-dashboard` OIDC client, and `Secret pagerless/pagerless-keycloak-dashboard`. The Kubernetes identity provider discovers the cluster's ServiceAccount OIDC issuer from `/.well-known/openid-configuration`, so federated token exchange works on EKS and other managed clusters.
3. **agentgateway (`v1.7.0`) and `deploy/gateway`:** installs the `agentgateway-crds` and `agentgateway` OCI charts and applies the `Gateway`, `AgentgatewayBackend`, `HTTPRoute`, and `AgentgatewayPolicy` resources that enforce Keycloak JWT authentication, tool allowlists, and `Secret` resource denials ([ADR-0022](adr/0022-agentgateway-is-the-only-mcp-caller.md)).
4. **Secrets:** idempotently creates `pagerless-alertmanager`, `github-read`, `github-write`, `argocd-read`, `mcp-backend-tokens`, `mcp-client-tokens` (in both `pagerless` and `agentgateway-system`), and `pagerless-agent-credentials` before the release starts, so every pod that mounts one can be scheduled.
5. **Pagerless release (`charts/pagerless`):** runs `helm upgrade --install` with the computed values. The release renders the `ServiceAccount`s, the release-namespace and per-namespace RBAC, both admission policies, the webhook configurations, `ConfigMap pagerless-sandbox` and `ConfigMap pagerless-agent-config`, the history claim, the server and Executor `Service`s and `Deployment`s, and the bundled MCP servers. The installer waits for the server and the Executor to roll out and binds `Role pagerless-approver` to the current explicit Kubernetes user identity when available. `make manifests` copies the generated CRDs into `charts/pagerless/crds/`, and `make validate` fails if the two copies differ.

`hack/install.sh` passes every operator setting as a chart value, so the release is applied exactly as the chart defines it; there is no post-apply patch of the server Deployment.

### Prerequisites

Install these pinned tools on the machine running the installer (versions from [`hack/versions.env`](../hack/versions.env)):

- `kubectl` **v1.37.0** with cluster-admin access to the target cluster context
- `helm` **v4.3.2**
- `uv` **0.13.8** (manages Python **3.14.10** for Keycloak realm and authorization provisioning)
- `openssl`

### Quick start

Preview the installation plan and every rendered manifest without contacting a cluster:

```bash
bash hack/install.sh \
  --dry-run \
  --context my-cluster \
  --namespaces payments,checkout \
  --provider anthropic
```

Install in live-only mode with an Anthropic API key:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."

bash hack/install.sh \
  --context my-cluster \
  --namespaces payments,checkout \
  --provider anthropic \
  --yes
```

Install in live-only mode with Amazon Bedrock:

```bash
export AWS_REGION="us-east-1"
# Supply inference-scoped credentials, or use EKS Pod Identity on the pagerless-agent ServiceAccount:
export AWS_ACCESS_KEY_ID="..."
export AWS_SECRET_ACCESS_KEY="..."

bash hack/install.sh \
  --context my-cluster \
  --namespaces payments,checkout \
  --provider bedrock \
  --yes
```

Install with GitOps and Argo CD:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
export GITHUB_READ_TOKEN="github_pat_..."
export GITHUB_WRITE_TOKEN="github_pat_..."
export ARGOCD_AUTH_TOKEN="..."
export GRAFANA_SERVICE_ACCOUNT_TOKEN="glsa_..."

bash hack/install.sh \
  --context my-cluster \
  --namespaces payments,checkout \
  --provider anthropic \
  --sandbox-url https://github.com/my-org/k8s-manifests \
  --argocd-app payments-prod \
  --mcp-servers k8s,grafana,argocd,github \
  --yes
```

## Command-line options and environment variables

| Option | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `--dry-run` | — | off | Print the installation plan and rendered manifests to stdout without contacting a cluster. |
| `--context <ctx>` | `PAGERLESS_KUBE_CONTEXT` | current `kubectl` context | Kubernetes context to target. |
| `--namespaces <csv>` | `WORKLOAD_NAMESPACES` | `default` | Comma-separated Workload namespaces that `pagerless-server`, `pagerless-executor`, and `mcp-kubernetes` may access. |
| `--provider <name>` | `MODEL_PROVIDER` | `anthropic` | Model provider for investigation and Narrative Jobs (`anthropic`, `bedrock`, or `replay`). |
| `--sandbox-url <url>` | `SANDBOX_URL` | empty (live-only) | HTTPS URL of the GitHub repository holding Workload manifests for pull-request remediations. |
| `--mcp-servers <csv>` | `PAGERLESS_MCP_SERVERS` | auto-detected | Comma-separated MCP servers to enable (`k8s`, `grafana`, `argocd`, `github`). `k8s` is always required. |
| `--argocd-app <app>` | `ARGOCD_APPLICATION` | empty | Argo CD Application name inspected during Context Bundle gathering and Verification. |
| `--grafana-url <url>` | `GRAFANA_URL` | `http://kube-prometheus-stack-grafana.monitoring.svc` | In-cluster Grafana base URL queried by `mcp-grafana`. |
| `--yes`, `-y` | — | off | Skip interactive confirmation before applying changes to the cluster. |

The installer also reads `BROWSER_GRAFANA_URL`, `DASHBOARD_BASE_URL`, `DASHBOARD_COOKIE_SECURE`, `SANDBOX_BRANCH`, `ARGOCD_SERVER_URL`, `ALERTMANAGER_URL`, `PAGERLESS_GITHUB_REPOSITORY`, and `PAGERLESS_MCP_GITHUB_HOST` when they are set, and passes each to the chart.

Optional credentials read from the environment or `.env` (never passed on a command line or printed):

- `ANTHROPIC_API_KEY`: required when `--provider anthropic`.
- `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`: used when `--provider bedrock` (credentials are optional when EKS Pod Identity is bound to `pagerless/pagerless-agent`).
- `GITHUB_READ_TOKEN` and `GITHUB_WRITE_TOKEN`: fine-grained tokens for the repository passed to `--sandbox-url` ([ADR-0015](adr/0015-fine-grained-tokens-for-github.md)).
- `GRAFANA_SERVICE_ACCOUNT_TOKEN`: Viewer service account token for `mcp-grafana`.
- `ARGOCD_AUTH_TOKEN`: read-only Argo CD API token for `mcp-argocd` and post-merge Verification.

## Per-namespace access

The chart binds three least-privilege `Role`s in every namespace listed in `workloadNamespaces` (or `--namespaces`):

- `pagerless-mcp-read` (`ServiceAccount pagerless/mcp-kubernetes`): read-only access to Pods, pod logs, Events, Deployments, StatefulSets, ReplicaSets, and Chaos Mesh `StressChaos`.
- `pagerless-server-validation` (`ServiceAccount pagerless/pagerless-server`): `get`, `list`, and `update` on Deployments and StatefulSets (restricted to server-side dry runs by the `pagerless-server-dry-run-only` `ValidatingAdmissionPolicy`) plus `get` and `list` on Pods.
- `pagerless-executor-restart` (`ServiceAccount pagerless/pagerless-executor`): `get`, `list`, `watch`, and `delete` on Pods plus `get`, `list`, and `watch` on Deployments and StatefulSets for live `RestartWorkload` execution.

Set `mcp.bundled.enabled=false` when an existing installation provides the MCP servers; the chart then skips their Deployments and the `pagerless-mcp-read` Roles.

## Upgrading

`helm upgrade --install` updates every object the release owns. Registry credentials changed by the installer take effect on the next rollout, because the server and the Executor re-read their token files on every request.

A cluster that an earlier release of `hack/install.sh` bootstrapped with `kubectl apply -k` holds those objects outside Helm ownership, and Helm refuses to adopt them. Remove the old bases once, then run the installer:

```bash
kubectl delete -k deploy/pagerless-executor --ignore-not-found
kubectl delete -k deploy/pagerless-server --ignore-not-found
bash hack/install.sh --context my-cluster --namespaces payments,checkout --yes
```

## Verifying the installation

Forward the dashboard and start a Triage against any Deployment or StatefulSet in your configured namespaces:

```bash
kubectl --context my-cluster -n pagerless port-forward svc/pagerless-server 8080:8082
```

With the [`kubectl-triage`](../README.md#cli-reference) plugin on your `PATH`:

```bash
kubectl triage --context my-cluster --namespace payments deploy/my-service --hint "check recent restarts"
kubectl triage --context my-cluster list
```
