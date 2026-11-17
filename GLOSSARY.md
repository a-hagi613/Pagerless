# Glossary

Pagerless diagnoses failing Kubernetes Workloads with an AI agent and applies a remediation only after a human approves it. This glossary defines the project's vocabulary. Code, documentation, issues, and pull requests use these terms exactly as defined here.

## Investigation

**Triage**:
One investigation of one failing Workload, from Trigger to verified outcome. Also the name of the Kubernetes resource that records it.
_Avoid_: incident, ticket, case, run

**Trigger**:
The event that starts or joins a Triage: an opted-in alert from Alertmanager, or a human request through the CLI or dashboard.
_Avoid_: event, page, signal

**Workload**:
The long-lived owner of the failing pods (a Deployment or StatefulSet) that a Triage targets. The pod that raised the Trigger is evidence, not the target.
_Avoid_: target pod, service, app

**Active Triage**:
A Triage that has not reached a terminal phase. A Workload has at most one Active Triage, and further Triggers attach to it.
_Avoid_: open triage, running triage

**Follow-up Triage**:
A Triage started automatically when Verification ends in NotRecovered, linked to the Triage it follows.
_Avoid_: retry, re-triage

**Hint**:
Optional guidance a human attaches when starting a Triage, passed to the agent as human input.
_Avoid_: note, prompt, instruction

**Context Bundle**:
The fixed set of evidence Pagerless gathers before the agent's first model call: Workload status, pod states, events, recent logs, Argo CD status, and recent Sandbox commits.
_Avoid_: context, prefetch, snapshot

**Finding**:
One cited piece of evidence, such as a log line, metric, trace, event, or commit, that supports a Diagnosis.
_Avoid_: observation, clue, signal

**Diagnosis**:
The agent's root-cause conclusion for a Triage, supported by Findings.
_Avoid_: analysis, RCA, summary

## Remediation

**Action**:
One kind of remediation on the fixed menu: RestartWorkload, Scale, PatchResources, Rollback, or NoAction.
_Avoid_: fix type, operation, command

**Proposal**:
The single typed remediation the agent recommends for a Triage: one Action with its parameters. It is the unit that is validated, hashed, and approved.
_Avoid_: fix, suggestion, plan, recommendation

**Validation**:
The deterministic check a Proposal must pass before a human sees it: allowed Action, parameter bounds, allowed namespace, server-side dry run, and rendered diff.
_Avoid_: verification (the post-execution check), review

**Stale Proposal**:
A Proposal whose base (the Workload's generation or the Sandbox commit) changed before Approval. It must be re-validated and approved again under a new hash.
_Avoid_: outdated proposal, invalid proposal

**Approval**:
A human decision to accept one specific Proposal, bound to that Proposal's hash.
_Avoid_: sign-off, ack, confirmation

**Rejection**:
A human decision to decline one specific Proposal. A Rejection is final for its Triage.
_Avoid_: denial, veto

**Executor**:
The separate write identity that carries out an approved Proposal, either live or as a pull request.
_Avoid_: applier, remediator, actuator

**Verification**:
The deterministic check after execution that decides whether the Workload is Recovered or NotRecovered.
_Avoid_: validation (the pre-approval check), fixed / not fixed

## Lifecycle

**Phase**:
The stage a Triage is in. The main path is Pending, Investigating, Proposed, Approved, Executing, Verifying, then Recovered or NotRecovered. A Triage may instead end as Diagnosed (NoAction acknowledged), Rejected, Expired (no decision within one hour), Invalid (failed Validation twice), or Failed (agent error, exhausted budget, refused or abandoned execution, or suppression of an unapproved duplicate Triage).
_Avoid_: state, status, stage

## Environment

**Sandbox**:
The separate Git repository that holds the demo Workloads' manifests. Argo CD deploys from it, Scenarios inject failures into it, and the Executor's pull requests target it.
_Avoid_: demo repo, GitOps repo, test repo

**Scenario**:
One repeatable, injectable failure, such as an OOMKilled pod, a bad image tag, or a prompt injection in logs, that serves as both a demonstration and an evaluation case.
_Avoid_: test case, fault, chaos experiment

**Replay**:
Running the agent against recorded model responses for a Scenario instead of a live model, at no cost and with deterministic results.
_Avoid_: mock mode, offline mode, fake LLM
