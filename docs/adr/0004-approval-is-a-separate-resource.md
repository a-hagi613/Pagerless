# ADR-0004: Approval is a separate resource with webhook-stamped identity

- **Status:** Accepted; the impersonation clause is superseded by [ADR-0020](0020-dashboard-approvals-assert-the-approver.md)
- **Date:** 2026-11-16

## Context

An Approval must record who approved in a way that cannot be forged, and must bind to one exact Proposal. ADR-0001 prohibits humans from writing Triage status.

## Decision

A human approves or rejects by creating a `TriageApproval` resource containing the Triage reference, the Proposal hash, and the decision. A validating admission webhook records the requester's identity from the admission request, and rejects the resource if the hash does not match the current Proposal or the Triage is not in the `Proposed` phase.

The dashboard creates the same resource by impersonating the user authenticated through Keycloak.

**Superseded by [ADR-0020](0020-dashboard-approvals-assert-the-approver.md):** the dashboard asserts the approver on the TriageApproval under the `pagerless-server` service account instead of impersonating the user, so the impersonation permission described below is no longer required.

## Consequences

- Kubernetes RBAC controls who may approve.
- Approval works with plain `kubectl` as well as with the plugin and the dashboard.
- `pagerless-server` requires impersonation permissions, which must be tightly scoped.

## Alternatives considered

- **Patch an approval field on the Triage.** Violates ADR-0001, and the field cannot prove who wrote it.
