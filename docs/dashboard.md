# Dashboard

`pagerless-server` serves a React dashboard and its HTTP API on the listener given by
`--dashboard-bind-address`, port 8082 in the supplied manifests. ADR-0013 records why the dashboard is a custom
application and why its assets are embedded in the server, and ADR-0020 records why dashboard decisions assert the
approver instead of impersonating the user.

The dashboard has three views:

- **Triage list** (`/`) shows Phase, Workload, Trigger and age, most recent first. The list starts from the latest
  SQLite snapshot of every Triage and overlays the live resource, so a terminal Triage whose resource the retention
  controller deleted still appears, with the Workload and Trigger recorded in history.
- **Triage detail** (`/triages/{name}`) shows the live steps, Findings with their sources, the Diagnosis, the
  Proposal with its rendered change, the Approve and Reject controls while the Triage is Proposed, the decision and
  execution result, the Verification timeline and checks, the Narrative, and the Workload's Grafana metrics and logs.
- **Start Triage** (`/start`) starts a Triage for a chosen Deployment or StatefulSet, with an optional Hint.

Both reading views are keyboard accessible and laid out for desktop widths. The list refreshes every five seconds;
the detail view receives steps and Phase changes over Server-Sent Events.

## Sign-in

The dashboard is served only to a signed-in user. `pagerless-server` performs the OpenID Connect authorization-code
flow with PKCE against the Keycloak realm:

1. `GET /login` creates a state, a nonce, and a PKCE verifier, stores them in a short-lived HTTP-only cookie scoped
   to `/auth`, and redirects the browser to Keycloak.
2. `GET /auth/callback` checks the state, exchanges the code with the verifier and the client secret, and verifies
   the ID token's signature, issuer, audience, expiry, and nonce.
3. The server creates a session and sets `pagerless_session`: an opaque, random identifier, `HttpOnly`,
   `SameSite=Lax`, `Path=/`, and `Secure` when `--dashboard-cookie-secure` is true. Tokens never leave the server.
4. `POST /logout` requires the session's CSRF token, deletes the session, and redirects to `/signed-out`.

Sessions live in memory for `DefaultSessionTTL` (eight hours). `pagerless-server` runs one replica (ADR-0001), so a
restart ends every session; users sign in again.

Every write repeats the session's CSRF token in the `X-CSRF-Token` header. `GET /api/session` returns the signed-in
username, groups, whether the user may approve, and that token. The sign-in routes and the `/assets/` bundle are the
only paths served without a session; an unauthenticated API request is answered with `401` and a page request is
redirected to `/login`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dashboard-base-url` | `http://localhost:8080` | The browser-facing base URL; the callback is this URL plus `/auth/callback`. |
| `--dashboard-cookie-secure` | `true` | Mark cookies `Secure`. The supplied manifests set it to `false` because the local port-forward is plain HTTP. |
| `--oidc-issuer` | `http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless` | The realm issuer. |
| `--oidc-client-id` | `pagerless-dashboard` | The confidential client that completes sign-in. |
| `--oidc-client-secret-file` | `/var/run/pagerless/keycloak/client-secret` | The client secret, read on every token request. |
| `--approvers-group` | `pagerless-approvers` | The Keycloak group whose members may approve. |

## Decisions

Approve and Reject create a `TriageApproval` under `pagerless-server`'s service account with `spec.approver` set to
the signed-in user's Keycloak `preferred_username`. Two annotations keep the audit trail complete:
`pagerless.dev/decision-source: dashboard` and `pagerless.dev/approver-subject` with the Keycloak subject. The
validating admission webhook accepts an asserted approver only when the requester is that service account and
Keycloak reports the user as a member of `pagerless-approvers`; every other requester is stamped with its own
identity, as the CLI always was. The server holds no impersonation permission.

The decision is bound to the Proposal hash, so a Proposal that changes, expires, or becomes stale before admission
is refused, and the page shows why. A rejection is final for its Triage.

## Start Triage

`GET /api/workloads` lists the Deployments and StatefulSets in the namespaces given by `--workload-namespaces`.
`POST /api/triages` creates a Triage for one of them with `spec.trigger.source: Dashboard` and the optional Hint.
A Workload has at most one Active Triage (ADR-0018), so when one exists the answer is `200` with
`{"alreadyActive": true, "name": "<existing>"}` and nothing is started; the page links to the existing Triage. A
Workload that does not exist is refused with `404` before any Triage is created.

