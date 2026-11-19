# Portal browser tests

The suite drives the built Portal against a **real** `pagerless-server`: envtest's API server, the
server's own controllers, its dashboard listener, and its bootstrap-token sign-in. There is no mock API;
every Page, tile, trace step, diff, and setting on screen came from `/api/*` or the event stream.

The Go harness in `internal/server/portal_e2e_test.go` starts the server on `127.0.0.1:18082` and runs
Playwright once per viewport (1440×900 and 390×844). It seeds the cluster with two Proposed Pages per
namespace, one of which each project approves and one of which it dismisses, an Investigating Page whose
trace keeps growing, and a Node Page.

```bash
# From the repository root. Builds the dashboard the server embeds, then runs both halves.
make portal-e2e
```

`npm run test:e2e` on its own uses `PAGERLESS_PORTAL_URL` (default `http://127.0.0.1:18082`) and expects
that harness to be running, with `PAGERLESS_BOOTSTRAP_TOKEN` set to the token the harness printed.

The suite fails on any browser console error, on a page that scrolls horizontally, and on any flow whose
Phase, settings, or theme does not change as documented.
