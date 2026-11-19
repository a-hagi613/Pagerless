# Glossary

Pagerless watches a Kubernetes cluster for failing Targets and diagnoses them
with an AI agent. A remediation is proposed as a pull request against the Git
repository that owns the Target, and is opened only after a human approves it;
Pagerless itself never writes to the cluster. This glossary defines the project's
vocabulary. Code, documentation, issues, and pull requests use these terms
exactly as defined here.

## Investigation

**Page**:
One investigation of one failing Target, from Trigger to verified outcome. Also
the name of the Kubernetes resource that records it (`pages.pagerless.dev`).
_Avoid_: triage, incident, ticket, alert, case, run

**Target**:
The Kubernetes object that is failing and that a Page investigates: a
Deployment, StatefulSet, DaemonSet, Job, CronJob, Pod, or
PersistentVolumeClaim — or, for infrastructure faults, a Node. The pod that
raised a Trigger is evidence, not the Target.
_Avoid_: workload, resource, app, service

**Trigger**:
The observation that opens or joins a Page: a Kubernetes Warning Event, a state
transition in a Target's status, an inbound alert from a Connector, or a human
request.
_Avoid_: alert (for the observation as a whole), event, signal, page (as a verb)

**Fingerprint**:
The stable identity of one failure, built from the failing object, its kind,
namespace and UID, the failure reason, and the failing container. It deliberately
excludes messages, timestamps, and repetition counts, so that one crash loop is
one failure rather than hundreds.
_Avoid_: hash, key, signature

**Active Page**:
A Page that has not reached a terminal Phase. A Fingerprint has at most one
Active Page, and further Triggers join it.
_Avoid_: open page, running page

**Cooldown**:
The period after a Page reaches a terminal Phase during which the same
Fingerprint does not open a new Page; Triggers that arrive during it are counted
on the Page that recorded the outcome.
_Avoid_: backoff, quiet period, rate limit

**Recurrence**:
A failure that happens again after its Cooldown has elapsed and therefore opens
a new Page, linked to the Page that recorded the previous outcome.
_Avoid_: repeat, retry, flapping

**Suppression**:
A Trigger that does not open a Page because an Active Page already covers its
Fingerprint, because the Fingerprint is in Cooldown, or because the Trigger
exceeds the burst ceiling. A suppressed Trigger is still counted and visible.
_Avoid_: drop, throttle, deduplication

**Hint**:
Optional guidance a human attaches when opening a Page, passed to the agent as
human input.
_Avoid_: note, prompt, instruction

**Context Bundle**:
The bounded set of evidence Pagerless gathers before the agent's first model
call: Target status, pod states, Events, recent logs, and the Target's
provenance in its repository.
_Avoid_: context, prefetch, snapshot

**Finding**:
One cited piece of evidence, such as a log line, an Event, a metric, a trace, a
commit, or a manifest, that supports a Diagnosis.
_Avoid_: observation, clue, signal

**Diagnosis**:
The agent's root-cause conclusion for a Page, supported by Findings.
_Avoid_: analysis, RCA, summary

## Remediation

**Target Repository**:
The Git repository, branch, and path that hold the Target's manifests and that a
Proposal changes. A repository becomes a Target Repository by configuration or by
provenance, never by its name.
_Avoid_: sandbox, GitOps repository, source repository

**Proposal**:
The exact change the agent recommends for a Page: one or more file edits against
a recorded base commit, or no change at all. It is the unit that is validated,
rendered, hashed, and approved.
_Avoid_: fix, suggestion, plan, recommendation, patch (for the whole object)

**Diff**:
The rendered, human-readable change a Proposal would make, shown to the approver
and carried into the pull request.
_Avoid_: patch (for the rendered form), change

**Validation**:
The deterministic checks a Proposal passes before a human sees it: file and size
bounds, paths inside the Target Repository, a clean application against the base
commit, and no change to repository automation or credentials.
_Avoid_: review, verification

**Checks**:
The Target Repository's own continuous-integration results on the pull request
that carries a Proposal: schema validation, rendering, policy, and tests.
Pagerless never overrides a failing check.
_Avoid_: validation (the in-process pre-check), CI status

**Stale Proposal**:
A Proposal whose base commit or Target changed before approval. It cannot be
approved; it is re-rendered under a new hash.
_Avoid_: outdated proposal, invalid proposal

**Approval**:
One human decision to accept one specific Proposal hash, recorded on the Page.
_Avoid_: sign-off, ack, confirmation

**Rejection**:
One human decision to decline a Proposal. A Rejection is final for its Page.
_Avoid_: denial, veto

**Pull Request**:
The artifact that carries an approved Proposal into a Target Repository for
review and merge. It is the only way Pagerless changes a cluster's desired state.
_Avoid_: merge request, change request

**Verification**:
The deterministic check after a Pull Request merges that decides whether the
Target is Recovered or NotRecovered.
_Avoid_: validation (the pre-approval check), fixed / not fixed

**Narrative**:
A short, purely descriptive account of a Page written after Verification. It
never affects the outcome.
_Avoid_: summary, report, postmortem

## Lifecycle

**Phase**:
The stage a Page is in. The main path is Pending, Investigating, Proposed,
Approved, PullRequestOpen, Verifying, then Recovered or NotRecovered. A Page may
instead end as Diagnosed (no change recommended), Rejected, Expired (no decision
inside the approval window), Invalid (the Proposal failed Validation twice),
Superseded (another Page is canonical for the Fingerprint), or Failed (agent
error, exhausted budget, or a refused or abandoned pull request).
_Avoid_: state, status, stage

## Environment

**Cluster**:
The one Kubernetes cluster an installation watches, identified by a stable
identifier that survives control-plane restarts. An installation serves exactly
one Cluster.
_Avoid_: environment, site, region

**Connector**:
A configured, read-only integration that supplies evidence or Triggers: the
Kubernetes API, a log or metric backend, an alert source, or a Git provider. A
Connector can only read.
_Avoid_: adapter, integration, plugin, MCP server

**Operator**:
The human accountable for one installation, who supplies its credentials,
approves Proposals, and owns the Target Repositories.
_Avoid_: admin, user, owner, approver (as a synonym)