## HTTP API

The API is served only for the namespace the server watches and only to a signed-in user. It reads through the
server's controller-runtime client, so it inherits the server's Role and cannot see a Triage or Workload the server
cannot see. Writes run as the server's service account and pass through the same admission webhooks as any other
client. It never writes Triage status, history, or the Sandbox.

| Method | Path | Meaning |
| --- | --- | --- |
| `GET` | `/api/session` | The signed-in user, approver status, and CSRF token. |
| `GET` | `/api/triages` | The Triage list, most recent first. |
| `GET` | `/api/triages/{name}` | One Triage's complete record. |
| `GET` | `/api/triages/{name}/events` | The Triage's steps and Phase changes as Server-Sent Events. |
| `POST` | `/api/triages` | Start a Triage for a chosen Workload. |
| `POST` | `/api/triages/{name}/decision` | Approve or reject the current Proposal. |
| `GET` | `/api/workloads` | The Workloads that Start Triage offers. |

Writes require `Content-Type: application/json` and the `X-CSRF-Token` header. An error is
`{"error": "..."}` with `400` (malformed request), `401` (no session), `403` (no CSRF token or not an approver),
`404` (unknown Triage or Workload), `409` (the Triage is not Proposed, or a decision already exists), or `422` (the
admission webhook refused the decision, for example because the Proposal is stale or expired).

### `GET /api/triages`

Query parameters, all optional and exact matches:

| Parameter | Values | Meaning |
| --- | --- | --- |
| `phase` | A Phase such as `Proposed` | Only Triages in the Phase. |
| `workload` | A Workload name | Only Triages for the Workload. |
| `trigger` | `Alertmanager`, `CLI`, `Dashboard`, `FollowUp` | Only Triages started by the Trigger source. |
| `since` | RFC 3339 time or a Go duration such as `24h` | Only Triages created at or after the bound. |

The answer is `{"triages": [...]}`. Each row carries `name`, `namespace`, `phase`, `workload`, `trigger`,
`createdAt`, optional `startedAt`, `terminalAt`, `action` and `followUpTriage`, and `source`, which is `live` when
the resource exists and `history` when only SQLite retains the Triage. An unknown `phase`, `trigger` or `since`
value is refused with `400` and `{"error": "..."}`.

### `GET /api/triages/{name}`

The answer is the Triage record: the spec's Workload, Trigger, Hint and Follow-up reference; the Phase and
lifetime times; the steps; the Findings; the Diagnosis; the Proposal, including `renderedChange.path` and
`renderedChange.patch`; the Decision; the Execution; the Verification timeline and checks; the Narrative; and
`grafana`, which carries the embedded panel and deep-link URLs. `source` is `live` or `history`. A name that neither
the resource nor history knows is answered with `404`.

### `GET /api/triages/{name}/events`

The response is `text/event-stream`. The first event is `snapshot`, whose data is the same record as
`GET /api/triages/{name}`. Later events are:

| Event | Data | Meaning |
| --- | --- | --- |
| `step` | `{"step": {...}, "finding": {...}?, "stepCount": n}` | One step event as the server records it. |
| `phase` | `{"phase": "...", "terminal": false}` | A Phase change. |
| `end` | `{"phase": "...", "terminal": true}` | The Triage reached a terminal Phase; the server closes the stream. |

A comment line every fifteen seconds keeps an idle connection open. A slow client may miss an event rather than
block the server's status write; reconnecting delivers a fresh snapshot. A client should deduplicate a step that
appears in both the snapshot and a later `step` event. The stream authenticates from the session cookie, which the
browser sends on the same origin, so no token appears in the URL.

### `POST /api/triages/{name}/decision`

```json
{"decision": "Approved", "reason": "the memory limit is below the allocation"}
```

`decision` is `Approved` or `Rejected`; `reason` is optional and at most 1000 characters. The answer is `201` with
`{"name": "<TriageApproval>", "triage": "...", "decision": "...", "approver": "<Keycloak username>"}`. The Triage's
Phase changes asynchronously as the server applies the decision, so the page refreshes after a successful answer.

### `POST /api/triages`

