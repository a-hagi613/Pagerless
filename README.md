<p align="center">
  <img src="docs/assets/pagerless-icon.png" alt="Pagerless" width="96" height="96">
</p>

<h1 align="center">Pagerless</h1>

<p align="center">
  <strong>A standalone, read-only Kubernetes remediation operator that proposes every change as a pull request and never writes to your cluster.</strong>
</p>

Pagerless watches a Kubernetes cluster for failing **Targets**, diagnoses them
with an AI agent, and opens a **Pull Request** against the Git repository that
owns the Target. A human approves one specific **Proposal** hash; the repository's
own CI decides whether it may merge. The cluster identity is read-only, so a
compromised model, a wrong diagnosis, or a bug in Pagerless cannot change a
workload directly.

## Status: v2.0 in progress

This tree is the v2.0 product. It is a visible break from v1.0:

- **Landed in this release:** the `Page` API (`pages.pagerless.dev/v1alpha1`),
  the strictly read-only cluster identity, the standalone single-namespace Helm
  chart shell with zero admission webhooks, the decision-on-the-Page approval
  path, the operator's glossary and core ADRs, and the `kubectl page` CLI
  (including `kubectl page version`).
- **Being delivered by the remaining v2.0 slices:** the dual-API error watcher
  and cooldowns, read-only observability connectors and alert webhooks, the
  OpenAI-compatible agent and tool broker, the GitOps provenance and pull
  request engine, the auth and onboarding flow, the Variant C Portal, the release
  channel, and the `pagerless-sandbox` showcase.

Until those slices land, an installation runs the API, the reconcilers, and the
report and metrics endpoints; it does not yet watch failures or serve the Portal.
`Triage` and `TriageApproval` are removed with no alias and no migration:
v1 objects are not converted. The upgrade guide is part of the release slice.

## Install

```bash
helm install pagerless oci://ghcr.io/a-hagi613/charts/pagerless \
  --namespace pagerless --create-namespace
```

The chart installs one `pagerless-server` Deployment, one Service, one
`pagerless-data` PersistentVolumeClaim, two ConfigMaps, a ServiceAccount, an
empty `pagerless-credentials` Secret, and exactly one read identity. It installs
no database, identity provider, message broker, gateway, or admission webhook.

`helm install --set credentials.existingSecret=…` keeps an external secret
manager as the only writer of the credentials Secret.

## The read-only boundary

The rendered grant is compared against a checked-in allowlist on every pull
request, so widening it is a reviewed act:

- `get`/`list`/`watch` on pods, services, endpoints, endpoint slices,
  ConfigMaps, PVCs, both Event APIs, `apps` workloads including replica sets,
  `batch` jobs and cron jobs, ingresses, nodes, and namespaces; `get` on
  `pods/log`; `get` on `/openapi/v3` for schema validation.
- The product's own `pages` and `pages/status`, and the agent Jobs, in the
  release namespace.
- `get` on two Secrets by name, and `update`/`patch` on the credentials Secret
  only.

There is no `update`, `patch`, `create`, or `delete` on any workload, Event, or
Node; no `pods/exec`, `pods/attach`, `pods/portforward`, or
`pods/ephemeralcontainers`; no `serviceaccounts/token`; no `impersonate`,
`escalate`, or `bind`; no `*/scale`; and no Secret read outside the two named
Secrets. The evidence is [ADR-0037](docs/adr/0037-read-only-cluster-identity.md),
a chart test that fails when a rule widens, an envtest authorization matrix, and
`make verify-chart`, which installs the chart on a throwaway kind cluster and
runs `kubectl auth can-i` as the server's ServiceAccount.

## Command line

`cmd/kubectl-page` builds `kubectl-page`, which kubectl resolves as
`kubectl page`:

```console
kubectl page deploy/my-service --namespace payments    # open a Page
kubectl page list
kubectl page show <name>
kubectl page approve <name>                            # bind the displayed hash
kubectl page dismiss <name> --reason "<why>"
kubectl page version                                   # version, commit, expected chart
```

A decision is an annotation patch on the Page, so it needs only `patch` on
`pages`; `Role pagerless-approver` grants exactly that. `spec` is immutable and
`status` is a separate subresource, so a decision cannot change the investigation.
See [ADR-0031](docs/adr/0031-approval-is-a-decision-on-the-page.md).

## Repository layout

```
api/         Page CRD types (kubebuilder)
cmd/         pagerless-server and kubectl-page
internal/    Go packages: reconcilers, CLI, proposal validation, history, metrics
agent/       Python investigation agent (uv)
dashboard/   React Portal served by pagerless-server
charts/      The standalone Helm chart
hack/        The development and verification scripts
docs/        ADRs, installation, and operations documentation
```

The demo workloads, Terraform, and the GitOps showcase live in
`a-hagi613/pagerless-sandbox`.

## Development

```bash
make generate manifests   # deepcopy and the CRDs in charts/pagerless/crds
make build-cli            # bin/kubectl-page
make lint-go test-go      # Go lint, unit, envtest, and chart tests
make lint-agent test-agent
make dashboard-check dashboard-build
make validate             # helm lint, render both watch modes, kubeconform
make verify-chart         # install on a throwaway kind cluster and prove the grant
```

`make test-go` needs the envtest binaries, which `Makefile` downloads through the
pinned `setup-envtest`. `make verify-chart` needs Docker, kind, Helm, and kubectl,
and deletes its cluster when it finishes.

## Documentation

- [GLOSSARY.md](GLOSSARY.md) — the product's vocabulary, used exactly.
- [docs/adr/](docs/adr/README.md) — the decisions that constrain the design.
- [docs/install.md](docs/install.md) — installing and configuring the chart.
- [charts/pagerless/README.md](charts/pagerless/README.md) — every chart value.

## License

[Apache-2.0](LICENSE)
