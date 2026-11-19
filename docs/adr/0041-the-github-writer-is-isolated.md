# ADR-0041: The GitHub writer is isolated in internal/github/pr

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

The agent reads repository content through tools, and every read path is reachable from a prompt or from model output. The GitHub credential that opens a pull request is the most damaging credential in the product: it can change any file in the Target Repository. A read tool that could reach the writer would let a prompt injection turn evidence collection into a write.

## Decision

The write capability lives in one package, `internal/github/pr`, with its own client type and its own `Token` function. `internal/github` is a read-only client that holds no write method at all. A unit test walks the dependency graph and fails if the read-only GitHub client, the provenance ladder, Proposal Validation, the Proposal model, the agent report contract, or the Page store can reach the writer.

The writer's entry point accepts a server-validated value: the Page identity, the approved Proposal hash, the base commit, and whole-file contents. It never accepts model text. It creates blobs, a tree with `base_tree` set to the base commit, one commit, one ref named `pagerless/<page>-<hash8>`, and one Pull Request through the Git Data API. Every step is idempotent: a rerun reuses the branch and the Pull Request, a branch carrying a different tree fails with a branch conflict instead of overwriting a human's work, and a `422 already exists` resolves by lookup. Before writing it re-checks that the base branch still points at the recorded base commit.

## Consequences

- The writer cannot be reached from a read path, and the boundary is tested rather than documented.
- A stale base fails the write with a specific error, so the Proposal must be approved again.
- The credential volume is mounted only when pull requests are enabled.
