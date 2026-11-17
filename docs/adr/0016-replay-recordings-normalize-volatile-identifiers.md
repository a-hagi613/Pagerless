# ADR-0016: Replay recordings normalize volatile identifiers

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

Replay runs the agent against recorded model responses, in CI and as the default for development (ADR-0010). Tool calls still execute for real, so a replayed response that names a pod must name a pod that exists in the current run. Every run of a Scenario creates new pod names, timestamps, and Sandbox commit SHAs, so a recording that stores them verbatim cannot replay.

## Decision

A recording is a versioned JSON document, `pagerless.dev/replay` version 1, that holds the model responses of one run in order. Each response is tagged with the agent stage that asked for it.

- **Sequence, not request matching.** Replay plays response N for the Nth request. It refuses to continue when the stage of the request differs from the stage the response was recorded for, and when the recording runs out. A recording that no longer matches the graph therefore fails with a message that says so, and never answers with a wrong response.
- **Placeholders.** When a response is recorded, every volatile identifier is replaced, in all strings and keys of the response, by a placeholder resolved from the Context Bundle: `{{pod:N}}`, `{{pod:trigger}}`, and `{{replicaset:N}}` for pods in creation order, `{{commit:N}}` and `{{commit:N:L}}` for the Sandbox commits (full and abbreviated), and `{{ts:OFFSET:SHAPE:ZONE}}` for timestamps. Replay resolves each placeholder against the Context Bundle of the current run.
- **Relative time.** A timestamp is stored as its offset from the bundle's `gathered_at`, with its separator, fractional digits, and zone, so that it round-trips exactly against the same bundle and keeps its form against another.
- **Escaping.** A literal `{{` in recorded text is stored as `{{lbrace}}`, so text read from a log can never be mistaken for a placeholder.
- **Generated names only.** Only names that Kubernetes generates are normalized. Names of StatefulSet pods are stable and are stored as they are.
- **Failing early.** Recording refuses a response that still contains a volatile identifier that no bundle can resolve (a full Git SHA, a Workload pod name, or a timestamp that cannot be parsed), and loading refuses the same, so a recording that would not replay is rejected when it is written. A placeholder that the current bundle cannot resolve stops the run with an error that names it.
- **Reproducible calls.** Tool call identifiers are renumbered sequentially when a response is recorded.

## Consequences

- The Context Bundle must order pods by creation time and commits newest first, so that an index names the same entity in every run. A change to that order is a change to the format.
- A replayed timestamp is correct relative to the current run but is not the timestamp the run's own evidence shows, so a replayed Finding can cite a time a few seconds away from the one in the live bundle.
- A volatile identifier that appears only in a tool's output, and not in the Context Bundle, cannot be normalized. Recording such a run fails, and the Scenario's bundle or prompt must be changed to bring the identifier in.
- Changing the placeholder grammar requires a new recording version.

## Alternatives considered

- **Match each response to a hash of its request.** Robust to reordering, but the request contains the volatile identifiers too, and any prompt change would invalidate every recording.
- **Store absolute timestamps and shift them on replay.** Equivalent for one timestamp, but it needs a recorded reference, and a single clock rule is simpler to verify.
- **Freeze the cluster's identifiers.** Pods and commits are created by the cluster and Git, so a frozen run would need a bespoke environment for CI.
