# What the demo shows

The README links two recordings made against the 1.0.0 distribution. Both are real investigations:
the question is typed into the live client, the model makes its own tool calls, and the answer and
proposal are whatever the installation returned. Nothing is a stored transcript and nothing is
sped up.

## Recorded environment

| Item | Value |
| --- | --- |
| Distribution | Pagerless `1.0.0`, fresh release coordinates |
| Cluster | kind `v0.33.0`, one node, Kubernetes `1.37.0` |
| Helm | `4.3.2` |
| Namespace and release | `pagerless-live` |
| Dashboard | `kubectl port-forward service/live-pagerless-server 18430:8080` |
| Model | OpenAI-compatible endpoint `https://api.anthropic.com/v1`, model `claude-haiku-5-5` |
| Source repository | a private acceptance repository with an inventory workload |
| Question | "Why is the inventory Deployment not ready? Cite the evidence and do not propose a source change." |

The acceptance repository and its draft pull request are private by design. The recordings show the
in-app evidence and the proposed diff, so a viewer does not need access to the repository to see
the result.

## Dashboard recording

Duration **39 seconds**, 1000x900, H.264.

1. A completed investigation opens on its **Proposed change** panel. The panel shows the verified
   base, the changed file and the one-line diff (`inventory-setting` to `inventory-settings`), plus
   the **Open draft pull request** action.
2. **New investigation** opens the composer. The namespace hint `inventory` is filled; the
   resource kind and name fields still show the composer's placeholder examples (`Deployment`,
   `checkout`) and are not submitted. The question is typed at human speed and names the inventory
   Deployment itself.
3. The page switches to **Running**. Each read-only tool attempt appears in the activity list with
   its call ID, status, duration and evidence control. One read fails because the referenced
   ConfigMap does not exist; the turn continues and uses the successful reads.
4. The first citation opens the **Tool evidence** dialog with the sanitized arguments and a bounded
   JSON result.
5. The terminal answer shows its citations and the page settles at **Complete** with
   "No source change was proposed."

The final still in the README is the proposal panel; the live instruction in this run explicitly
asked for no source change, so it did not create a duplicate pull request.

## CLI recording

Duration **20 seconds**, 1920x1080, H.264.

1. `kubectl page "Why is the inventory Deployment not ready? ..." -n inventory` is typed into a
   real PTY.
2. The CLI streams the page and turn IDs, then each tool call and its outcome, including the failed
   read of the missing ConfigMap.
3. The answer lists the observed Deployment, Pod, event and ConfigMap evidence with call IDs, then
   the citation list.
4. `kubectl page diff` prints the real proposed change and pull request created by an earlier
   investigation. The diff is legible in the frame; the private pull request URL is not required to
   understand the result.

## Real interaction, deterministic CI

The recordings use a real model and a real GitHub repository. CI uses a deterministic
OpenAI-compatible fixture instead, so every test and build is reproducible without a model key, a
cloud account or a maintainer-owned repository. A green CI badge therefore proves the contract and
the integration suite; it does not claim that a particular model answer is reproducible.

## Privacy and credential handling

- The operator session was created and checked in a separate, unrecorded browser context. No
  sign-in screen, token field or cookie appears in either video.
- The CLI operator token came from the installation Secret on standard input. It was never passed
  as a flag, printed, or written into a frame.
- The model key and the GitHub read and write tokens were never read by the recording scripts and
  never appear on screen.
- Only the operator token was used. The recordings contain cluster state and source diffs from the
  acceptance fixture, not personal chat or credentials.
- The recording scripts are kept private; this page documents the procedure rather than shipping a
  script that embeds an installation's credentials.

## Verification

Each file was checked with `ffprobe`, decoded end to end with `ffmpeg -f null -`, and sampled at
several timestamps to confirm that the question, live tool calls, evidence and result are legible.
The stills in `docs/assets/` are WebP crops of those frames; the README links the full recordings
from the release assets.

To reproduce the setup without the private scripts:

1. Install the chart as described in the [README](../README.md), using a model that supports tool
   calls and a repository you own.
2. Port-forward the dashboard and sign in privately.
3. Start a new investigation with a namespace hint, or run `kubectl page` with `--token-file -`.
4. Record the browser with Playwright's `recordVideo` or the terminal with a real PTY; do not
   replay a stored transcript.
5. Check the result with `ffprobe` and sample frames before publishing.
