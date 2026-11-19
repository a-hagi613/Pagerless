# Dashboard browser tests

The suite drives the production build in Chromium against an offline mock of `pagerless-server`. Nothing
here reaches a cluster, a model, a container runtime, or the network.

`playwright.config.ts` starts two processes:

1. `node e2e/mock-api.mjs` on `127.0.0.1:8092`. It serves deterministic Pages, the cluster matrix, the
   settings view, CSRF-checked decisions, and a real Server-Sent Events trace that sends two further steps
   and a phase change while the browser watches. `POST /__test/reset` and `POST /__test/ownership` exist
   only in the mock, so each test starts from the same fixtures and the read-only external-credential state
   can be exercised.
2. `vite preview --port 8091`, serving `dist/` and proxying `/api/*` to the mock through
   `preview.proxy` in `vite.config.ts`.

## What the suite proves

`e2e/portal.spec.ts` runs at `1440x900` and `390x844`:

- Sign-in: a wrong token is refused inline, the bootstrap token opens the Portal, and `auth.mode: disabled`
  needs no form.
- The namespace tile and node row filter the inbox, and the brand lockup clears namespace, node, severity,
  and expansion state.
- A card expands into root cause, Findings, the streaming trace, and the stored Proposal diff; the trace
  grows over Server-Sent Events and the Phase moves without a reload.
- Approve binds the displayed Proposal hash and links the resulting draft PR; Dismiss records `Rejected`.
- `/settings` is a route: a save reports success, and an external Secret renders every field read-only with
  `managedBy: external` while the write is refused inline.
- The CLI drawer lists every Portal verb plus a "This Page" tab for `show`, `diff`, `approve --create-pr`,
  and `dismiss`.
- Light, Dark, and System persist across a reload, follow `prefers-color-scheme` while System is selected,
  and never leave the wrong palette on first paint.
- Zero console errors (the two documented 401s from the signed-out probe and the deliberate wrong token are
  excluded by URL) and zero horizontal overflow at both viewports.

## Run

```bash
(cd dashboard && npm ci && npm run build)
(cd dashboard && npx playwright install chromium)   # once
(cd dashboard && npm run test:e2e)
```

The screenshot and trace of a failure are kept in `dashboard/test-results/`, which is not committed.
