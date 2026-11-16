# Pagerless

**An AI agent that diagnoses failing Kubernetes workloads and remediates them only after a human approves the exact change.**

> **Project status:** design complete, implementation in progress. This document describes the target system. The architecture and every significant decision are recorded in [`docs/adr/`](docs/adr/README.md).

## Overview

An alert or a command starts a Triage. A read-only agent investigates using logs, metrics, traces, events, Argo CD state, and Git history, then proposes a single remediation from a fixed menu. Deterministic code validates the Proposal and renders the exact change. Nothing is executed until a human approves that change, and recovery is verified by code rather than by the model.

## How it works

1. **Trigger.** An opted-in Alertmanager alert, or `kubectl triage deploy/checkout`, creates a Triage resource for the failing Workload.
2. **Context.** `pagerless-server` gathers a Context Bundle and starts a short-lived, read-only agent Job.
3. **Investigate.** The LangGraph agent, running Claude on Amazon Bedrock, queries MCP tools through agentgateway and reports its steps back to the server as it works.
4. **Propose.** The agent submits a Diagnosis and one typed Proposal. The server validates it against allowed Actions, parameter bounds, and namespaces, performs a server-side dry run, and renders the exact patch.
5. **Approve.** A human reviews the rendered change in the CLI or dashboard and approves it. The Approval is bound to the Proposal's hash.
6. **Execute.** A separate Executor, the only component with write credentials, independently re-checks the Approval and either restarts the Workload or opens a pull request that Argo CD syncs.
7. **Verify.** Deterministic checks decide whether the Workload recovered. If it did not, a Follow-up Triage begins.

## Safety model

The model proposes; deterministic code decides.

- The agent has no write credentials and cannot read Secrets or execute commands in pods.
- Remediations come from a fixed menu: RestartWorkload, Scale, PatchResources, Rollback, and NoAction.
- Every Proposal is validated and its exact change rendered before a human sees it.
- Approval is bound to a hash of the exact change. If the Workload or the repository changes first, the Proposal becomes Stale and must be approved again.
- The Executor verifies the Approval independently before acting.
- Each investigation is capped by tool-call, time, and token budgets.
- Logs are treated as untrusted input, and a prompt-injection suite tests that the agent ignores instructions embedded in them.
- Every MCP tool call is authenticated with a Keycloak-issued token, authorized by policy, and logged by agentgateway.

## Architecture

| Layer | Components |
| --- | --- |
| Control plane | `pagerless-server` (Go): reconciler, agent report endpoint, Alertmanager receiver, admission webhook, dashboard API |
| Execution | `pagerless-executor` (Go): the only write identity |
| Agent | Python, LangGraph, Claude on Amazon Bedrock |
| CLI | `kubectl-triage` (Go, Cobra) |
| Dashboard | React, TypeScript, shadcn/ui, with embedded Grafana panels |
| MCP tools | kubernetes-mcp-server, mcp-grafana, mcp-for-argocd, GitHub MCP, all read-only |
| MCP security | agentgateway, Keycloak |
| Observability | OpenTelemetry Collector, Prometheus, Alertmanager, Loki, Tempo, Grafana |
| GitOps and CI | Argo CD (app of apps), GitHub Actions |
| Infrastructure | kind locally; Amazon EKS with Terraform, Karpenter, and EKS Pod Identity in the cloud |
| Failure injection | OpenTelemetry Demo with flagd, Chaos Mesh, purpose-built manifests |
| Evaluation | pytest harness, promptfoo |

## Local bootstrap

`make up` creates a local kind cluster, installs Argo CD, and deploys your copy of the [Sandbox](https://github.com/a-hagi613/pagerless-sandbox), which holds the demo Workloads.

Prerequisites, at the versions pinned in [`hack/versions.env`](hack/versions.env):

- Docker Desktop, with at least 8 GiB of memory for the engine.
- kind v0.33.0, kubectl v1.37.0, and Helm v4.3.2, on the `PATH`.
- GitHub CLI v2.104.0, required by `make break` and `make reset`.
- GNU make. On Windows, run every command in WSL Ubuntu.

Steps:

1. Create your copy of the Sandbox from its template, then enable auto-merge and require the `kubeconform` check on `main`, as described in the Sandbox README.
2. Run `make up`. It prompts for your Sandbox URL, a read-only token and a write token (both fine-grained and scoped to your copy), and the model provider, which defaults to `replay`. The answers are written to `.env`, which Git ignores.
3. Run `make break SCENARIO=oom`. It commits the OOM Scenario to the Sandbox through an auto-merging pull request. The `checkout` Workload is then OOMKilled.
4. Run `make reset`. It force-resets the Sandbox `main` branch to the `known-good` tag, and the Workload returns to healthy.
5. Run `make down` to delete the cluster. It keeps `.env`.

## CLI

| Command | Description |
| --- | --- |
| `kubectl triage <pod/name \| deploy/name>` | Start a Triage for a failing Workload |
| `kubectl triage list` | List recent Triages |
| `kubectl triage show <name>` | Show the Diagnosis, Findings, and Proposal with its rendered change |
| `kubectl triage approve <name>` | Approve the current Proposal |
| `kubectl triage reject <name>` | Reject the current Proposal |
| `kubectl get triages` | List Triages with plain `kubectl` |

## Roadmap

1. **Core loop (local).** kind, the observability stack, Argo CD with the Sandbox, `pagerless-server`, the agent with read-only MCP tools, `pagerless-executor` with the pull request flow, the `kubectl triage` CLI, and Replay. Complete when the OOMKilled Scenario runs from alert through Diagnosis, Approval in the terminal, and pull request merge to a Verification of Recovered.
2. **Complete product (local).** The dashboard, the remaining Scenarios, agentgateway and Keycloak in front of every MCP tool, and the evaluation harness with the prompt-injection suite. Complete when `make eval` runs all ten Scenarios on kind and a blocked tool call is demonstrated.
3. **Cloud and release.** Terraform for EKS with Karpenter and Pod Identity, keyless Bedrock access, the nightly teardown workflow, and AWS Budgets alerts, followed by the demonstration video, published evaluation results, and measured operating costs. Complete when the stack runs on EKS, the demonstration is recorded, and the cluster is destroyed.

## Documentation

- [Glossary](GLOSSARY.md): the project's domain vocabulary.
- [Architecture Decision Records](docs/adr/README.md): significant decisions and their rationale.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
