# ADR-0015: Fine-grained personal access tokens for GitHub

- **Status:** Accepted
- **Date:** 2026-11-16
- **Amended by:** [ADR-0033](0033-one-credentials-secret.md), [ADR-0043](0043-the-github-writer-is-isolated.md)

> Amended in v2.0: there is no second token holder. Every credential, the GitHub
> token included, is one key of the Credentials Secret ([ADR-0033](0033-one-credentials-secret.md)),
> and the write capability is held only by the isolated writer in
> `internal/github/pr` ([ADR-0043](0043-the-github-writer-is-isolated.md)); the
> Executor the write token existed for is gone
> ([ADR-0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md)). Reads stay
> read-only.

## Context

The agent and the server need read access to the Sandbox, and the Executor needs write access. A GitHub App provides short-lived installation tokens, but every user would have to create and install one manually.

## Decision

GitHub access uses two fine-grained personal access tokens scoped to the user's Sandbox fork: a read-only token for the agent and the server, and a write token for the Executor. `make up` prompts for both and stores them in a git-ignored `.env` file.

## Consequences

- Setup takes minutes, which preserves the single-command quickstart.
- The tokens are long-lived, so users should set an expiry. A GitHub App remains the recommended design for production use.
