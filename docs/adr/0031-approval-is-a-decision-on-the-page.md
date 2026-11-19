# ADR-0031: An approval is a decision recorded on the Page

- **Status:** Accepted
- **Date:** 2026-11-18
- **Supersedes:** [ADR-0004](0004-approval-is-a-separate-resource.md) and [ADR-0020](0020-dashboard-approvals-assert-the-approver.md).

## Context

v1 expressed an approval as a separate `TriageApproval` resource. A mutating
admission webhook stamped the requester's identity into it and a validating
webhook bound it to the current Proposal and checked the approver's group
membership, which in turn required a Keycloak client. Two validating webhooks
also guarded the Triage itself: one refused a second Active Triage for a
Workload, and one refused a status write that did not come from the server.

A `MutatingWebhookConfiguration`, a `ValidatingWebhookConfiguration`, their
serving certificate, and the certificate bootstrap are cluster-scoped objects.
They are the reason a v1 installation needed more privilege than reading a
cluster and writing its own namespace. The standalone chart must install with
nothing more than CRDs, one read ClusterRole, and namespaced objects, so the
webhooks cannot stay.

The trade-off is explicit: without a webhook-stamped identity, the recorded
approver is whoever the caller declares, and its trust is exactly the caller's
RBAC on Pages. That is the intended authority model for one operator.

## Decision

There is no approval resource and no admission webhook.

- `TriageApproval` is deleted. An approval, a rejection, and an acknowledgement
  of a no-change recommendation are recorded in `status.decision`.
- A human decision is written as annotations on the Page:
  `pagerless.dev/decision=approve|reject`,
  `pagerless.dev/decision-proposal=<hash>`,
  `pagerless.dev/decision-reason=<text>`, and
  `pagerless.dev/decided-by=<identity>`.
- `pagerless-server` reconciles those annotations into `status.decision`. It
  ignores a decision whose hash does not match the current Proposal, that
  arrives after the approval window, that declares no identity, or that targets
  a Proposal which has become stale. A Page that already holds a decision is no
  longer `Proposed`, so a replay is ignored.
- The `pagerless-approver` Role grants `get`/`list`/`watch` and `patch` on
  `pages` in the release namespace. `patch` is the only write an approver holds;
  `spec` is immutable and `status` is a separate subresource, so a decision
  cannot change the investigation.
- Canonical Active Page uniqueness is enforced by the single-writer server: it
  selects one Page per Fingerprint by creation time, then name, then UID, and
  marks the others `Superseded` deterministically on every resync. A duplicate
  that races the selection is repaired, not refused at admission.
- `spec` immutability is a CEL rule on the CRD, which needs no webhook.

## Consequences

- The chart installs zero `MutatingWebhookConfiguration`,
  `ValidatingWebhookConfiguration`, and `ValidatingAdmissionPolicy` objects, and
  the server serves no webhook port.
- Anyone who can `patch` Pages in the release namespace can record a decision,
  and can edit unrelated Page annotations. For a single-operator installation
  that is the intended authority; a multi-operator installation must bind the
  approver Role deliberately and accept that attribution is declarative.
- `decidedBy` is an audit record, not an authenticated identity. The Portal
  writes the signed-in subject; the CLI writes the kubeconfig user; a hand
  `kubectl annotate` writes whatever it says.
- `internal/keycloak` is removed with the onboarding slice ([ADR-0032](0032-single-operator-local-authentication.md)). The
  chart no longer requires an identity provider.
- The agent-Job admission policy that bounded the Job's shape is gone with the
  webhooks; the Job builder and its tests are now the only enforcement.

## Alternatives considered

- **Keep the webhooks and install them with high privilege.** They buy
  admission-stamped identity and admission-level duplicate refusal, at the cost
  of the one-command, least-privilege install that is the product's premise.
- **A `PageApproval` resource without webhooks.** It adds a second CRD, a second
  retention rule, deterministic name hashing, a second watch, and a
  cross-object consistency check to express one boolean that a `patch` on the
  Page already grants.
- **A `pages/approval` subresource.** A CRD's only built-in subresources are
  `status` and `scale`; a custom subresource is an aggregated API server.
