# ADR-0017: A local adapter tests Sandbox integration without GitHub credentials

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

The end-to-end harness must exercise the deployed pagerless-server, Executor,
agent, GitHub MCP server, and Argo CD without using a user's GitHub token or
mutating a real Sandbox fork. The production path uses GitHub REST and GraphQL,
Git smart HTTP, protected pull request merging, and an Argo CD repository
credential. A test that replaces the MCP server, directly edits a local
checkout, or stubs the Executor would skip those integration boundaries.

## Decision

The end-to-end environment uses a test-only adapter backed by a bare Git
repository initialized from a pinned known-good Sandbox commit. It serves Git
smart HTTP over HTTPS with an E2E-only CA and implements only the GitHub REST
and GraphQL operations required by Pagerless. Disposable read and write tokens
are distinct and generated per run. Read-only consumers receive only the read
token; the Executor and the host-side Scenario client use the write token.

The adapter records an open pull request, accepts the same squash auto-merge
mutation used by production, and performs a real squash merge in Git only after
the host harness has run the pinned strict manifest check. A separate,
loopback-only control listener exposes the check and reset operations. The
original GitHub MCP binary is retained; an E2E-only relay in its Pod forwards
its loopback GitHub Enterprise API requests to the adapter over HTTPS and
trusts only the generated test CA.

## Consequences

- The harness exercises Pagerless' production binaries, MCP protocol and
  server, Git transport, Argo CD repository access, and real Git merge behavior
  against the adapter.
- It does not test GitHub.com's external API implementation, GitHub-hosted
  branch protection, GitHub Actions, or the behavior of a user's fork. Results
  must be described as adapter-backed integration coverage.
- The adapter is test-only, isolated in the E2E namespace, and uses no real
  GitHub credentials. Its reset operation restores only its own bare
  repository to the pinned base commit.
- The adapter's API surface must remain limited to routes observed in the
  production integration. Expanding it requires tests that exercise the
  corresponding production call path.
