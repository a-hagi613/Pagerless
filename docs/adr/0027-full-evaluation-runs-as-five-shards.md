# ADR-0027: The full paid evaluation runs as five parallel shards that one job merges

- **Status:** Accepted
- **Date:** 2026-11-18
- **Relates to:** [ADR-0006](0006-verification-is-decided-by-code.md), [ADR-0010](0010-cost-model.md)

## Context

A full paid batch runs every Scenario twice: twenty live investigations against one kind cluster, each resetting the Sandbox, injecting a fault, waiting for an alert or a manual Trigger, approving the Proposal, and waiting out the five-minute Observation window. On one runner that took roughly three hours, and the batch spent most of that time waiting rather than using the model. A failure near the end also lost the whole run, because the one job published a single document.

The batch also hid a deadline bug. `email-memory-leak` starts from `ContainerMemoryGrowth`, whose rule ignores a pod younger than 15 minutes and then holds its `for: 1m`, so the alert cannot fire until about 16.5 minutes after the pod was created. `hack/break.sh` allowed 15 minutes, and the harness replaced the pods that read the demo's flags before every run, which reset the email pod's age to zero immediately before the memory-leak run. Scenario 8 therefore failed on a deadline that its own rule made unreachable.

## Decision

`eval-live.yml` dispatches a full batch as five parallel shards on five runners, and one `aggregate` job merges their documents into the batch document a full run publishes.

- The shards split the answer key by cost and dependency: `1-checkout-fast` (`oom`, `bad-config`), `2-checkout-spec` (`bad-image-tag`, `unschedulable`), `3-flags-a` (`cart-flag`, `payment-flag`), `4-flags-cpu-inj` (`product-catalog-flag`, `cpu`, `injection`), and `5-memory-leak` (`email-memory-leak`). Every Scenario appears in exactly one shard.
- A `measured` run, and any dispatch that names `scenarios`, still runs one shard named `single`. The shard matrix is one `fromJSON` expression over the inputs, so the workflow has one shape for both cases.
- Each shard uploads its own result document, memory samples, and diagnostics, named with the shard id. The `aggregate` job runs only for a sharded full batch, downloads the artifacts through the GitHub API, and writes the merged document with `python -m pagerless_eval.results merge`.
- `merge_documents` refuses a batch that cannot be published honestly: a shard from another commit, another evidence mode, or a dirty worktree; a missing or extra Scenario or repeat; two shards that record the same run; and any failed run. It starts the document at the earliest shard, sums the spend and the per-shard budgets, and marks the batch exhausted when a shard was. It renders the result table into the job summary and the workflow concatenates the shards' memory samples.
- `hack/break.sh` allows 22 minutes for an alert and the harness allows 28 minutes for the whole injection, because the memory-growth rule needs about 16.5 minutes from a fresh pod plus Prometheus's scrape and evaluation delay.
- The harness replaces the pods that read the demo's flags only after a Scenario changed the flags. `hack/reset.sh` keeps replacing them itself in a live run.

## Consequences

- Wall clock falls from about three hours to about 45 minutes, because the shards wait in parallel. Each shard still pays the stack setup, so total runner minutes rise: five kind clusters are created instead of one.
- The batch budget is enforced per shard, because the workflow passes the same inputs to each. The merged document records the sum of the shard budgets, which is the total the batch could have spent, instead of presenting one shard's limit as the batch's.
- A partial batch publishes nothing. The `aggregate` job fails when a shard failed, and the merge refuses a document set that misses a shard, so a published document always covers all ten Scenarios twice.
- Artifact names carry the shard id, and the merged artifacts keep the `full` suffix without one. The documentation and the artifact table name both forms, so an operator can find a single shard's failed document without downloading the merged one.
- The 22-minute alert deadline is longer than any other wait in the harness. It is deliberate: the alert rule's 15-minute pod-age requirement is what makes the memory leak a leak rather than a startup ramp, and shortening the deadline would only make the Scenario fail sooner.
