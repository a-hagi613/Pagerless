# Credential-free end-to-end harness

`make e2e` runs the OOMKilled Scenario with the synthetic Replay fixture in a
dedicated kind cluster. Set `PAGERLESS_E2E_SCENARIO` to run another Scenario
that `make break` accepts; the harness plays `fixtures/synthetic-<scenario>.json`
unless `PAGERLESS_E2E_RECORDING` names another recording in that directory, so
`PAGERLESS_E2E_SCENARIO=injection` runs the prompt-injection Scenario end to end.
The harness uses separate generated read and write
tokens, a per-run TLS CA, and a test-only GitHub-compatible adapter. It invokes
the ordinary `make break` path, validates the Sandbox manifests with pinned
kubeconform v0.8.0 before allowing the adapter's required check to pass, and
approves the resulting Proposal with `kubectl triage approve`. The host then
checks out the Executor pull request's exact head with the read-only adapter
token, validates its manifests, and signals success for that SHA. The adapter
refuses a check for a changed head and merges only the checked commit. The harness verifies the
merge SHA, Argo CD health at that SHA, the Recovered phase, the full five-minute
Verification window, and every deterministic Verification check.

The fixtures under `fixtures/` are test data. They are not live recordings and
are never copied to `/app/recordings/oom.json`. This test exercises
Pagerless integrations against the adapter; it does not test GitHub.com's
external API, GitHub-hosted branch protection, or GitHub Actions.

By default the harness creates and deletes its dedicated kind cluster and its
temporary state directory. Set `PAGERLESS_E2E_KEEP_CLUSTER=1` to retain the
healthy, reset Sandbox and the generated state for a coordinated live capture.
The retained mode prints the state directory; it contains disposable adapter
tokens and TLS private material and must be kept private. Set
`PAGERLESS_E2E_REUSE_CLUSTER=1` only when reusing the isolated Pagerless E2E
cluster. A local run must set `PLATFORM_REVISION` to the full SHA of the exact
commit already pushed to the public repository; CI uses its triggering commit
SHA. The runner never silently targets `main` and never reads the repository's
`.env` file.
The supplied SHA also labels the local image builds; run from a clean checkout
at that exact commit so the image metadata and deployed manifests agree.

Bootstrap creates the adapter's TLS Secret before its first rollout and installs
the generated CA for Argo CD. The Executor receives the explicit repository
identity before its initial health wait. Server and Executor TLS trust and the
GitHub MCP loopback relay are overlaid after the standard bootstrap converges.
Relay probes run inside its container so the API listener stays loopback-only.

After `make dev` converges, the runner saves and pauses automated sync for the
Argo CD `root`, `pagerless-server`, `pagerless-executor`, and `mcp` Applications
before applying the E2E TLS and relay overlay patches. The `sandbox` Application
keeps automated sync, so the tested merge is still applied by Argo CD. A
default run deletes its cluster, restoring the unmodified Argo state. A
retained run leaves those four Applications paused for the coordinated capture;
after capture, restore their saved policies with
`bash evals/e2e/restore.sh <retained-state-directory>`.

Failure cleanup terminates the observer and its Kubernetes watch. When reusing
an existing E2E cluster without retained mode, cleanup restores saved automation
policies even after a failed check. Recovery files are preserved if restoration
fails. These runs use locally built images; the release workflow's publication
and anonymous GHCR access require separate verification.
