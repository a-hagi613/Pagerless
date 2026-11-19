# ADR-0008: pagerless-server renders the change; the Executor only applies it

- **Status:** Accepted; its auto-merge delivery clause is superseded by [0041](0041-provenance-is-a-recorded-ladder.md)
- **Date:** 2026-11-16
- **Amended by:** [ADR-0021](0021-rollback-may-revert-declared-dependency-paths.md), [ADR-0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md), [ADR-0043](0043-the-github-writer-is-isolated.md)

> Amended in v2.0: the split of duties stands — `pagerless-server` still renders
> the change deterministically during Validation, and the approver still sees
> exactly what was validated — and only the component that applied it is gone.
> The isolated pull request writer in `internal/github/pr`
> ([ADR-0043](0043-the-github-writer-is-isolated.md)) opens one pull request
> against the Target Repository, and nothing applies a stored patch any more:
> the repository's own Checks are authoritative and the Operator merges
> ([ADR-0041](0041-provenance-is-a-recorded-ladder.md)). The Executor named in
> the title and the decision below is history
> ([ADR-0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md)).

## Context

What a human approves must be exactly what is executed. If the Executor derived the change itself, the approved Proposal and the executed change could diverge.

## Decision

During Validation, `pagerless-server` renders the exact change: a git patch on a recorded base commit for pull request Actions, or the Workload generation for RestartWorkload. The rendered change is shown to the approver and included in the Proposal hash, together with the Triage identifier, the Action, its parameters, and the target Workload.

`pagerless-executor` applies the stored patch byte-for-byte and refuses if it does not apply cleanly.

For Rollback, the parameter is a single commit, which must be among the last 20 commits touching the Workload's path or a path the Workload declares in `pagerless.dev/depends-on-paths` (ADR-0021). The patch is the revert of that commit.

## Consequences

- If the Workload or the Sandbox changes before Approval, the Proposal becomes Stale and requires re-validation and a new Approval.
- Unapproved Proposals expire after one hour.
