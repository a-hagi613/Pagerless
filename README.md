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

## Quick start

Install the operator into one namespace of a cluster you already run:

```bash
helm install pagerless oci://ghcr.io/a-hagi613/charts/pagerless \
  --namespace pagerless --create-namespace --version 2.0.0
```

Install the `kubectl page` plugin with Krew, or with Homebrew on macOS or Linux:

```bash
kubectl krew install page
brew install a-hagi613/tap/kubectl-page
```

Then check the installation and open your first Page:

```console
kubectl page version
kubectl page new deploy/checkout --namespace shop
kubectl page list
```

The chart installs one `pagerless-server` Deployment, one Service, one
`pagerless-data` PersistentVolumeClaim, two ConfigMaps, a ServiceAccount, an
empty `pagerless-credentials` Secret, the bootstrap token and session key
Secrets, and exactly one read identity. It installs no database, identity
provider, message broker, gateway, or admission webhook.

The release notes print the sign-in steps: port-forward the Portal, decode the
bootstrap token, open `/settings`, and finish onboarding. The Portal writes the
same credentials Secret that `kubectl page config` writes from a terminal, and
`helm install --set credentials.existingSecret=…` keeps an external secret
manager as the only writer: both surfaces then report the settings as read-only.

[`docs/install.md`](docs/install.md) covers the watch modes and the credentials
Secret; [`docs/onboarding.md`](docs/onboarding.md) finishes an installation that
starts without credentials.

## The read-only boundary

The rendered grant is compared against a checked-in allowlist on every pull
request, so widening it is a reviewed act:

- `get`/`list`/`watch` on pods, services, endpoints, endpoint slices,
  ConfigMaps, PVCs, both Event APIs, `apps` workloads including replica sets,
  `batch` jobs and cron jobs, ingresses, nodes, and namespaces; `get` on
  `pods/log`; `get` on `/openapi/v3` for schema validation.
- The product's own `pages` and `pages/status`, and the agent Jobs, in the
  release namespace.
- `get`, `update`, and `patch` on the credentials Secret only, by
  `resourceNames`. The bootstrap token and session key are read from their
  mounted volumes, so they need no API grant.

There is no `update`, `patch`, `create`, or `delete` on any workload, Event, or
Node; no `pods/exec`, `pods/attach`, `pods/portforward`, or
`pods/ephemeralcontainers`; no `serviceaccounts/token`; no `impersonate`,
`escalate`, or `bind`; no `*/scale`; and no Secret read outside the credentials
Secret. The evidence is [ADR-0037](docs/adr/0037-read-only-cluster-identity.md),
a chart test that fails when a rule widens, an envtest authorization matrix, and
`make verify-chart`, which installs the chart on a throwaway kind cluster and
runs `kubectl auth can-i` as the server's ServiceAccount.

## Command line

`cmd/kubectl-page` builds `kubectl-page`, which kubectl resolves as
`kubectl page`:

```console
kubectl page new deploy/my-service -n payments   # open a Page for a Target
kubectl page list                                # the resource's printer columns
kubectl page get <name> -o yaml                  # the Page record
kubectl page diff <name>                         # the Proposal against its base
kubectl page approve <name>                      # bind the displayed hash
kubectl page reject <name> --reason "<why>"      # close it without a change
kubectl page config get                          # masked credentials and fingerprints
kubectl page config set ai                       # write the AI endpoint
kubectl page login --server https://portal       # store a session for server mode
kubectl page version                             # version, commit, expected chart

```

Commands use the kubeconfig by default and the Portal API with `--mode server`,
and every run reports which mode it used. No command accepts a secret value as a
flag: a credential is read from an interactive prompt without echo, standard
input, a file, or an env file. The exit codes are `0` success, `1` failure, `2`
invalid command line, `3` a write refused because a secret manager owns the
credentials Secret, and `4` a Proposal that changed before it was decided.

A decision is an annotation patch on the Page, so it needs only `patch` on
`pages`; `Role pagerless-approver` grants exactly that. `spec` is immutable and
`status` is a separate subresource, so a decision cannot change the investigation.
See [ADR-0031](docs/adr/0031-approval-is-a-decision-on-the-page.md) and
[`docs/cli.md`](docs/cli.md) for every command, flag, and exit code.

## Release status

This tree is the v2.0 product, and it is a visible break from v1.0:

- **Landed:** the `Page` API (`pages.pagerless.dev/v1alpha1`), the dual-API
  error watcher, canonical Target resolution, the SQLite Cooldown store, the
  strictly read-only cluster identity, the standalone single-namespace chart,
  the decision-on-the-Page approval path, the `kubectl page` CLI, and the
  release channel that publishes the plugin, the images, and the chart at one
  version.
- **Delivered by the remaining v2.0 slices:** the read-only observability
  connectors and alert webhooks, the OpenAI-compatible agent and tool broker,
  the GitOps provenance and pull request engine, the onboarding flow and Portal
  settings route, and the Variant C Portal.

Until those slices land, an installation watches for failures and opens Pages,
but it does not yet diagnose them or serve the Portal. `Triage` and
`TriageApproval` are removed with no alias and no migration: v1 objects are not
converted. [`docs/upgrade.md`](docs/upgrade.md) lists every removed value and the
replacement path.

## Repository layout

```
api/          Page CRD types (kubebuilder)
cmd/          pagerless-server and kubectl-page
internal/     Go packages: reconcilers, CLI, proposal validation, history, metrics
agent/        Python investigation agent (uv)
dashboard/    React Portal served by pagerless-server
charts/       The standalone Helm chart
plugins/      The Krew plugin manifest, rendered from the release checksums
scripts/      The release scripts
hack/         The development and verification scripts
docs/         ADRs, installation, and operations documentation
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
make release-check        # goreleaser check and the Krew manifest lint
make release-snapshot     # the release archives, checksums, SBOMs, and manifest
```

`make test-go` needs the envtest binaries, which the `Makefile` downloads through
the pinned `setup-envtest`. `make verify-chart` needs Docker, kind, Helm, and
kubectl, and deletes its cluster when it finishes. `make release-snapshot` needs
Go and a network connection for the pinned GoReleaser and Syft binaries; it needs
no tag, no registry credential, and no GitHub token.

## Documentation

- [GLOSSARY.md](GLOSSARY.md) — the product's vocabulary, used exactly.
- [docs/adr/](docs/adr/README.md) — the decisions that constrain the design.
- [docs/install.md](docs/install.md) — installing and configuring the chart.
- [docs/onboarding.md](docs/onboarding.md) — the paths from an installed chart to
  a working installation.
- [docs/auth.md](docs/auth.md) — Portal authentication, sessions, CSRF, and the
  threat model.
- [docs/cli.md](docs/cli.md) — every `kubectl page` command, flag, and exit code.
- [docs/portal.md](docs/portal.md) — the Portal routes and the parity contract.
- [docs/upgrade.md](docs/upgrade.md) — the v1 to v2 break, in full.
- [docs/release.md](docs/release.md) — how one tag publishes every artifact.
- [charts/pagerless/README.md](charts/pagerless/README.md) — every chart value.

## License

[Apache-2.0](LICENSE)
