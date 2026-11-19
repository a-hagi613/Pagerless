# Installing Pagerless

Pagerless installs into one namespace of a Kubernetes cluster you already run.
The chart is standalone: it needs no database, identity provider, message broker,
gateway, or admission webhook. Its cluster identity is read-only, and every
remediation is a pull request.

## Prerequisites

| Prerequisite | Why it is needed |
| --- | --- |
| Kubernetes 1.30 or later | The chart uses a CEL validation rule on the Page CRD. |
| Helm 3.14 or later | The chart is an `apiVersion: v2` application chart. |
| A default StorageClass, or `persistence.storageClassName` | The append-only Page history lives on the `pagerless-data` claim. |

No other service is required. The remaining v2.0 slices add the connectors, the
agent endpoint, and the Portal, which point at services you already run; none of
them is installed by this chart.

## Install with Helm

```bash
helm install pagerless oci://ghcr.io/a-hagi613/charts/pagerless \
  --namespace pagerless --create-namespace
```

Or from a checkout:

```bash
helm upgrade --install pagerless ./charts/pagerless \
  --namespace pagerless --create-namespace
```

Helm installs the CRDs in `charts/pagerless/crds/` only when the release is
created. Apply them explicitly after a chart upgrade, because Helm never upgrades
a chart's `crds/` directory:

```bash
kubectl apply -f charts/pagerless/crds/pagerless.dev_pages.yaml
```

## Watch mode

The chart renders exactly one of two mutually exclusive read modes:

```bash
# Every namespace (default): one read ClusterRole and ClusterRoleBinding.
helm upgrade --install pagerless ./charts/pagerless -n pagerless

# An explicit allowlist: one read Role and RoleBinding per namespace.
helm upgrade --install pagerless ./charts/pagerless -n pagerless \
  --set rbac.watchAllNamespaces=false \
  --set 'rbac.watchNamespaces={shop,payments}'
```

Setting both, or setting neither an allowlist nor cluster-wide mode, fails the
render with a message. Allowlist mode drops the cluster-scoped rules: there is no
node or namespace read, no `/openapi/v3` validation, and no `tokenreviews`.
Agent authentication therefore needs cluster-wide mode, or an operator-granted
`create` on `authentication.k8s.io/tokenreviews`. See
[ADR-0038](../docs/adr/0038-one-watch-mode-or-an-explicit-namespace-allowlist.md).

## Credentials

The chart pre-creates `pagerless-credentials` with every documented key present
and empty, so an unconfigured installation is a normal state rather than a
missing-file error:

```
AI_BASE_URL  AI_API_KEY  AI_MODEL  GITHUB_TOKEN  GITHUB_REPO  GITHUB_API_URL
```

Fill it with `kubectl -n pagerless edit secret pagerless-credentials`, or keep an
external secret manager as the only writer:

```bash
helm upgrade --install pagerless ./charts/pagerless -n pagerless \
  --set credentials.existingSecret=team-credentials
```

With `credentials.existingSecret` set, the chart renders no Secret at all and the
Deployment mounts the named one.

## Install with hack/install.sh

```bash
# Render the plan and the manifests without contacting a cluster.
bash hack/install.sh --dry-run --namespaces shop,payments

# Install after filling the credentials Secret.
kubectl -n pagerless create secret generic pagerless-credentials --from-env-file=./pagerless.env
bash hack/install.sh --context my-cluster --namespaces shop,payments
```

## Verify the release

```bash
kubectl -n pagerless rollout status deploy/pagerless-server
kubectl auth can-i get pods/log -n shop --as=system:serviceaccount:pagerless:pagerless-server  # yes
kubectl auth can-i update deployments -n shop --as=system:serviceaccount:pagerless:pagerless-server  # no
```

`make verify-chart` runs the whole matrix on a throwaway kind cluster, builds the
server image, and deletes the cluster afterwards.

## Values

[`charts/pagerless/README.md`](../charts/pagerless/README.md) documents every
value, the rendered object inventory, and the read-only grant.
