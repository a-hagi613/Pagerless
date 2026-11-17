# Credential-free end-to-end harness

`make e2e` runs the OOMKilled Scenario with the synthetic Replay fixture in a
dedicated kind cluster. The harness uses separate generated read and write
tokens, a per-run TLS CA, and a test-only GitHub-compatible adapter. It invokes
the ordinary `make break` path, validates the Sandbox manifests with pinned
kubeconform v0.8.0 before allowing the adapter's required check to pass, and
approves the resulting Proposal with `kubectl triage approve`. It verifies the
merge SHA, Argo CD health at that SHA, the Recovered phase, the full five-minute
Verification window, and every deterministic Verification check.

The fixture at `fixtures/synthetic-oom.json` is test data. It is not a live
recording and is never copied to `/app/recordings/oom.json`. This test exercises
Pagerless integrations against the adapter; it does not test GitHub.com's
external API, GitHub-hosted branch protection, or GitHub Actions.

By default the harness creates and deletes its dedicated kind cluster and its
temporary state directory. Set `PAGERLESS_E2E_KEEP_CLUSTER=1` to retain the
healthy, reset Sandbox and the generated state for a coordinated live capture.
The retained mode prints the state directory; it contains disposable adapter
tokens and TLS private material and must be kept private. Set
`PAGERLESS_E2E_REUSE_CLUSTER=1` only when reusing the isolated Pagerless E2E
cluster. The runner never reads the repository's `.env` file.
