# ADR-0014: The Executor verifies the Approval independently

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

`pagerless-server` processes untrusted agent output. If the Executor acted on the server's instruction alone, a defect or compromise in the server could result in an unapproved change being executed.

## Decision

`pagerless-server` calls `pagerless-executor` over HTTP with the Triage and the Proposal hash. Before applying any change, the Executor independently confirms that the Triage is in the `Approved` phase and that a valid `TriageApproval` exists for exactly that hash. It then applies the stored change and returns the result (a pull request URL or the restarted pods), which the server records in Triage status.

## Consequences

- Executing a change requires both a valid Approval and a request from the server.
