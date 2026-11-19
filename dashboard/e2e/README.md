# Dashboard smoke test

`run.sh` proves the acceptance path end to end: a browser signs in through a real Keycloak, the dashboard streams
the agent's steps, and Approve records the Keycloak user as the approver.

It starts three things:

1. Keycloak 26.8.2 in Docker, with `hack/provision-keycloak.py`'s dashboard client shape, the
   `pagerless-approvers` group, and one generated approver. `keycloak.py` provisions the realm over the admin API
   and writes the credentials to a mode-0600 file; nothing is printed.
2. The envtest API server, the admission webhooks, and `pagerless-server` with the real `dashboardauth` flow, via
   the `smoke`-tagged `internal/server` test. A stand-in agent records twenty steps and a NoAction Proposal.
3. Chromium driven by `@playwright/test`, via `e2e/smoke.spec.ts`. The browser starts the Page from the Start
   Page view, waits for a step that arrives after the first snapshot, approves, and checks the recorded approver.

## Prerequisites

- Docker, Go, Node 22.17.1, and Python 3.
- The envtest binaries: `go tool setup-envtest use -p path 1.37.0` (the runner runs it).
- The Chromium build: `(cd dashboard && npx playwright install chromium)`.
- Dashboard dependencies: `(cd dashboard && npm ci)`.

## Run

```bash
bash dashboard/e2e/run.sh
```

The runner refuses to start if port 18080 (Keycloak) or 18082 (dashboard) is in use, removes its container and
temporary state on exit, and prints the server log only when the test fails. It starts no kind cluster and makes no
model calls.

The screenshot and trace of a failure are kept in `dashboard/test-results/`, which is not committed.
