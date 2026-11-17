# Demo assets

The evidence the README shows, and the tools that produce it. Nothing here is hand-written: the GIF is a recording of a real session and the screenshots are taken by the Playwright smoke test.

| Asset | Produced by | Pinned |
| --- | --- | --- |
| `pagerless.gif` | `vhs docs/demo/pagerless.tape` against a live session | VHS 0.12.1, ttyd 1.7.7, ffmpeg |
| `dashboard-*.png` | `dashboard/e2e/run.sh` with `CAPTURE=1` | the Playwright version in `dashboard/package-lock.json` |

## The terminal demo

`pagerless.tape` runs the change of state that the project exists to show: a Scenario breaks the `checkout` Workload, a Trigger opens a Triage, the agent proposes one Action, a human approves it, the Executor applies it as a Sandbox pull request, and Verification ends in Recovered.

Record it from the repository root while a session is up:

```bash
export PAGERLESS_KUBE_CONTEXT=pagerless-eks
vhs docs/demo/pagerless.tape
```

VHS drives a real terminal, so the recording contains the real latencies, scaled down by `Set PlaybackSpeed`. The scenario must start from a healthy Sandbox: run `make reset` before recording a second time.

## The dashboard screenshots

The Playwright smoke test signs in to the dashboard, starts a Triage and asserts the decision path. With `CAPTURE=1` it also writes the screenshots the README embeds, so they cannot drift from the UI they claim to show.
