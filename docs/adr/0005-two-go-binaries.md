# ADR-0005: Two Go binaries, pagerless-server and pagerless-executor

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

The system needs a reconciler, an agent report endpoint, an Alertmanager receiver, an admission webhook, and a dashboard API, all of which read or write Triage state. Write credentials for the cluster and for GitHub must be isolated from code that processes untrusted agent output.

## Decision

`pagerless-server` runs as a single replica and hosts the reconciler, the agent report endpoint, the Alertmanager receiver, the admission webhook, and the dashboard API and UI, backed by SQLite on a persistent volume.

`pagerless-executor` holds the only write credentials (its own service account and the write-scoped GitHub token) and has no other responsibilities. The server invokes the Executor over HTTP and records the result (see ADR-0014).

## Consequences

- There is exactly one writer of Triage status, as ADR-0001 requires.
- Write credentials never share a process with agent-facing code.
- Pagerless itself is not highly available (see ADR-0012).
