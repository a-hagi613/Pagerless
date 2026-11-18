# Evaluation results

`make eval` writes one versioned document here per batch, named
`<YYYY-MM-DD>-<commit12>-<evidence>.json`. The format, schema, and publication
rules are in [docs/evaluation.md](../../docs/evaluation.md).

- `evidence` is `live` for a paid provider and `replay` for the credential-free
  adapter subset.
- A later batch for the same date, commit, and evidence mode replaces the entry
  for the same Scenario and repeat rather than appending a duplicate.
- `make eval-table` publishes only live runs from a clean worktree. Replay runs
  are harness evidence, not model-performance evidence.
- A full paid batch runs as five shards, and one merged document is written here
  for the whole batch, not one file per shard.

Do not hand-write a document. The harness reads the Triage, the injected values,
and the gateway audit before it writes a run.
