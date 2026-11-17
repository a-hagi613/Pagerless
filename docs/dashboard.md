# Dashboard

`pagerless-server` serves a React dashboard and a read-only HTTP API on the listener given by
`--dashboard-bind-address`, port 8082 in the supplied manifests. ADR-0013 records why the dashboard is a custom
application and why its assets are embedded in the server.

The dashboard has two views:

- **Triage list** (`/`) shows Phase, Workload, Trigger and age, most recent first. The list starts from the latest
  SQLite snapshot of every Triage and overlays the live resource, so a terminal Triage whose resource the retention
  controller deleted still appears, with the Workload and Trigger recorded in history.
- **Triage detail** (`/triages/{name}`) shows the live steps, Findings with their sources, the Diagnosis, the
  Proposal with its rendered change, the decision and execution result, the Verification timeline and checks, the
  Narrative, and the Workload's Grafana metrics and logs.

Both views are keyboard accessible and laid out for desktop widths. The list refreshes every five seconds; the
detail view receives steps and Phase changes over Server-Sent Events.

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
and `npm run dev` in `dashboard/`; Vite proxies `/api` to `127.0.0.1:8082`.

## HTTP API

The API is read-only, has no authentication yet (#29 adds Keycloak sign-in), and is served only for the namespace the server watches.
It reads through the server's controller-runtime client, so it inherits the server's Role and cannot see a Triage
or Workload the server cannot see. It never writes status, history, or the Sandbox.

| Method | Path | Meaning |
| --- | --- | --- |
| `GET` | `/api/triages` | The Triage list, most recent first. |
| `GET` | `/api/triages/{name}` | One Triage's complete record. |
| `GET` | `/api/triages/{name}/events` | The Triage's steps and Phase changes as Server-Sent Events. |

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

```bash
curl -s 'http://localhost:8082/api/triages?phase=Proposed&trigger=Alertmanager&since=24h'
```

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
appears in both the snapshot and a later `step` event.

```bash
curl -N 'http://localhost:8082/api/triages/triage-abc/events'
```

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

## Tests

The API is covered at the envtest seam in `internal/server/dashboard_api_test.go`: list ordering and every filter,
history fallback after deletion, the detail record after the full Triage loop, live step and Phase delivery over
the stream, and the 404 for an absent Triage. `internal/dashboardapi` covers the single-page handler, the Grafana
URLs, and the history-less states; `internal/history` covers the schema version 2 upgrade and the latest-snapshot
query; `dashboard/embed_test.go` checks the embedded build. The Playwright smoke test that signs in through
Keycloak is part of #29.
