# ADR-0030: One Go binary with an ephemeral zero-RBAC agent Job

- **Status:** Accepted
- **Date:** 2026-11-18
- **Supersedes:** [ADR-0005](0005-two-go-binaries.md) and [ADR-0014](0014-executor-verifies-approval-independently.md).

## Context

v1 ran two Go binaries. `pagerless-server` reconciled Triages and served the
dashboard, and `pagerless-executor` was the only holder of write credentials: it
re-checked the approval and applied the change. The split existed to keep write
credentials out of the process that a browser and a model could reach, but it
also doubled the deployment, the RBAC, and the HTTP hop between the approval and
the change.

v2 removes every live cluster write, so there is no write credential left to
separate. Remediation is a pull request, and the pull request engine runs in the
server behind the same approval record that v1's executor re-read.

## Decision

There is one long-running Go binary, `pagerless-server`. It reconciles Pages,
serves the agent report API and metrics, records decisions, publishes approved
Proposals, and verifies the outcome.

The agent is an ephemeral `batch/v1` Job per Page:

- it runs as `pagerless-agent`, a ServiceAccount bound to no Role or
  ClusterRole, with `automountServiceAccountToken: false`;
- its only token is a projected token whose audience is the server, mounted as a
  file and presented to the report API, which authenticates it with a
  TokenReview;
- it holds no GitHub credential and no connector credential;
- it runs one container, with a memory-backed `emptyDir`, `runAsNonRoot`,
  `readOnlyRootFilesystem`, all capabilities dropped, and
  `seccompProfile: RuntimeDefault`.

The executor binary, its image, its Deployment, its Service, its Role and
token audience, and its HTTP API are deleted. The chart installs no second
workload.

## Consequences

- The chart's install footprint is one Deployment, one Service, one
  PersistentVolumeClaim, two ConfigMaps, one ServiceAccount plus the agent's,
  and a read-only identity.
- Page status has one writer that can observe both the approval and the
  publication (ADR-0001).
- The agent cannot escalate by reaching the API server: it has no RBAC, and the
  server is the only party that reads evidence on its behalf.
- A publish failure is retried by the controller, not by a CLI that has already
  exited.
- An operator who wants the v1 shape has no supported path; the upgrade guide
  says so.

## Alternatives considered

- **Keep the executor as a second process.** It would hold no credential it
  needs, because the write token belongs to the pull request engine, and it
  would add a second deployment without adding a boundary.
- **Run the agent as a long-lived Deployment.** It would hold a token and a
  queue forever, and a compromised model would keep its authority between
  investigations instead of losing it when the Job ends.
