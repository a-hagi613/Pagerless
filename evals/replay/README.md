# OOMKilled Replay capture

The production `oom.json` artifact must contain model responses from a real Anthropic API investigation of the OOMKilled Scenario. A successful live capture also produces `oom.provenance.json`, which binds the artifact's digest to the agent Job and the server-validated PatchResources Proposal. The repository contains that validated pair; this document describes how it was produced and how to reproduce it. The separate `evals/e2e/fixtures/synthetic-oom.json` file is a development fixture and is never evidence of a live run.

Run the helper with the agent's pinned Python 3.14 environment and `kubectl` and Docker on `PATH`. It operates on an existing single-node kind stack and never creates a cluster or changes the remote Sandbox. In the isolated end-to-end environment, the Workload, Argo CD, Git repository and upstream MCP services remain real; the local GitHub adapter replaces the external GitHub API and pull request service.

## Prepare without making a model call

```bash
python evals/replay/capture.py prepare --context kind-pagerless
```

This reads `ANTHROPIC_API_KEY` from the ignored root `.env` as data and sends it through stdin to the namespace-scoped `pagerless-agent-credentials` Secret. The key is never put in command arguments or written to another local file. The helper creates the `pagerless-replay-capture` persistent volume and claim, backed by a directory inside the kind node and owned by UID/GID 65532. A restricted helper Pod then writes, reads, and removes a probe file using the same UID/GID and filesystem group as the agent. Preparation must succeed before the paid stage.

## Run one live investigation

Before starting the paid stage, confirm that the OOMKilled Scenario is present, the upstream MCP services are reachable, and no investigation of the Workload is active. The coordinator configures the server's runtime ConfigMap with:

- `PAGERLESS_MODEL_PROVIDER=anthropic`
- `PAGERLESS_RECORDING_PVC=pagerless-replay-capture`
- `PAGERLESS_RECORDING_FILE=/recordings/oom.json`

Restart the server to load that configuration, then start one Triage for the failing `sandbox/checkout` Deployment. The existing agent limits remain 15 tool calls, five minutes and 100,000 input tokens. Do not repeat a failed live investigation automatically; inspect and report its cause first. The helper commands themselves never call a model.

## Retrieve and validate

Once the agent Job succeeds and the Triage is Proposed:

```bash
python evals/replay/capture.py retrieve --context kind-pagerless
python evals/replay/capture.py provenance --context kind-pagerless \
  --triage <live-triage-name> --source-revision <full-git-commit>
python evals/replay/capture.py validate
```

Retrieval uses a restricted, non-root helper Pod that mounts the claim read-only. It captures that Pod's output directly into memory, checks normalization, the expected model and Action, input/tool budgets, and absence of the configured API key before replacing `oom.json`. No `kubectl exec` or `kubectl cp` from the completed agent container is needed. The helper Pod is deleted afterwards.

Clear the recording file and PVC settings, restore `PAGERLESS_MODEL_PROVIDER=replay`, and restart the server. Rebuild and load the agent image with the retrieved artifact at `/app/recordings/oom.json`. Run Replay against another injection of the real local Scenario to prove the placeholders resolve with fresh pod names and commit identifiers. That validation makes no paid model calls.

The capture claim is deliberately separate from the normal Replay input path. The kind node owns the retained local volume; tearing down that cluster removes its underlying storage. Keep the validated repository artifact before teardown. No credential belongs in the recording, provenance, or commits.
