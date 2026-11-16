# ADR-0006: Verification is decided by deterministic code

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

After execution, the system must decide whether the Workload recovered. Allowing the model to judge its own remediation would contradict the core safety principle and make published fix rates self-graded.

## Decision

Deterministic Go checks decide whether the outcome is Recovered or NotRecovered:

- The rollout is available.
- The Argo CD application is Synced and Healthy at the pull request's merge commit.
- No new restarts or OOMKills occur within a five-minute observation window.
- The triggering alert has resolved.

The agent contributes only a narrative summary. For pull request Actions, the `Executing` phase lasts until the pull request is merged and Argo CD has synced it.

A NotRecovered outcome starts a Follow-up Triage, in which any rollback is proposed and approved like any other Proposal.

## Consequences

- Evaluation results are graded by code, not by the model under test.
- Every Verification takes at least the length of the observation window.
