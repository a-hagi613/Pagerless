# Pagerless

**An AI agent that diagnoses failing Kubernetes Workloads and remediates them only after a human approves the exact change.**

> **Project status:** Phase 1 is complete and was validated locally: a fresh kind run and CI both reached Recovered from an injected OOMKilled Workload, and `evals/replay/oom.json` is a genuine live recording with provenance. The `v0.1.0` runtime images are published to GHCR and publicly pullable. The dashboard's Triage list and detail views are implemented, and agentgateway with Keycloak now fronts every MCP tool call. Start Triage, dashboard sign-in and approvals, and the EKS deployment are later work.

## The Phase 1 loop

1. **Trigger.** An authenticated, opted-in Alertmanager delivery or a CLI request creates a Triage for one failing Deployment or StatefulSet. Repeated Triggers join its Active Triage.
2. **Context.** `pagerless-server` starts a short-lived agent Job. Deterministic Python code gathers the Context Bundle through read-only MCP tools, reached through agentgateway: Workload status, pods, events, recent logs, Argo CD status and recent Sandbox commits.
3. **Investigate.** The LangGraph agent uses Replay, the Anthropic API or Amazon Bedrock. Every MCP call carries a Keycloak token that agentgateway validates and authorizes by tool, and the agent posts step events and Findings to the server.
4. **Propose.** The agent submits a Diagnosis and one typed Proposal. Phase 1 implements PatchResources and NoAction. The server validates allowed namespaces, parameter bounds and the server-side dry run, then renders the exact Sandbox patch and computes its hash.
5. **Approve.** A human reviews the rendered change in the CLI and creates a TriageApproval bound to that hash. The admission webhooks stamp the authenticated Kubernetes identity and refuse a Stale Proposal, an expired Proposal or a mismatched hash.
6. **Execute.** The separate Executor independently checks the Approval and stored change before opening an auto-merging Sandbox pull request. Argo CD applies the merged commit. A NoAction Approval acknowledges the Diagnosis without changing the Workload.
7. **Verify.** Go checks the rollout, Argo CD health at the merge commit, restarts during the full five-minute observation window, and resolution of any triggering alert. The Triage ends Recovered or NotRecovered; NotRecovered starts a linked Follow-up Triage when no other Active Triage already covers the Workload.
8. **Narrate.** After Verification ends, `pagerless-server` starts one short-lived Narrative Job. It makes one Claude Haiku 5.5 call capped at about 2,000 output tokens and stores a short, purely descriptive account in the Triage's status. It never changes the outcome: a failed or timed-out Narrative Job leaves the Verification outcome and every other status field as they were.

