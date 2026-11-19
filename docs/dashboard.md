# The embedded Portal

`pagerless-server` embeds a React single-page application and serves it on its
dashboard listener. This page documents the Portal as the v2.0 chart shell leaves
it: the v1 layout, running against the `Page` API. The Variant C Cluster Health
Matrix Portal is delivered by the Portal slice and replaces this layout; until
then the layout is stable and the API below is the contract.

The Portal is served only when the server is started with a dashboard listener
and an identity provider. The v2.0 chart shell starts neither, because the
sign-in and Portal slices own them, so a chart installation exposes the agent
report API and the metrics API only.

## Routes

- `/` lists Pages, most recent first, and refreshes every few seconds. The list
  starts from the latest SQLite snapshot of every Page and overlays the live
  resource, so a terminal Page whose resource the retention controller deleted
  still appears.
- `/pages/{name}` shows the live steps, Findings with their sources, the
  Diagnosis, the Proposal with its rendered change, the pull request, the
  Verification checks, and the Narrative. While the Page is `Proposed` it shows
  the Approve and Dismiss controls.
- `/start` starts a Page for a chosen Deployment or StatefulSet, with an optional
  Hint. A Page whose Fingerprint already has an Active Page returns that Page
  instead of starting a second one.
- `/login`, `/auth/callback`, and `/signed-out` complete the sign-in flow.

## API

| Method and path | Meaning |
| --- | --- |
| `GET /api/session` | The signed-in user, their groups, and the CSRF token. |
| `GET /api/pages` | Page summaries, filtered by `phase`, `target`, `trigger`, and `since`. |
| `GET /api/pages/{name}` | One Page record, from the live resource or from history. |
| `GET /api/pages/{name}/events` | Server-Sent Events: steps, phase changes, and a final snapshot. |
| `POST /api/pages` | Start a Page for `{target, hint}`. |
| `POST /api/pages/{name}/decision` | Record `{decision, reason}` on the current Proposal. |
| `GET /api/targets` | The Deployments and StatefulSets that Start Page offers. |

Every write repeats the session's CSRF token in `X-CSRF-Token` and is refused
without it.

## Decisions

A dashboard decision patches the Page's decision annotations with the signed-in
username, the current Proposal hash, and the optional reason:

```
pagerless.dev/decision=approve
pagerless.dev/decision-proposal=<status.proposal.hash>
pagerless.dev/decision-reason=<reason>
pagerless.dev/decided-by=<username>
pagerless.dev/decision-source=dashboard
pagerless.dev/approver-subject=<subject>
```

`pagerless-server` reconciles them into `status.decision`. A decision on a
Proposal that has changed, expired, or become stale is ignored rather than
applied, and a Page that already holds a decision cannot be decided twice. Only a
member of the approvers group may decide. See
[ADR-0031](../docs/adr/0031-approval-is-a-decision-on-the-page.md).

## Access

The Portal reads and writes through the server's controller-runtime client, so it
inherits the server's Role: Pages in the release namespace, and the read identity
for evidence. It never writes Page status; only the reconcilers do.
