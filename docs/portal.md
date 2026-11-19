# The Portal

`pagerless-server` embeds a React single-page application and serves it on its
dashboard listener. The Portal is a view over the same `Page` resources that
`kubectl page` reads, with the same decision path, so the two surfaces cannot
disagree about what a Page is or who decided it.

The chart does not start the Portal: it starts neither the dashboard listener nor
an identity provider, because the sign-in and Portal slices own them. This page
documents the Portal as this tree serves it, and the contract that the Variant C
Cluster Health Matrix Portal must keep when it replaces the layout.

## Enabling it

The Portal is served only when the server is started with a dashboard listener
and an identity provider:

```
--dashboard-bind-address=:8080
--oidc-issuer=https://sso.example.com/realms/pagerless
--oidc-client-id=pagerless-dashboard
--oidc-client-secret-file=/var/run/pagerless/oidc/client-secret
--dashboard-base-url=https://pagerless.example.com
```

[`docs/auth.md`](auth.md) documents the sign-in flow and what each flag controls.

## Routes

| Route | What it does |
| --- | --- |
| `/` | Lists Pages, most recent first, and refreshes them. It starts from the latest history snapshot of every Page and overlays the live resource, so a terminal Page whose resource the retention controller deleted still appears. |
| `/pages/{name}` | Shows the live steps, Findings with their sources, the Diagnosis, the Proposal with its rendered change, the pull request, the Verification checks, and the Narrative. While the Page is `Proposed` it shows the Approve and Dismiss controls. |
| `/start` | Starts a Page for a chosen Deployment or StatefulSet, with an optional Hint. A Page whose Fingerprint already has an Active Page returns that Page instead of starting a second one. |
| `/login`, `/auth/callback`, `/signed-out` | The sign-in flow, including its failure page. |

`POST /logout` ends the session and redirects to `/signed-out`. There is no other
route: the application renders a not-found page for an unknown path.

## API

| Method and path | Meaning |
| --- | --- |
| `GET /api/session` | The signed-in user, their groups, whether they may approve, and the CSRF token. |
| `GET /api/pages` | Page summaries, filtered by `phase`, `target`, `trigger`, and `since`. |
| `GET /api/pages/{name}` | One Page record, from the live resource or from history. |
| `GET /api/pages/{name}/events` | Server-Sent Events: steps, Phase changes, and a final snapshot, with a comment every 15 seconds so an idle stream stays open. |
| `POST /api/pages` | Start a Page for `{target, hint}`. |
| `POST /api/pages/{name}/decision` | Record `{decision, reason}` on the current Proposal. |
| `GET /api/targets` | The Deployments and StatefulSets that Start Page offers. |

Every write repeats the session's CSRF token in `X-CSRF-Token` and is refused
without it. A decision on a Page that is not `Proposed` is answered with `409`;
one from a user outside the approvers group with `403`.

## Decisions

A Portal decision patches the Page's decision annotations with the signed-in
username, the exact Proposal hash, and the optional reason:

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
[ADR-0031](adr/0031-approval-is-a-decision-on-the-page.md).

## The parity contract

The Portal is never the only way to do something. Every action it offers has a
`kubectl page` equivalent, both write the same fields on the same resource, and
neither is a privileged path around the other:

| Portal action | CLI equivalent |
| --- | --- |
| List and filter Pages on `/` | `kubectl page list` |
| Open a Page on `/pages/{name}` | `kubectl page show <name>` |
| Start a Page on `/start` | `kubectl page deploy/<name>` or `kubectl page sts/<name>` |
| Approve and create the pull request | `kubectl page approve <name>` |
| Dismiss with a reason | `kubectl page dismiss <name> --reason <reason>` |
| Stream an investigation's steps | `kubectl page show <name> --watch` |
| Read a Page after its resource is gone | `kubectl page show <name>` reads the history snapshot |

The contract holds in both directions. The Portal must not add an action the CLI
cannot perform, and a CLI decision must appear in the Portal exactly as a Portal
decision does. A page that breaks the contract is a bug in the Portal, not a
feature of it.

## Access

The Portal reads and writes through the server's controller-runtime client, so it
inherits the server's Role: Pages in the release namespace, and the read identity
for evidence. It never writes Page status; only the reconcilers do. A Portal user
is a Kubernetes-authorized operator through the server's identity, so the Portal
adds authentication and an approver check, not a second authorization system.
