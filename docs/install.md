# Installing Pagerless

Pagerless installs into one namespace of a Kubernetes cluster you already run.
It needs no database, identity provider, message broker, gateway, or admission
webhook, and its cluster identity is read-only: every remediation is a pull
request.

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
render with a message. Allowlist mode drops the cluster-scoped read rules: there
is no node or namespace read and no `/openapi/v3` validation. A separate
ClusterRole and ClusterRoleBinding in every watch mode still grant `create` on
`authentication.k8s.io/tokenreviews`; that authenticates the per-Page agent
token and grants no access to any cluster object. See
[ADR-0038](adr/0038-one-watch-mode-or-an-explicit-namespace-allowlist.md) and
[ADR-0037](adr/0037-read-only-cluster-identity.md).

## Authentication

The chart requires no identity provider. In the default `auth.mode: token` it
generates two Secrets:

- `pagerless-admin-secret` holds a 48-character bootstrap token. The release
  notes print the one command that decodes it; the operator signs in at
  `/login`, then deletes the Secret as the documented hardening step. Existing
  browser sessions keep working.
- `pagerless-session-key` signs the session cookies. Rotating it signs every
  session out.

The session key is always kept across `helm uninstall`; the token Secret is kept
unless `auth.adminSecret.keep=false`. Set `auth.adminSecret.create=false` in
GitOps to supply the token out of band.

To authenticate with a reverse proxy instead, set `auth.mode: disabled` and
`auth.proxy.identityHeader`. The chart refuses to render that mode with an
Ingress and no header. [ADR-0032](adr/0032-single-operator-local-authentication.md)
records the design and [`docs/auth.md`](auth.md) documents the flags.

## Credentials

The chart pre-creates `pagerless-credentials` with every fixed key present and
empty, plus the four webhook tokens, so an unconfigured installation is a normal
state rather than a missing-file error:

```
AI_BASE_URL  AI_API_KEY  AI_MODEL  GITHUB_TOKEN  GITHUB_REPO  GITHUB_API_URL
ALERTMANAGER_WEBHOOK_TOKEN  GRAFANA_WEBHOOK_SECRET
DATADOG_WEBHOOK_TOKEN  GENERIC_WEBHOOK_TOKEN
```

Connector tokens are not pre-created: write `CONNECTOR_<UPPER_NAME>_TOKEN` for
each configured Connector.

Finish onboarding at `/settings` in the Portal, or write the same keys from a
terminal with `kubectl page config set ai` and `kubectl page config set github`.
Both surfaces read, merge, and write the same Secret
([ADR-0033](adr/0033-one-credentials-secret.md)). A secret manager can remain the
only writer:

```bash
helm upgrade --install pagerless ./charts/pagerless -n pagerless \
  --set credentials.existingSecret=team-credentials
```

With `credentials.existingSecret` set, the chart renders no credentials Secret
at all and the Deployment mounts the named one. The Secret must exist before the
pod starts, and the Portal and the CLI report the settings as read-only.
[`docs/onboarding.md`](onboarding.md) covers each way of filling it, including
finishing an installation that started without credentials.

## Target Repository

The chart resolves the Target Repository from `GITHUB_REPO` in that Secret and
uses `GITHUB_API_URL` to select a GitHub Enterprise host; otherwise it uses
`https://github.com`. The server re-reads the Secret when a Page starts and when
an approved Proposal is published, so onboarding completes the path without a
restart. An air-gapped installation with no repository sets
`targetRepository.enabled=false`; it then diagnoses Pages and records decisions
without opening a pull request.

## Verify the installation

```bash
kubectl -n pagerless rollout status deploy/pagerless-server
kubectl -n pagerless get deploy,svc,pvc,configmap,sa
kubectl auth can-i get pods/log -n shop --as=system:serviceaccount:pagerless:pagerless-server   # yes
kubectl auth can-i update deployments -n shop --as=system:serviceaccount:pagerless:pagerless-server  # no
```

A healthy installation renders one Deployment, one Service, one
PersistentVolumeClaim, two ConfigMaps, two ServiceAccounts, one
NetworkPolicy, the `pages.pagerless.dev` CRD, the credentials Secret (omitted
with `credentials.existingSecret`) and, in token mode, the two authentication
Secrets, the read identity, and the agent-auth ClusterRole and
ClusterRoleBinding that grant only `create` on `tokenreviews`. It renders no
identity provider, gateway, database, or MCP server, and no admission webhook.

`make verify-chart` runs the whole matrix on a throwaway kind cluster, builds the
server image, and deletes the cluster afterwards.

## Install the plugin

The release publishes the rendered manifest as the `page.yaml` asset, so install
that asset directly as soon as the release is published:

```bash
curl -fLO https://github.com/a-hagi613/pagerless/releases/download/v2.0.0/page.yaml
kubectl krew install --manifest=page.yaml
```

`kubectl krew install page` starts working when the release's pull request
against the Krew index is accepted upstream, and
`brew install a-hagi613/tap/kubectl-page` when the release's formula pull
request to `a-hagi613/homebrew-tap` is merged:

```bash
kubectl krew install page
brew install a-hagi613/tap/kubectl-page
```

A checkout carries `plugins/page.yaml` as a template, because a manifest cannot
carry checksums before the archives exist. Render it from a release's checksum
file and install the result without touching the index:

```bash
gh release download v2.0.0 --pattern checksums.txt
scripts/release/render-krew-manifest.sh --checksums checksums.txt --version 2.0.0 --output /tmp/page.yaml
kubectl krew install --manifest=/tmp/page.yaml
```

The release job rewrites `plugins/page.yaml` to the rendered manifest and opens
a Pull Request when the copy differs; once that refresh Pull Request is merged,
a checkout can install `--manifest=plugins/page.yaml` directly. The lint rejects
a manifest that still contains a template placeholder.

The plugin reads Pages through the Kubernetes API, so it needs only a kubeconfig
that can `get`, `list`, and watch `pages.pagerless.dev`. A user who decides
Proposals needs `patch` on `pages`; `Role pagerless-approver` grants exactly that.

## Uninstall

```bash
helm uninstall pagerless --namespace pagerless
kubectl delete namespace pagerless
kubectl delete crd pages.pagerless.dev
```

Uninstalling the release removes the credentials Secret the chart created. The
session key always carries `helm.sh/resource-policy: keep`, and the bootstrap
token Secret carries it unless `auth.adminSecret.keep=false`, so both survive
until the namespace is deleted or keep is disabled. An external secret manager
keeps whatever it owns, because the chart never rendered that Secret.

## Values

[`charts/pagerless/README.md`](../charts/pagerless/README.md) documents the
values, the rendered object inventory, and the read-only grant.
