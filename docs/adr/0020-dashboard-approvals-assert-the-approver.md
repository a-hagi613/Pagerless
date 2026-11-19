# ADR-0020: Dashboard Approvals assert the approver rather than impersonate

- **Status:** Superseded by [ADR-0031](0031-approval-is-a-decision-on-the-page.md). The Keycloak client this record depends on is removed by the onboarding slice, not by this one.
- **Date:** 2026-11-16
- **Supersedes:** the impersonation clause of [ADR-0004](0004-approval-is-a-separate-resource.md)

> Superseded in v2.0: a decision is an annotation on the Page, and the declaration is trusted exactly as far as the caller's RBAC on Pages. `internal/keycloak` stays in the tree, unused, until the onboarding slice replaces the dashboard sign-in and deletes it. See ADR-0031.

## Context

ADR-0004 had the dashboard create TriageApprovals by impersonating the Keycloak-authenticated user. Kubernetes cannot restrict `impersonate` on users to a set of external identities, so `pagerless-server` would need permission to impersonate any user, including cluster administrators. That component processes untrusted agent output, so a compromise would become cluster-wide privilege escalation.

## Decision

For dashboard decisions, `pagerless-server` creates the TriageApproval under its own service account and sets `spec.approver` from the user's verified Keycloak session. The admission webhook accepts an asserted approver only when the requester is the `pagerless-server` service account and the asserted user belongs to the Keycloak `pagerless-approvers` group. For every other requester, the webhook stamps the requester's own identity as before.

## Consequences

- `pagerless-server` holds no impersonation permission.
- The server is trusted to vouch for dashboard identities, but gains no cluster-wide authority.
- Audit records show both the asserted approver and that the decision arrived through the dashboard.

## Alternatives considered

- **Impersonation, as in ADR-0004.** Rejected because of the escalation path above.
