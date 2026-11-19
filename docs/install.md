# Installing Pagerless

Pagerless installs into one namespace of a Kubernetes cluster you already run.
Its prerequisites are a cluster and one Secret. It needs no database, identity
provider, message broker, gateway, or admission webhook, and its cluster identity
is read-only: every remediation is a pull request.

## Prerequisites

| Prerequisite | Why it is needed |
| --- | --- |
| Kubernetes 1.30 or later | The chart uses a CEL validation rule on the Page CRD. |
| Helm 3.14 or later | The chart is an `apiVersion: v2` application chart. |
| A default StorageClass, or `persistence.storageClassName` | The append-only Page history lives on the `pagerless-data` claim. |
| One Secret | The chart creates `pagerless-credentials` itself, or names one you manage; see [Credentials](#credentials). |

## Install from the registry

```bash
helm install pagerless oci://ghcr.io/a-hagi613/charts/pagerless \
  --namespace pagerless --create-namespace --version 2.0.0
```

Or from a checkout of the matching release:

```bash
helm upgrade --install pagerless ./charts/pagerless \
  --namespace pagerless --create-namespace
```

Helm installs the CRDs in `charts/pagerless/crds/` only when the release is
created. Helm never upgrades a chart's `crds/` directory, so apply the CRD
explicitly after a chart upgrade:

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
[ADR-0038](adr/0038-one-watch-mode-or-an-explicit-namespace-allowlist.md).

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
Deployment mounts the named one. The Secret must exist before the pod starts.
[`docs/onboarding.md`](onboarding.md) covers each way of filling it, including
finishing an installation that started without credentials.

## Verify the installation

```bash
kubectl -n pagerless rollout status deploy/pagerless-server
kubectl -n pagerless get deploy,svc,pvc,configmap,sa
kubectl auth can-i get pods/log -n shop --as=system:serviceaccount:pagerless:pagerless-server   # yes
kubectl auth can-i update deployments -n shop --as=system:serviceaccount:pagerless:pagerless-server  # no
```

A healthy installation renders one Deployment, one Service, one
PersistentVolumeClaim, two ConfigMaps, the two ServiceAccounts, and the read
identity. It renders no Keycloak, gateway, database, or MCP server, and no
admission webhook.

`make verify-chart` runs the whole matrix on a throwaway kind cluster, builds the
server image, and deletes the cluster afterwards.

## Install the plugin

Install the plugin from the Krew index, or from Homebrew:

```bash
kubectl krew install page
brew install a-hagi613/tap/kubectl-page
```

The release also publishes the rendered manifest as the `page.yaml` asset. A
checkout carries the template that the release renders, because a manifest
cannot carry checksums before the archives exist; render it from a release's
checksum file and install it without touching the index:

```bash
gh release download v2.0.0 --pattern checksums.txt
scripts/release/render-krew-manifest.sh --checksums checksums.txt --version 2.0.0 --output /tmp/page.yaml
kubectl krew install --manifest=/tmp/page.yaml
```

The release job rewrites `plugins/page.yaml` to the rendered manifest and opens
a Pull Request when the copy differs, so a checkout of a later release can
install `--manifest=plugins/page.yaml` directly. The lint rejects a manifest
that still contains a template placeholder.

The plugin reads Pages through the Kubernetes API, so it needs only a kubeconfig
that can `get`, `list`, and watch `pages.pagerless.dev`. A user who decides
Proposals needs `patch` on `pages`; `Role pagerless-approver` grants exactly that.

## Uninstall

```bash
helm uninstall pagerless --namespace pagerless
kubectl delete namespace pagerless
kubectl delete crd pages.pagerless.dev
```

Uninstalling the release removes the credentials Secret the chart created. An
external secret manager keeps whatever it owns, because the chart never rendered
that Secret.

## Values

[`charts/pagerless/README.md`](../charts/pagerless/README.md) documents the
values, the rendered object inventory, and the read-only grant.
