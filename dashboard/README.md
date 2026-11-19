# Pagerless Portal

The Portal is **Variant C: Cluster Health Matrix and Guided Inbox**, built on the official `shadcn/ui`
primitives in `src/components/ui`. It is a static build embedded in `pagerless-server`; it has no fixture
store, no variant switcher, and no prototype tree. Every value it shows comes from the server.

```
src/
  pages/portal.tsx             the cluster matrix, the inbox, inline expansion, decisions
  pages/settings.tsx           /settings: AI endpoint, Target Repository, connectors
  pages/login.tsx              /login: bootstrap-token exchange
  components/                  header, brand lockup, matrix, card, trace, diff, CLI drawer
  components/ui/               shadcn/ui primitives (button, badge, card, dialog, sheet, tabs, switch…)
  lib/api.ts                   the only network boundary
  lib/theme*.ts                Light / Dark / System, persisted and applied before first paint
  lib/filters.ts               namespace / node / severity filtering, severity ordering
  lib/matrix.ts                namespace and node tiles derived from the live Pages
  lib/cli-parity.ts            the Portal-to-CLI parity manifest
  lib/diff.ts                  the unified-diff reader
```

## Routes

| Route | What it is |
| --- | --- |
| `/` | The cluster view: quickstart, health matrix, and the severity-ordered Page inbox. |
| `/settings` | A real route for the AI endpoint, Target Repository, and connectors, reachable from the header and linkable directly. |
| `/login` | The `auth.mode: token` bootstrap-token exchange. `auth.mode: disabled` never reaches it. |
| `*` | A plain not-found page linking back to the cluster view. |

`?theme=light|dark|system` pins the palette for one load (used for the committed screenshots and the
accessibility scan) and is never persisted. `?page=<name>` opens that Page expanded, so a Page is a
shareable link.

## API contract

`src/lib/api.ts` is the single boundary. It talks to these endpoints only:

| Method and path | Use |
| --- | --- |
| `GET /api/session` | Session, `csrfToken`, `mode`, `canApprove`. `401` means signed out. |
| `POST /api/auth/login` | `{token}` → signed cookie. |
| `POST /api/auth/logout` | Revokes the session; requires `X-CSRF-Token`. |
| `GET /api/pages` | `{pages: PageSummary[]}`; each row carries `severity`, `proposalHash`, and `pullRequest`. |
| `GET /api/pages/{name}` | `PageDetail`; authoritative for the Proposal and Findings. |
| `GET /api/pages/{name}/trace` | `text/event-stream`: `snapshot`, `step`, `phase`, `end`; the Portal re-reads the detail on `phase`. |
| `POST /api/pages/{name}/decision` | `{decision, proposalHash, reason}`; `409` when the hash is stale or the Phase moved. |
| `POST /api/pages` | Opens a Page for `{kind, namespace, name, hint}`. |
| `GET /api/targets` | The Deployments and StatefulSets the server can offer. |
| `GET`/`PATCH /api/settings` | The masked settings view and the write path. `409 settings_externally_managed` when an external Secret owns the credentials. |
| `DELETE /api/settings/{key}` | Clears one credential key. |
| `POST /api/settings/test` | `{target}` → `{ok, detail}`; the same probe `kubectl page config test` runs. |
| `GET /api/version` | The server version and authentication mode. |

`/api/pages/{name}/trace` is the server's alias of `/api/pages/{name}/events`; the Portal uses `/trace` and
the two routes are served by the same handler.

## Theme and accessibility

- `index.html` carries no hardcoded `dark` class. A tiny inline script resolves Light, Dark, or System from
  `localStorage` (or `?theme=`) before first paint, applies `.light`/`.dark` and `document.documentElement.style.colorScheme`,
  so no wrong palette flashes.
- The header's three-state control is a `radiogroup`, not a switch. `System` listens to
  `prefers-color-scheme` changes.
- Light: `#F8FAFC` canvas, `#FFFFFF` cards, `#CBD5E1` borders, `#0F172A` ink, `#334155` secondary, `#15803D`
  primary. Dark: `#0F172A` canvas, `#14213D` cards, `#192948` panes, `#3B4D6B` borders, `#F1F5F9` ink,
  `#CBD5E1` secondary, `#22C55E` primary, `#A7F3D0` mint. Both palettes define every token.
- Body and card text sit in a 14–16 px hierarchy with a 12 px floor for functional meta text. Every pill
  carries an icon and a word beside its colour; diff rows carry an explicit `+`/`−` marker.
- `:focus-visible` draws a two-pixel ring in the theme's focus colour on every interactive element.

## CLI parity

`lib/cli-parity.ts` enumerates the twelve `kubectl page` verbs `cmd/kubectl-page` registers and the twelve
Portal paths that perform them, and `parityReport()` fails when a verb has no Portal path, a Portal path
has no verb, or two paths claim one verb. `lib/cli-parity.test.ts` asserts the bijection. The drawer
renders that same manifest, with a "This Page" tab (`get`, `diff`, `approve`, `dismiss`) bound to the
expanded Page.

The verbs are the shipped ones. The ticket's earlier wording (`approve --create-pr`, `decline`) predates
the CLI: approval opens the draft pull request through the Page controller, and `dismiss` is the accepted
synonym of `reject`, so the drawer shows the commands that exist rather than inventing flags.

## Commands

```bash
npm ci
npm run typecheck
npm run lint
npm run test          # vitest: filters, matrix, diff parsing, CLI parity
npm run build         # also typechecks; writes dist/ for the Go embed
npm run test:e2e      # Playwright against a running harness (see e2e/README.md)
```

`make portal-e2e` from the repository root builds the dashboard and runs the Playwright suite against a
real `pagerless-server` on envtest, including its bootstrap-token sign-in. `npm run dev` proxies `/api` to
`PAGERLESS_API_TARGET` (default `http://127.0.0.1:8082`).

## Brand assets

`public/pagerless-mascot.png` and `public/favicon.png` are the regenerated transparent marks;
`docs/assets/pagerless-mascot.png` and `docs/assets/pagerless-icon.png` are the documentation copies. The
40 px mark, the `Pagerless` wordmark, the `>_` chip, and the `cluster pages` subtitle are one `<Link>` home
with an accessible name that also clears every active filter.
