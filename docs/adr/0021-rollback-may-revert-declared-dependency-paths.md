# ADR-0021: Rollback may revert commits on a Workload's declared dependency paths

- **Status:** Accepted
- **Date:** 2026-11-16
- **Amends:** [ADR-0008](0008-server-renders-the-change.md)

## Context

ADR-0008 limits Rollback to one of the last 20 commits touching the Workload's own Sandbox path. In the feature-flag Scenarios, the alert fires on the affected service, but the breaking commit edits the shared flagd configuration at a different path, so the correct Rollback would fail Validation.

## Decision

A Workload may declare the Sandbox paths it depends on in a `pagerless.dev/depends-on-paths` annotation. Rollback may revert one of the last 20 commits touching the Workload's own path or any declared dependency path. The revert is rendered, shown and hashed exactly as ADR-0008 describes.

## Consequences

- Shared configuration can be rolled back from a Triage on any Workload that declares it.
- The annotation widens Rollback's reach, so Validation still requires a single commit and the namespace allowlist still applies.