```json
{"workload": {"kind": "Deployment", "namespace": "sandbox", "name": "checkout"}, "hint": "check the database pool"}
```

`kind` is `Deployment` or `StatefulSet`, the namespace must be one Start Triage offers, and the Hint is at most 1000
characters. The answer is `201` with `{"name": "<Triage>", "alreadyActive": false}`, or `200` with
`{"name": "<existing>", "alreadyActive": true}` when the Workload already has an Active Triage.

## Grafana

The detail view embeds two Grafana dashboards in iframes and always shows a deep link beside each panel. When
embedding is unavailable — Grafana denies framing, or its sign-in is required — the deep link opens the same
dashboard, Workload variables and time range directly.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--grafana-url` | `http://localhost:3000` | The Grafana base URL that the browser can reach. Empty disables the panels. |
| `--grafana-workload-dashboard-uid` | `pagerless-workload` | The dashboard that charts the Workload's metrics. |
| `--grafana-logs-dashboard-uid` | `pagerless-workload-logs` | The dashboard that shows the Workload's logs. |

The two dashboards are provisioned from `deploy/observability/dashboards/workload-metrics.json` and
`workload-logs.json`. They take `namespace` and `workload` variables; the API fills those in, with a time window
that starts fifteen minutes before the Triage and ends five minutes after it, or at the present time while the
Triage is active. The kube-prometheus-stack values set `security.allow_embedding: true`, because Grafana refuses to
be framed otherwise, and pin the Loki and Tempo data source UIDs that the log dashboard references.

Local use assumes the Grafana Service is port-forwarded to `localhost:3000`. The dashboard cannot reach a
cluster-internal Service URL from the browser.

## Build and run

The stack is Vite, React, TypeScript, Tailwind CSS, and shadcn/ui. Node is pinned to 22.17.1 in
[`dashboard/.nvmrc`](../dashboard/.nvmrc) and `dashboard/package.json`; every dependency is pinned exactly in
[`dashboard/package-lock.json`](../dashboard/package-lock.json). TypeScript is pinned to 6.0.3 because
typescript-eslint 8.73 supports TypeScript below 6.1.

```bash
make dashboard-check   # npm ci, then tsc --noEmit and eslint
make dashboard-build   # npm ci, then the production build into dashboard/dist
```

The Go package `dashboard` embeds `dashboard/dist` with `//go:embed all:dist`. The repository commits a marker in
that directory so a fresh checkout compiles; a server built without running the build answers the dashboard with
`503` and the build instruction. `Dockerfile.pagerless-server` builds the application in a `node:22.17.1-bookworm`
stage and copies its output into the Go build, so the published image contains the dashboard.

During development, run the server's dashboard listener on port 8082 (for example with `--dashboard-bind-address`)
and `npm run dev` in `dashboard/`; Vite proxies `/api` to `127.0.0.1:8082`. Sign-in still redirects the browser to
the realm's issuer, so use the local stack's port-forward on port 8080 rather than the Vite dev server when the
flow itself is what you are testing.

## Tests

The API is covered at the envtest seam in `internal/server/dashboard_api_test.go`,
`internal/server/dashboard_write_test.go`, and `internal/server/approval_asserted_test.go`: list ordering and every
filter, history fallback after deletion, the detail record after the full Triage loop, live step and Phase delivery
over the stream, the 404 for an absent Triage, a dashboard Approval and Rejection with the asserted approver, the
CSRF and approver refusals, Start Triage with its duplicate-active behaviour, and the four approver rules
(asserted member, non-server requester, non-member, and the unchanged stamped identity). `internal/dashboardapi`
covers the single-page handler, the Grafana URLs, and the history-less states; `internal/dashboardauth` covers the
authorization-code flow with PKCE, the state and nonce checks, the cookie flags, expiry, and sign-out against a
stand-in provider; `internal/keycloak` covers the membership lookup and its cache; `internal/history` covers the
schema version 2 upgrade and the latest-snapshot query; `dashboard/embed_test.go` checks the embedded build.

The Playwright smoke test signs in through Keycloak, watches live steps, and approves. It needs a realm, a user in
`pagerless-approvers`, and a dashboard the browser can reach; `dashboard/e2e/run.sh` starts all of them and runs
`@playwright/test` in Chromium. See [`dashboard/e2e/README.md`](../dashboard/e2e/README.md).
