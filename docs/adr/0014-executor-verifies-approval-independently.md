# ADR-0014: The Executor verifies the Approval independently

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

`pagerless-server` processes untrusted agent output. If the Executor acted on the server's instruction alone, a defect or compromise in the server could result in an unapproved change being executed.

## Decision

`pagerless-server` calls `pagerless-executor` over HTTP with the Triage and the Proposal hash. Before applying any change, the Executor independently confirms that the Triage is in the `Approved` phase and that a valid `TriageApproval` exists for exactly that hash. It then applies the stored change and returns the result (a pull request URL or the restarted pods), which the server records in Triage status.

## Consequences

- Executing a change requires both a valid Approval and a request from the server.
- The server's permission to create agent Jobs must not let it assume the Executor's ServiceAccount or mount its write credentials. An independent, fail-closed admission policy limits server-created Jobs to the unprivileged agent identity and the agent's intended read-only credentials, report token, temporary storage, and dedicated Replay capture claim. Server code alone does not enforce this boundary.
- The capture claim name is reserved as `pagerless-replay-capture`. Allowing arbitrary claim names would expose other data in the shared namespace; changing the name requires an operator to change the policy and server validation together.
