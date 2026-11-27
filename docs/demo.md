# What the demo shows

The README embeds two recordings made against the 1.1.0 distribution. Both are real
investigations: the question is typed into the live client, the model makes its own tool calls,
and the answer and proposal are whatever the installation returned. Nothing is a stored transcript
and nothing is sped up.

## Recorded environment

| Item | Value |
| --- | --- |
| Distribution | Pagerless `1.1.0` from the published chart, including the Markdown dashboard and colour CLI shown here |
| Cluster | kind `v0.33.0`, one node, Kubernetes `1.29.14` |
| Helm | `4.3.2` |
| Namespace and release | `pagerless` |
| Dashboard | `kubectl port-forward service/pagerless-server 18434:8080` |
| Model | OpenAI-compatible endpoint `https://api.anthropic.com/v1`, model `claude-haiku-5-5` |
| Source repository | a private acceptance repository with an inventory workload |
| Question | "Why is the inventory Deployment not ready? Cite the evidence and do not propose a source change." |

The acceptance repository and its draft pull request are private by design. The recordings show the
in-app evidence and the proposed diff, so a viewer does not need access to the repository to see
the result.

## Dashboard recording

Duration **52 seconds**, 1280x900, H.264.

1. A completed investigation opens on its **Proposed change** panel. The panel shows the verified
   base, the changed file and the one-line diff (`inventory-setting` to `inventory-settings`), plus
   the **Open draft pull request** action.
2. **New investigation** opens the composer. The namespace and target hints are filled with
   `inventory` / `Deployment` / `inventory`. The question is typed at human speed.
3. The page switches to **Running**. Each read-only tool attempt appears in the activity list with
   its call ID, status, duration and evidence control. One read fails because the referenced
   ConfigMap does not exist; the turn continues and uses the successful reads.
4. A citation opens the **Tool evidence** dialog with the sanitized arguments and a bounded JSON
   result.
5. The answer is rendered as formatted Markdown: bold headings, bullets, inline code and citation
   chips. The page settles at **Complete** with "No source change was proposed."

The live instruction in this run explicitly asked for no source change, so it did not create a
duplicate pull request. The proposal panel comes from the earlier investigation that did.

## CLI recording

Duration **43 seconds**, 1920x1080, H.264.

1. `kubectl page "Why is inventory stuck before startup? ..." -n inventory` is typed into a real
   PTY.
2. The CLI streams the page and turn IDs, then each tool call and its outcome in colour: green for
   a successful read, red for the failed one, grey for the call ID and duration.
3. The answer renders Markdown headings, bullets and inline code, then lists the citations.
4. `kubectl page diff` prints the real proposed change from the earlier investigation in a
   red/green unified diff and links the private draft pull request.

## Real interaction, deterministic CI

The recordings use a real model and a real GitHub repository. CI uses a deterministic
OpenAI-compatible fixture instead, so every test and build is reproducible without a model key, a
cloud account or a maintainer-owned repository. A green CI badge therefore proves the contract and
the integration suite; it does not claim that a particular model answer is reproducible.

## Privacy and credential handling

- The operator session was created and checked in a separate, unrecorded browser context. No
  sign-in screen, token field or cookie appears in either video.
- The CLI operator token was read from the installation Secret into a private 0600 file and passed
  through `PAGERLESS_TOKEN_FILE`. It was never a flag value, never printed and never written into a
  frame.
- The model key and the GitHub read and write tokens were never read by the recording scripts and
  never appear on screen.
- Only the operator token was used. The recordings contain cluster state and source diffs from the
  acceptance fixture, not personal chat or credentials.
- The recording scripts are kept private; this page documents the procedure rather than shipping a
  script that embeds an installation's credentials.

## Verification

Each file was checked with `ffprobe`, decoded end to end with `ffmpeg -f null -`, and sampled at
several timestamps to confirm that the question, live tool calls, evidence and result are legible.
The stills in `docs/assets/` are WebP crops of those frames; the README embeds the full
recordings and keeps a release-asset copy of each.

## Published 1.0.1 acceptance

The recordings demonstrate the interfaces. The published release was also installed a second time
from its public coordinates, on a fresh kind `v1.29.14` cluster, with empty Docker and Helm
credential stores:

- `helm pull oci://ghcr.io/a-hagi613/pagerless/core/charts/pagerless --version 1.0.1` returned
  chart digest `sha256:ed51f04ff874df591da5b3a3499a018657ace01cc8a7bfaa4a4009e81982d5ff`.
- The running server and agent pods used
  `ghcr.io/a-hagi613/pagerless/core/{server,agent}:1.0.1`, and `kubectl-page 1.0.1` came from the
  release archive after its checksum was verified.
- A real `claude-haiku-5-5` turn answered the inventory question with six citations and proposed
  `inventory-setting` to `inventory-settings` in `apps/inventory/workload.yaml`.
- `kubectl page diff` opened draft pull request #5 against the private acceptance fixture. The
  applied Deployment kept the same UID and spec during the turn: the service had read the cluster,
  not changed it.
- The release workflow's credential-free **Anonymous install gate** passed, and
  `scripts/release/verify-public-access.sh` reported every archive, the chart, and both images
  public without a token. The 1.0.0 release passed the same gate before it.

To reproduce the setup without the private scripts:

1. Install the chart as described in the [README](../README.md), using a model that supports tool
   calls and a repository you own.
2. Port-forward the dashboard and sign in privately.
3. Start a new investigation with a namespace hint, or run `kubectl page` with `--token-file -`.
4. Record the browser with Playwright's `recordVideo` or the terminal with a real PTY; do not
   replay a stored transcript.
5. Check the result with `ffprobe` and sample frames before publishing.
