# ADR-0018: Deterministic decisions and canonical Active Triages

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

Admission checks run before the API server stores a resource. Two requests can both pass a check that searches for an earlier resource before either is stored. This affects the single Active Triage per Workload and the single human decision per Proposal. A process-local admission lock does not cover the storage operation.

Resolving duplicate Triages only by creation time and name could also replace an existing execution when a late duplicate has the same second-resolution creation time. An already approved change or open Sandbox pull request must not be abandoned to resolve that tie.

## Decision

A TriageApproval has one deterministic resource name for the exact Triage identity and Proposal hash. `TriageApprovalName` hashes the namespace, full Triage name, UID, and exact Proposal hash, separated by NUL bytes, with SHA-256. Its name is `decision-` followed by the lowercase, unpadded base32 digest: 61 characters, within a DNS label. It does not truncate any identity field.

The CLI uses this name, and the validating webhook requires it and rejects `generateName`. Direct API clients use the same contract. Competing decisions therefore contend on one resource name and receive AlreadyExists after the first resource is stored. This supplements the requester identity, immutable spec, phase, current hash, expiry, and Stale Proposal checks; it changes no execution hash semantics. A new UID or Proposal hash produces a different decision name.

The Triage reconciler repairs duplicate admission through direct API reads. An Active Triage with an admitted Approval, an applied Approval, or an execution in progress is protected. A sole protected Triage remains canonical. Otherwise, the oldest creation timestamp wins, followed by name and UID for deterministic ties. Preserving approved work takes precedence over creation order. Opaque resource versions are not ordered.

Unapproved duplicates end as Failed with reason Duplicate. The reconciler deletes only their controlled agent Jobs, with a UID precondition, and also cleans a Job observed after that failure. Approval admission and the execution reconciler check the canonical Active Triage before permitting new work. If legacy data contains multiple approved Active Triages, resolution fails closed without choosing one to cancel; an operator must repair that inconsistent state.

## Consequences

- One exact Proposal can have at most one newly admitted decision resource, independent of admission request timing. Plain-manifest clients must compute the canonical name or use the CLI.
- The complete Triage identity prevents collisions caused by long-name truncation and prevents a recreated Triage from inheriting an earlier decision.
- Duplicate resolution is a repair across several Kubernetes resources, not a distributed transaction. A read-only agent already started in the admission window may run briefly before cleanup. The checks do not retroactively cancel approved external work or an open pull request.
- The inconsistent legacy case requires operator repair; the normal lifecycle retains Approval as its only human gate.
- Tests create duplicate records by bypassing only the create webhook in isolated envtest fixtures. Production admission remains enabled.
