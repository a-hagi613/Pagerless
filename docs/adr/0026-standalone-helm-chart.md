# ADR-0026: A standalone Helm chart installs Pagerless without the reference Sandbox stack

- **Status:** Accepted
- **Date:** 2026-11-18
- **Relates to:** [ADR-0003](0003-separate-sandbox-repository.md), [ADR-0005](0005-two-go-binaries.md), [ADR-0019](0019-opentelemetry-demo-as-rendered-manifests.md), [ADR-0022](0022-agentgateway-is-the-only-mcp-caller.md)

## Context

Until now the only way to install Pagerless was `hack/install.sh`. It applied the kustomize bases in `deploy/pagerless-server` and `deploy/pagerless-executor`, then replaced the server container's argument list with a JSON patch to inject the operator's namespaces, Sandbox URL, MCP servers, and Grafana URL. The patch duplicated every flag that the base already set, and nothing verified that the two stayed in step: a flag added to the Deployment without being added to the patch was silently dropped on install, and a flag the patch set without a default in the binary failed only when the pod started.

The reference environment has the same shape. `deploy/apps` is an Argo CD app-of-apps that renders the platform charts and the Pagerless bases together with the OpenTelemetry shopping-cart demo, Chaos Mesh, Argo CD, Keycloak, and agentgateway. An operator who wants Pagerless against their own Workloads must therefore either copy manifests out of the repository or accept services they did not ask for. ADR-0003 already puts the demo Workloads in a separate repository; the installer described that separation, but there was no installable artifact that expressed it.

A cluster install also has a lifecycle problem. The objects the installer creates are clusters to be upgraded, not one-off resources: CRDs, admission policies, webhook configurations, and cluster-scoped RBAC are created once and must evolve with the release. Rendering them from a kustomize base and patching a Deployment afterwards leaves no record of which release created them and no upgrade transaction.

## Decision

`charts/pagerless` is the first-class installation unit for the Pagerless control plane. It is a plain Helm chart, not an Argo CD Application, with no dependency on the Sandbox, Chaos Mesh, or the app-of-apps stack.

- The chart renders the CRDs, ServiceAccounts, release-namespace and per-namespace RBAC, both admission policies, the webhook configurations, the two ConfigMaps, the history claim, the server and Executor Services and Deployments, and the bundled read-only MCP servers. Every container flag is a value rendered into the Deployment's argument list, so no post-render patch exists.
- The CRDs live in `charts/pagerless/crds/` as byte-identical copies of `deploy/pagerless-server/crds/`. `make manifests` writes both copies, and `make validate` fails when they differ.
- Cluster-scoped names stay fixed (`pagerless-server`, `pagerless-executor`, `pagerless-server-dry-run-only`, `pagerless-server-agent-jobs`) because the admission policies, webhook configurations, and RBAC refer to each other by name. Only the namespace is a release parameter.
- `hack/install.sh` keeps its job of provisioning the platform services that outlive a Pagerless release: Keycloak and its realm, agentgateway and the gateway routes, and the integration Secrets. It then runs `helm upgrade --install` with a private temporary values file, applies `charts/pagerless/crds/` directly, and skips chart CRD installation so that removing the release does not remove the CRDs.
- `hack/validate-manifests.sh` lints the chart and validates three renderings through kubeconform: the default live-only mode, GitOps mode with placeholder Secrets, and the installer's own `--dry-run` output.
- The release workflow publishes the packaged chart to `oci://ghcr.io/a-hagi613/charts` at the tag's version, after the Go, agent, and manifest jobs succeed. `hack/install.sh` and the documentation still install from the checkout.
- The existing `deploy/` kustomize bases remain, unchanged in behaviour, because `make up`, `make cloud-up`, `make e2e`, and the Argo CD reference stack consume them.

## Consequences

- One source of truth for the server and Executor flags. A new flag is added to the chart, the binary, and the kustomize base; the installer no longer needs a matching JSON patch, and a missing value shows up in the rendered chart rather than in a running pod.
- Two copies of each CRD exist. That is deliberate: a standalone install must not need the kustomize bases, and `make manifests` plus the `cmp` check make drift a validation failure rather than a review responsibility.
- Helm owns every object it creates. A cluster bootstrapped by an earlier installer holds those objects outside Helm ownership, so an upgrade requires removing the old bases once; `docs/install.md` documents that step. Deleting the release removes the Deployments, RBAC, policies, and webhooks but not the CRDs or the `Triages` they describe.
- The chart does not install Keycloak, agentgateway, or the integration Secrets, because those are platform services or operator credentials with their own lifecycle. A standalone install is therefore not a single command unless the operator supplies them; `hack/install.sh` remains the path that does.
- The chart's namespace is effectively `pagerless` unless the operator also changes the gateway manifests, which address `mcp-*.pagerless.svc` and read `mcp-client-tokens` from that namespace. Defaulting the chart's values to that namespace keeps the two consistent, and the README states the constraint.
- The published OCI chart and the repository's `deploy/` stack are two supported artifacts. A change that only updates one of them fails `make validate` (chart) or the reference end-to-end suite (kustomize), so both stay live.
