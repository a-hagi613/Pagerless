# ADR-0012: Explicitly out of scope

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

Several reasonable features would dilute the project's focus on a complete, safe triage loop.

## Decision

The following are out of scope:

- Notifications (Slack, email, PagerDuty)
- Multiple clusters
- Multi-tenancy, and RBAC beyond controlling who may approve
- High availability for Pagerless itself
- Auto-approval policies

Terminal Triage resources are deleted after seven days. SQLite holds the permanent history.

## Consequences

- `kubectl triage list` shows only retained Triages; the dashboard shows the full history.
