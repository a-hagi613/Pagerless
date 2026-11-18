# Standalone Cluster Installation

`hack/install.sh` installs Pagerless into any Kubernetes cluster (kind, Amazon EKS, k3s, bare metal, Google Kubernetes Engine, or Azure Kubernetes Service) without deploying the OpenTelemetry Demo, Chaos Mesh, or the Argo CD app-of-apps bootstrap used by `make up` and `make cloud-up`.

Use `hack/install.sh` when you want to run Pagerless against your own Workloads and namespaces. For the self-contained reference environment that includes the OpenTelemetry Demo, Chaos Mesh, and the ten evaluation Scenarios, see the local and cloud bootstrap instructions in [`README.md`](../README.md).

## Operating modes

Pagerless adapts its Context Bundle gathering, MCP tool discovery, and remediation menu to the integrations present in your cluster:

1. **Live-only mode (no GitOps repository required).** When `--sandbox-url` is omitted, `hack/install.sh` enables the `k8s` and `grafana` MCP servers (`PAGERLESS_MCP_SERVERS=k8s,grafana`) and creates empty placeholder Secrets for GitHub and Argo CD so every control-plane pod starts cleanly. The agent gathers Workload status, pod states, events, container logs, and Grafana telemetry. `RestartWorkload` and `NoAction` Proposals run end-to-end: `pagerless-server` runs server-side dry-run Validation, a human records an Approval via the CLI or dashboard, `pagerless-executor` replaces the Workload's pods one at a time, Verification observes the Workload for five minutes, and the Narrative Job records the post-Verification Narrative.
2. **GitOps mode (with `--sandbox-url` and optional `--argocd-app`).** When `--sandbox-url` points to the GitHub repository that holds your Workload manifests, `hack/install.sh` enables the `github` MCP server (and `argocd` when `--argocd-app` is set). The Context Bundle includes recent commits and Argo CD sync/health status, and the full Action menu (`RestartWorkload`, `Scale`, `PatchResources`, `Rollback`, and `NoAction`) is available.

Missing Chaos Mesh CRDs (`chaos-mesh.org/v1alpha1` `StressChaos`) and unmanaged Argo CD applications are handled gracefully during Context Bundle gathering and never fail an investigation.

## Prerequisites

Install these pinned tools on the machine running the installer (versions from [`hack/versions.env`](../hack/versions.env)):

- `kubectl` **v1.37.0** with cluster-admin access to the target cluster context
- `helm` **v4.3.2**
- `uv` **0.13.8** (manages Python **3.14.10** for Keycloak realm and authorization provisioning)
- `openssl`

## Quick start

### Preview the installation plan (`--dry-run`)

Render all manifests, per-namespace RBAC objects, ConfigMaps, and the `pagerless-server` deployment patch without contacting a cluster:

```bash
bash hack/install.sh \
  --dry-run \
  --context my-cluster \
  --namespaces payments,checkout \
  --provider anthropic
```

### Install in live-only mode (Anthropic API)

```bash
export ANTHROPIC_API_KEY="sk-ant-..."

bash hack/install.sh \
  --context my-cluster \
  --namespaces payments,checkout \
  --provider anthropic \
  --yes
```

### Install in live-only mode (Amazon Bedrock)

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

### Install with GitOps and Argo CD

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

Optional credentials read from the environment or `.env` (never passed on a command line or printed):

- `ANTHROPIC_API_KEY`: required when `--provider anthropic`.
- `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`: used when `--provider bedrock` (credentials are optional when EKS Pod Identity is bound to `pagerless/pagerless-agent`).
- `GITHUB_READ_TOKEN` and `GITHUB_WRITE_TOKEN`: fine-grained tokens for the repository passed to `--sandbox-url` ([ADR-0015](adr/0015-fine-grained-tokens-for-github.md)).
- `GRAFANA_SERVICE_ACCOUNT_TOKEN`: Viewer service account token for `mcp-grafana`.
- `ARGOCD_AUTH_TOKEN`: read-only Argo CD API token for `mcp-argocd` and post-merge Verification.

## What the installer provisions

1. **Namespaces and CustomResourceDefinitions:** creates `pagerless`, `keycloak`, `agentgateway-system`, and every namespace listed in `--namespaces`, then applies the `Triage` and `TriageApproval` CRDs (`deploy/pagerless-server/crds/`) and the pinned Gateway API `v1.6.3` CRDs (`deploy/gateway-api-crds/standard-install.yaml`).
2. **Keycloak (`deploy/keycloak`):** deploys PostgreSQL `17.12-alpine` and Keycloak `26.8.2`, generates and preserves `Secret keycloak/keycloak-credentials`, and runs `hack/provision-keycloak.py` to configure the `pagerless` realm, the `pagerless-approvers` group, the federated `pagerless-agent` client, the `pagerless-dashboard` OIDC client, and `Secret pagerless/pagerless-keycloak-dashboard`.
3. **agentgateway (`v1.7.0`) and `deploy/gateway`:** installs the `agentgateway-crds` and `agentgateway` OCI charts and applies the `Gateway`, `AgentgatewayBackend`, `HTTPRoute`, and `AgentgatewayPolicy` resources that enforce Keycloak JWT authentication, tool allowlists, and `Secret` resource denials ([ADR-0022](adr/0022-agentgateway-is-the-only-mcp-caller.md)).
4. **Secrets and ConfigMaps:** idempotently creates `pagerless-alertmanager`, `github-read`, `github-write`, `argocd-read`, `mcp-backend-tokens`, `mcp-client-tokens` (in both `pagerless` and `agentgateway-system`), `pagerless-agent-credentials`, `ConfigMap pagerless-sandbox`, and `ConfigMap pagerless-agent-config` (including `PAGERLESS_MCP_SERVERS` and `PAGERLESS_ALLOWED_NAMESPACES`).
5. **Read-only MCP servers (`deploy/mcp`) and per-namespace RBAC:** deploys `mcp-kubernetes`, `mcp-grafana`, `mcp-argocd`, and `mcp-github` in `pagerless`, and binds three least-privilege `Role`s in every namespace listed in `--namespaces`:
   - `pagerless-mcp-read` (`ServiceAccount pagerless/mcp-kubernetes`): read-only access to Pods, pod logs, Events, Deployments, StatefulSets, ReplicaSets, and Chaos Mesh `StressChaos`.
   - `pagerless-server-validation` (`ServiceAccount pagerless/pagerless-server`): `get`, `list`, and `update` on Deployments and StatefulSets (restricted to server-side dry runs by the `pagerless-server-dry-run-only` `ValidatingAdmissionPolicy`) plus `get` and `list` on Pods.
   - `pagerless-executor-restart` (`ServiceAccount pagerless/pagerless-executor`): `get`, `list`, `watch`, and `delete` on Pods plus `get`, `list`, and `watch` on Deployments and StatefulSets for live `RestartWorkload` execution.
6. **Control plane and Executor (`deploy/pagerless-server`, `deploy/pagerless-executor`):** applies the kustomizations, patches `deployment/pagerless-server` with `--workload-namespaces`, `--alert-namespaces`, `--patch-namespaces`, `--argocd-application`, and `--sandbox-url`, waits for both Deployments to roll out, and binds `Role pagerless-approver` to the current explicit Kubernetes user identity when available.

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
