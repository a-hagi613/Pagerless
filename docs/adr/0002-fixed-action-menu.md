# ADR-0002: Fixed Action menu without free-form pull requests

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

The core safety principle is that the model proposes and deterministic code decides, so every remediation must be enumerable and bounds-checkable. Separately, live changes to resources managed by Argo CD are reverted by self-heal.

## Decision

The Action menu is:

| Action | Execution |
| --- | --- |
| RestartWorkload | Live |
| Scale | Pull request |
| PatchResources | Pull request |
| Rollback | Pull request |
| NoAction | None (Diagnosis only) |

Each Action has typed parameters whose bounds are enforced during Validation. There is no generic "open a pull request with this diff" Action. RestartWorkload deletes pods one at a time rather than using `kubectl rollout restart`.

## Consequences

- Every executable change has one of four shapes that Validation can reason about.
- `kubectl rollout restart` writes a `restartedAt` annotation to the pod template, which Argo CD reports as drift and self-heal reverts, triggering a second rollout. Deleting pods produces no drift. Live scaling is excluded for the same reason.
- A failure that no Action can remediate ends in a Diagnosis with NoAction.
