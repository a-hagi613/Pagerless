# Pagerless Portal

The Portal is **Variant C: Cluster Health Matrix and Guided Inbox**, rebuilt from the ticket-83 prototype
on the official `shadcn/ui` primitives in `src/components/ui`. It is a static build embedded in
`pagerless-server`; it has no fixture store, no variant switcher, and no prototype tree. Every value it
shows comes from the server.

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
| `GET /api/auth/session` | Session, `csrfToken`, `mode`, `canApprove`. `401` means signed out. |
| `POST /api/auth/login` | `{token}` → signed cookie. |
| `POST /api/auth/logout` | Revokes the session; requires `X-CSRF-Token`. |
| `GET /api/pages` | `{pages: PageSummary[], matrix: ClusterMatrix}`. |
| `GET /api/pages/{name}` | `PageDetail`; authoritative for the Proposal and Findings. |
| `GET /api/pages/{name}/trace` | `text/event-stream`: `step`, `phase`, `done`; the Portal re-reads the detail on `phase`. |
| `POST /api/pages/{name}/decision` | `{decision, proposalHash, reason}`; `409` when the hash is stale. |
| `POST /api/pages` | Opens a Page (`kubectl page <TARGET>`). |
| `GET`/`PATCH /api/settings` | The masked settings view and the write path. `409 credential_writes_disabled` when an external Secret owns the credentials. |
| `POST /api/settings/test` | `{target}` → `{ok, detail}`. |

Two names differ from the wayfinder contract in `wayfinder/80-onboarding-cli-auth.md` §6.4: this build
uses `/api/pages/{name}/trace` for the SSE stream and `/api/pages/{name}/decision` for the decision, per the
ticket. The backend slices (`#91`, `#92`) own the Go handlers and must serve these paths, or the Portal and
the server change together in one commit.

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

`lib/cli-parity.ts` enumerates the 11 `kubectl page` verbs and the 11 Portal paths that perform them, and
`parityReport()` fails when a verb has no Portal path, a Portal path has no verb, or two paths claim one
verb. `lib/cli-parity.test.ts` asserts the bijection. The drawer renders that same manifest, with a "This
Page" tab (`show`, `diff`, `approve --create-pr`, `dismiss`) bound to the expanded Page.

## Commands

```bash
npm ci
npm run typecheck
npm run lint
npm run test          # vitest: filters, diff parsing, CLI parity
npm run build         # also typechecks; writes dist/ for the Go embed
npm run test:e2e      # Playwright against dist via vite preview and the offline mock
```

`npm run dev` proxies `/api` to `PAGERLESS_API_TARGET` (default `http://127.0.0.1:8082`). The Playwright
harness sets it to the mock on `:8092`; see `e2e/README.md`.

## Brand assets

`public/pagerless-mascot.png` and `public/favicon.png` are the regenerated transparent marks;
`docs/assets/pagerless-mascot.png` and `docs/assets/pagerless-icon.png` are the documentation copies. The
40 px mark, the `Pagerless` wordmark, the `>_` chip, and the `cluster pages` subtitle are one `<Link>` home
with an accessible name that also clears every active filter.
