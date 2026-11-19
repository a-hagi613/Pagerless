# Installing Pagerless

Pagerless installs into a Kubernetes cluster you already run, as an operator, an
investigation agent, an embedded web dashboard, and a CLI plugin. It does not
install a cluster's platform: an OIDC issuer, an MCP gateway, Prometheus,
Grafana, Argo CD, and the model endpoint are services you provide, and the chart
references them through values.

Two paths install the same release: the Helm chart (the supported artifact) and
`hack/install.sh`, which applies the CRDs, writes the credentials Secret, and
then runs `helm upgrade --install` with values derived from its arguments.

## Prerequisites

| Prerequisite | Why the chart needs it | Set with |
| --- | --- | --- |
| An OIDC issuer | The dashboard signs users in with the authorization-code flow plus PKCE. | `oidc.issuer`, `oidc.dashboardClientId`, `oidc.secretName` |
| An MCP gateway | The agent reaches the read-only `k8s`, `grafana`, `argocd`, and `github` tools through one base URL. | `mcp.gatewayUrl`, `mcp.servers` |
| Inference credentials | The agent calls the selected model provider. | Secret `pagerless-agent-credentials`, `agent.provider` |
| A target Git repository | Only pull-request Actions need it; without one the live-only modes are `NoAction`. | `sandbox.url`, `sandbox.readSecretName`, `sandbox.writeSecretName` |
| Argo CD | Verification asks whether the merge commit is synced and healthy. | `argocd.url`, `argocd.application`, `argocd.secretName` |
| Alertmanager | An opted-in alert can start a Triage. | `alertmanager.url`, `alertmanager.secretName` |

Every credential arrives as a Secret reference, never as a plain `--set` value.
`secrets.createPlaceholders=true` renders empty optional integration Secrets so a
trial can start before GitHub or Argo CD credentials exist.

## Install with the chart

```bash
helm upgrade --install pagerless ./charts/pagerless \
  --namespace pagerless --create-namespace \
  --values my-values.yaml
```

Helm installs the CRDs in `charts/pagerless/crds/` only when the release is
created, so apply them explicitly after a chart upgrade:

```bash
kubectl apply -f charts/pagerless/crds/pagerless.dev_triages.yaml
kubectl apply -f charts/pagerless/crds/pagerless.dev_triageapprovals.yaml
```

`charts/pagerless/README.md` documents every value.

## Install with hack/install.sh

```bash
# Preview the rendered plan and manifests without contacting a cluster.
bash hack/install.sh --dry-run \
  --context my-cluster \
  --namespaces payments,checkout \
  --provider anthropic

# Install, creating the credentials Secrets from the environment.
export ANTHROPIC_API_KEY="sk-ant-..."
bash hack/install.sh \
  --context my-cluster \
  --namespaces payments,checkout \
  --provider anthropic \
  --yes
```

The script:

1. creates the release namespace and applies the `Triage` and `TriageApproval`
   CRDs from `charts/pagerless/crds/`;
2. writes `Secret pagerless-agent-credentials` from the selected provider's
   environment variables, and `Secret github-read` and `Secret github-write`
   when `--sandbox-url` is set;
3. runs `helm upgrade --install` with the values it derives, then waits for the
   server Deployment to roll out.

| Flag | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `--context <ctx>` | `PAGERLESS_KUBE_CONTEXT` | current context | Kubernetes context to install into. |
| `--namespaces <csv>` | `WORKLOAD_NAMESPACES` | `default` | Workload namespaces to watch and propose changes for. |
| `--provider <name>` | `MODEL_PROVIDER` | inferred | `anthropic`, `bedrock`, or `replay`. |
| `--sandbox-url <url>` | `SANDBOX_URL` | empty | Target repository HTTPS URL; omit for live-only mode. |
| `--argocd-app <app>` | `ARGOCD_APPLICATION` | empty | Argo CD Application that deploys the Workloads. |

## What the chart installs

- `ServiceAccount`s `pagerless-server` and `pagerless-agent` (the agent receives
  no automatic API token).
- The release-namespace `Role` and `RoleBinding` for the server, and its
  `ClusterRole` grants (TokenReview, webhook `caBundle` publication, Node reads).
- `Role pagerless-approver` and, when `approvers.subjects` is set, its binding.
- `Role pagerless-server-validation` and its binding in every Workload
  namespace.
- The `pagerless-server-agent-jobs` and `pagerless-server-dry-run-only`
  admission policies and their bindings.
- The mutating and validating webhook configurations for `TriageApproval` and
  `Triage` creation.
- `ConfigMap pagerless-sandbox`, `ConfigMap pagerless-agent-config`, the
  `pagerless-history` claim, the `pagerless-server` Deployment and Service, and
  the `pagerless` Grafana dashboard.

## First Triage

```bash
kubectl --context my-cluster -n pagerless port-forward svc/pagerless-server 8080:8082
kubectl triage --context my-cluster --namespace payments deploy/my-service --hint "check recent restarts"
kubectl triage --context my-cluster list
kubectl triage --context my-cluster show <triage> --watch
kubectl triage --context my-cluster approve <triage>
```

## Verification of the installation

```bash
helm lint charts/pagerless
helm template pagerless charts/pagerless --namespace pagerless | kubectl apply --dry-run=server -f -
```

`make validate` renders the chart in its live-only and repository-configured
modes and checks every object with the pinned `kubeconform`.

## Uninstall

```bash
helm uninstall pagerless --namespace pagerless
kubectl delete -f charts/pagerless/crds/pagerless.dev_triages.yaml
kubectl delete -f charts/pagerless/crds/pagerless.dev_triageapprovals.yaml
```

The CRDs are applied outside the release so that an upgrade never deletes the
Triages they hold.