The [Sandbox](https://github.com/a-hagi613/pagerless-sandbox) is a separate repository for demo Workload manifests. Failure injection and remediation pull requests change that repository, as specified in [ADR-0003](docs/adr/0003-separate-sandbox-repository.md).

## Implemented components

- **Control plane:** `pagerless-server` in Go, with the reconcilers, agent report endpoint, authenticated Alertmanager receiver and admission webhooks.
- **Execution:** `pagerless-executor` in Go, with the separate GitHub write token and idempotent pull request execution.
- **Agent:** Python 3.14, LangGraph, sequence-based Replay and Claude Haiku 5.5 providers for the Anthropic API and Bedrock, with prompt caching. A separate short-lived Narrative Job writes each Triage's account after Verification.
- **CLI:** the Go/Cobra `kubectl-triage` plugin, using the selected kube context.
- **Read-only MCP services:** kubernetes-mcp-server, mcp-grafana, mcp-for-argocd and GitHub MCP, fronted by agentgateway 1.7.0 and authenticated with Keycloak 26.8.2. Their images, accounts and access restrictions are documented in [deploy/mcp](deploy/mcp/README.md), [deploy/gateway](deploy/gateway/README.md) and [deploy/keycloak](deploy/keycloak/README.md).
- **Observability and GitOps:** Prometheus, Alertmanager, Loki, Tempo, Grafana, the OpenTelemetry Collector and Argo CD's app-of-apps deployment, with a provisioned Grafana dashboard for Pagerless's own behaviour.
- **History:** an append-only SQLite record of every Phase change and every step event, with terminal Triages deleted after seven days while their history remains.
- **Dashboard:** a Vite, React, TypeScript, Tailwind CSS and shadcn/ui application that `pagerless-server` serves from its binary, with a Triage list, a Triage detail view that streams live steps over Server-Sent Events, and embedded Grafana panels for the Workload's metrics and logs.
- **Development checks:** Go envtest integration tests, Python agent tests, a synthetic Replay kind harness and a tagged GHCR publication workflow.

The Start Triage view, dashboard sign-in and approvals, the remaining Actions and Scenarios, and the published evaluation suite are remaining Phase 2 work. Terraform for EKS, Karpenter, Pod Identity, cloud teardown and release measurements are Phase 3 work.

## History, retention and metrics

`pagerless-server` appends a snapshot of a Triage's status to SQLite on every Phase change, and appends each step event as it arrives. The database is append-only: the application cannot update or delete a row, and the Triage resource remains the source of truth for an Active Triage. The server keeps the database on a PersistentVolumeClaim so it survives a restart.

The retention controller deletes a Triage seven days after the time in `status.terminalAt`. Deleting the resource does not delete its history. The metrics API on port 9090 exports Triages by Phase, Proposals by Action, decisions, Verification outcomes, time to Recovered, and the tokens and cost of each Triage; the provisioned Grafana dashboard charts them.

## Dashboard

`pagerless-server` serves the dashboard on port 8082. The Triage list merges the latest SQLite snapshot of every Triage with the live resources, so a row survives retention, and filters by Phase, Workload, Trigger source and time range. The detail view shows the live steps over Server-Sent Events, the Findings with their sources, the Diagnosis, the rendered change, the decision and execution, the Verification timeline, the Narrative, and embedded Grafana panels for the Workload's metrics and logs. The panels carry deep links, so the telemetry is one click away when an iframe cannot load. The API is read-only and uses the server's own Kubernetes client and history; it never writes.

The dashboard is built with Node 22.17.1 and the pinned dependencies in [`dashboard/package-lock.json`](dashboard/package-lock.json), and its output is embedded in the server binary at compile time. Local access:

```bash
make dashboard-build   # npm ci && npm run build into dashboard/dist
make build-images      # or build the server image, which runs the build in a Node stage
kubectl --context kind-pagerless -n pagerless port-forward svc/pagerless-server 8082:8082
# open http://localhost:8082
kubectl --context kind-pagerless -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
```

Grafana embeds at `http://localhost:3000`, which is configurable with `--grafana-url`; the dashboard references are configurable with `--grafana-workload-dashboard-uid` and `--grafana-logs-dashboard-uid`. The dashboards themselves are provisioned from `deploy/observability/dashboards`. The HTTP API, its filters and the event-stream format are documented in [docs/dashboard.md](docs/dashboard.md). Dashboard sign-in and approvals are not part of this view; they are added with Keycloak.

## Safety boundaries

The model proposes; deterministic code decides.

- The agent service account has no Kubernetes API permissions. The Kubernetes MCP identity can read only allowlisted namespaced resources and pod logs; it cannot read Secrets, exec, proxy or port-forward.
- Every MCP call presents a Keycloak access token, which the agent obtains with its projected service account token. agentgateway validates it against the realm JWKS, removes it before forwarding, and allowlists the tools the agent may call. A `resources_get` or `resources_list` call that names a Secret is denied by argument.
- MCP services run read-only. The agent's registry independently restricts tool names, the Workload namespace, the Sandbox repository and the Argo CD application. Grafana uses a Viewer account, and GitHub uses a read-only Sandbox token that only the gateway holds.
- Only the Executor receives the GitHub write token. Independent Job admission prevents the server from creating agent Jobs with another ServiceAccount, arbitrary Secret references, host access or privileged containers.
- A separate admission policy permits the server's Workload updates only as dry runs. The corresponding Role is scoped to `sandbox`; Validation also checks its configured namespace allowlist and CPU/memory bounds.
- Approval is tied to the exact rendered change, its Workload generation and its Sandbox base. Approval and Rejection for the same Proposal contend for one canonical TriageApproval name.
- The Executor recomputes the stored change's hash and checks a matching Approval before applying the patch. It rejects changes outside the Proposal's single file and changes to `.github/`.
- Alertmanager deliveries require a bearer token read from a mounted Secret on every request. Bootstrap provisions matching sender and receiver tokens and binds the approver Role to the explicit current Kubernetes user.
- Investigation budgets are 15 tool calls, five minutes and approximately 100,000 input tokens, including cached input. Tool outputs and the Context Bundle are bounded and wrapped as untrusted data. The current tests cover those boundaries; the broader prompt-injection evaluation suite is planned for Phase 2.
- Verification is decided by Go after the observation window. An unavailable evidence API leaves the outcome undecided; it does not count as recovery.

Keycloak issues the agent's MCP token and is configured with the dashboard client for #29, and agentgateway enforces authentication and per-tool authorization on every call. The architecture decisions are recorded in [docs/adr](docs/adr/README.md).

## Local workflow

Development uses WSL2 Ubuntu with the repository in the Linux filesystem. Allocate roughly 24 GB to WSL for the single unified stack described in [AGENTS.md](AGENTS.md). The bootstrap's Docker preflight rejects engines below its 8 GiB minimum.

Install a reachable Docker engine, GNU make, Git with a configured commit identity, and these pinned tools:

- Go **1.27.4**, as declared in [go.mod](go.mod), for the CLI, tests and end-to-end harness.
- kind **v0.33.0**, kubectl **v1.37.0**, Helm **v4.3.2** and uv **0.13.8**.
- Python **3.14.10**, managed by uv.
- GitHub CLI **v2.104.0**, required for the real Sandbox paths in `make break` and `make reset`.

[hack/versions.env](hack/versions.env) pins the bootstrap tools, kind node image, Argo CD chart and Pagerless image references. [Makefile](Makefile) lists the supported targets.

### Use your Sandbox fork

1. Create a repository from the [Sandbox template](https://github.com/a-hagi613/pagerless-sandbox). Enable auto-merge and require its `kubeconform` check on `main`, following that repository's setup instructions.
2. Prepare two fine-grained GitHub tokens scoped to that copy: a read-only token with Contents read access, and a separate write token with Contents and Pull requests write access. Replay avoids model charges; it still performs real MCP reads and real Sandbox GitHub operations in this workflow.
3. Run the local bootstrap. It prompts for the Sandbox URL, tokens and model provider, defaulting to `replay`, and preserves them in the ignored root `.env`. The ordinary `make up` path uses the published, publicly pullable `0.1.0` GHCR references; `make dev` builds and loads this checkout's images instead.

Argo CD reads platform manifests from Git even when images are built locally. Push the intended commit before selecting it with `PLATFORM_REVISION`; set `PLATFORM_REPO_URL` as well when using another platform repository. From a checkout at that pushed commit:

```bash
make build-cli
export PATH="$PWD/bin:$PATH"
PLATFORM_REVISION="$(git rev-parse HEAD)" make dev
```

The production Replay artifact at `evals/replay/oom.json` was captured from one live Haiku investigation of the Phase 1 placeholder Workload, and its provenance binds the artifact to that agent Job and the server-validated PatchResources Proposal. It predates the OpenTelemetry Demo in the Sandbox: its Proposal names the Phase 1 container, so it does not remediate the demo's `oom` Scenario. Replay plays the recorded responses against the current Sandbox, so the default Replay path needs a new genuine recording captured against the demo. Until that recording is committed, use a live provider, or the isolated end-to-end harness with its synthetic fixture, to run the demo. The Narrative Job plays `evals/replay/narrative.json`, which needs no paid call.

The terminal workflow is:

```bash
make break SCENARIO=oom
kubectl triage --context kind-pagerless list
```

`make break` opens an auto-merging Scenario pull request in your Sandbox and waits for the checkout Workload to become OOMKilled and for `ContainerOOMKilled` to fire in Alertmanager. The alert carries `pagerless_triage="true"`, `namespace`, `deployment` and `pod` labels. Use the printed Triage name with `kubectl triage --context kind-pagerless show <name> --watch`, review its rendered change, and approve it with `kubectl triage --context kind-pagerless approve <name>`. Approval prompts for confirmation; `--yes` is available for an explicitly reviewed noninteractive invocation.

`make break SCENARIO=cpu` injects the CPU-stress Scenario instead. It applies a Chaos Mesh `StressChaos` experiment to the `payment` Workload outside Git, so the Sandbox manifests stay at known-good, and waits for Chaos Mesh to inject it. The expected outcome is `NoAction` with a Diagnosis that names the experiment, because no change to the Workload fixes a cause that lives outside it. [docs/scenarios.md](docs/scenarios.md) records the Scenario list, the answer key, and both injection paths.

`make break SCENARIO=cart-flag` (and `payment-flag`, `product-catalog-flag`, `email-memory-leak`) injects a feature-flag fault. Each one commits a single change to the Sandbox's `flagd-config` ConfigMap, waits for Argo CD to sync it, replaces the flagd pod and, for the three service faults, the affected Workload's pod so the demo loads the new flag, and waits for the alert that the Workload raises: `ServiceErrorRateHigh` for the three service faults, or `ContainerMemoryGrowth` for the email memory leak. The expected outcome for all four is a `Rollback` of the injected commit.

To start a Triage manually, use `kubectl triage --context kind-pagerless --namespace sandbox deploy/checkout`. The CLI also accepts `pod/<name>` and `sts/<name>`, resolves pods to their owning Workload, and returns the existing Active Triage when one already exists.

`make reset` removes any Chaos Mesh experiment from the sandbox namespace, force-resets your Sandbox's `main` to its `known-good` tag, and waits for a healthy Argo CD rollout. `make down` deletes the local kind stack and keeps `.env`.

### Run the isolated end-to-end harness

The `make e2e` target uses the ordinary local stack and a local Git/GitHub adapter with disposable read/write credentials. It exercises real Kubernetes, Argo CD, Git commits and the upstream MCP images with the explicitly synthetic recording at `evals/e2e/fixtures/synthetic-oom.json`. It neither needs a paid model key nor mutates your remote Sandbox. CI runs this harness on every pull request, and it is the same flow that was validated locally against the pinned platform commit.

A local run requires the exact platform commit already pushed to the public repository:

```bash
PLATFORM_REVISION="$(git rev-parse HEAD)" make e2e
```

The runner refuses to reuse an existing `pagerless` kind cluster unless `PAGERLESS_E2E_REUSE_CLUSTER=1` is explicitly set for the isolated test cluster. It uses a separate protected configuration file and never reads the root `.env`. By default it removes the cluster and temporary test state; retained-cluster and recovery behavior are documented in [evals/e2e](evals/e2e/README.md).

The harness checks the complete Triage phase sequence, CLI Approval, a merged Sandbox pull request, Argo CD health at the merge commit, and the full five-minute Verification window. Its adapter does not test GitHub.com's external API or hosted branch protection.

### Use a live model and capture Replay

Select `MODEL_PROVIDER=anthropic` with `ANTHROPIC_API_KEY` in the ignored setup file, or `MODEL_PROVIDER=bedrock` with `AWS_REGION` and inference-scoped AWS credentials. Host AWS profile files are not mounted into agent Jobs. Live providers use the fixed Haiku 5.5 identifiers and prompt caching described in [agent](agent/README.md).

The genuine OOMKilled recording at `evals/replay/oom.json` was captured from one live investigation, and its provenance records the agent Job, proposal hash, model and token usage. Its durable claim, retrieval, credential checks, normalization and provenance workflow are documented in [evals/replay](evals/replay/README.md). Synthetic test responses are kept separate and are never promoted as live evidence. Development and CI remain on Replay by default.

## CLI reference

- `kubectl triage <pod/name | deploy/name | sts/name>` starts or joins a Triage; `--namespace` selects the Workload namespace and `--hint` adds human guidance.
- `kubectl triage list` lists Triages. `list -o json` and `list -o yaml` return structured output.
- `kubectl triage show <name>` shows Findings, the Diagnosis, Proposal, execution, Verification checks, and the Narrative. `show -o json` and `show -o yaml` return structured output.
- `kubectl triage show <name> --watch` streams investigation steps and stops at Proposed or a terminal phase. It cannot be combined with structured output.
- `kubectl triage approve <name>` reviews and approves the exact current Proposal.
- `kubectl triage reject <name> --reason "The proposed memory limit needs further review."` records a final Rejection.
- `kubectl --context kind-pagerless -n pagerless get triages` lists the underlying resources.

Triages are stored in `pagerless`; the Workload namespace is independent. The plugin uses the current kube context unless `--context` or `--kubeconfig` selects another one.

## Development checks and services

`make test` runs the Go and Python suites. `make lint` runs Go vet, golangci-lint, ruff formatting/lint and strict mypy. `make test-go`, `make test-agent`, `make lint-go` and `make lint-agent` run their respective subsets. The Go suites use envtest rather than the local kind cluster. `make dashboard-check` runs the dashboard's typecheck and lint, and `make dashboard-build` produces the assets that the Go embed and the server image use.

The observability stack runs in `monitoring`. Grafana has Loki and Tempo data sources and the provisioned **Pagerless** dashboard, which charts Triages by Phase, Proposals by Action, decisions, Verification outcomes, time to Recovered, and tokens and cost per Triage. It can be opened with:

```bash
kubectl --context kind-pagerless -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
```

Stack values and component pins are documented in [deploy/observability](deploy/observability/README.md). The [CI workflow](.github/workflows/ci.yml) defines Go and agent checks, credential-free kind integration, and versioned runtime image publication on release tags. The `v0.1.0` tag published the server, Executor and agent images, and anonymous registry pulls succeed for all three.

## Roadmap

1. **Phase 1: core loop locally.** Complete: the genuine live OOMKilled capture, free Replay validation through Recovered on kind, credential-free end-to-end CI, and the public `v0.1.0` runtime images. The operator, CLI, agent, MCP integrations, Validation, Approval, Executor and Verification form this phase.
2. **Phase 2: complete the local product.** Finish the React/shadcn dashboard with Start Triage, Keycloak sign-in and approvals, add RestartWorkload/Scale/Rollback, the remaining Scenarios, agentgateway, and the evaluation and prompt-injection suites. `make eval` is planned for that phase and is not an available target yet.
3. **Phase 3: cloud and release.** Add Terraform for EKS, Karpenter, Pod Identity, short-lived cloud sessions with teardown and budget controls, then publish the demonstration video, evaluation results and measured operating costs.

## Documentation

- [Glossary](GLOSSARY.md): the project's domain vocabulary.
- [Scenarios and the answer key](docs/scenarios.md): the failure injections, both injection paths, and the expected Proposal for each Scenario.
- [Architecture Decision Records](docs/adr/README.md): significant decisions and their rationale.
- [Dashboard API and build](docs/dashboard.md): the read-only HTTP endpoints, the event stream and the Grafana configuration.
- [Server manifests and admission boundaries](deploy/pagerless-server/README.md).
- [Read-only MCP services](deploy/mcp/README.md).
- [Chaos Mesh](deploy/chaos-mesh/README.md).
- [MCP gateway and tool policy](deploy/gateway/README.md).
- [Keycloak realm and clients](deploy/keycloak/README.md).
- [Agent runtime and Replay](agent/README.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
