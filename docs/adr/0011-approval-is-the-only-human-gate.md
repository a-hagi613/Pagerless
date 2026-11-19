# ADR-0011: Approval is the only human gate

- **Status:** Accepted; its auto-merge mechanism is superseded by [0039](0039-provenance-is-a-recorded-ladder.md)
- **Date:** 2026-11-16

## Context

Pull request Actions could require a second human review on GitHub after Approval in Pagerless. The approver has already reviewed the exact rendered patch (see ADR-0008), and the evaluation harness must run without manual merges.

## Decision

The Executor opens each pull request with auto-merge enabled, and the pull request merges once the Sandbox's `kubeconform` schema check passes on GitHub Actions.

Rejection is terminal. A new attempt is a new Triage, optionally with a Hint.

## Consequences

- The pull request remains as an audit record and as the GitOps change that Argo CD syncs.
- Sandbox branch protection must allow the Executor's pull requests to auto-merge on a passing check.
