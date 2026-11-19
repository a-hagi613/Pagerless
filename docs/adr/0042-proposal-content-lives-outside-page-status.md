# ADR-0042: Proposal content lives beside the Page history, not in Page status

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Page status is bounded at 100 KiB, and it must carry no secret, no raw tool output, and no model output beyond the Diagnosis and Findings. A whole-file Proposal may carry up to five files of 64 KiB each, so storing the new content in status would exceed the bound by a factor of three and would put repository bytes into the object that every Portal read and every history snapshot copies.

## Decision

The Page records each Proposal file's path, base blob, content digest, and rationale. The complete content is held by the server under the Proposal hash, in an additive SQLite table:

```sql
proposal_contents(proposal_hash, path, content, recorded_at)
```

The table is append-only, keyed by `(proposal_hash, path)`, and saving the same Proposal again is a no-op, so a retry after a crash is safe. The Proposal hash covers the digest, not the content, so the Page and the hash agree without the bytes. The same table serves Validation, the Diff, and the writer.

## Consequences

- Page status stays small and carries no repository bytes.
- The writer reads the content it commits from the store, so a restart between Approval and publication is safe.
- The content is retained with the Proposal for the retention period, not forever.
