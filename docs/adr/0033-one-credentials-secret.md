# ADR-0033: One credentials Secret is the only credential store

- **Status:** Accepted
- **Date:** 2026-11-18
- **Related:** [ADR-0032](0032-single-operator-local-authentication.md), [ADR-0037](0037-read-only-cluster-identity.md)

## Context

Onboarding has two surfaces that must agree. The Portal's settings route writes
the AI endpoint, the Target Repository, and connector tokens; `kubectl page
config` writes the same settings from a terminal or a GitOps pipeline. A third
path is a secret manager that owns the Secret itself. If each surface invented
its own keys or its own write protocol, a Secret written by one would be
incomplete for the other, and a stored credential could leak through a read
response or a log line.

## Decision

There is one Secret and one key per credential.

- The fixed keys are `AI_BASE_URL`, `AI_API_KEY`, `AI_MODEL`, `GITHUB_TOKEN`,
  `GITHUB_REPO`, and the optional `GITHUB_API_URL`. A connector adds
  `CONNECTOR_<UPPER_SNAKE_NAME>_TOKEN`. The chart pre-creates the Secret with
  every fixed key present and empty, so the mounted volume always has a file per
  key and an unconfigured installation is a normal state.
- Both surfaces write through `internal/credentials`. A write reads the Secret,
  replaces only the submitted keys, and updates with the observed resource
  version, retrying once on a conflict. It never creates the Secret and never
  clears a key that was not submitted. It logs field names only.
- The Secret is the release namespace's `pagerless-credentials`. The server's
  only Secret grant is `get`, `update`, and `patch` on that one Secret by
  `resourceNames`; `create`, `list`, and `watch` are never granted. The bootstrap
  token and session signing key Secrets are read from their mounted volumes
  instead, so they need no API grant.
- A settings response returns `{configured, fingerprint, updatedAt}` per key,
  where the fingerprint is the first eight hex characters of the value's
  SHA-256. It never returns a stored credential, even truncated. Values that are
  configuration rather than credentials (`AI_BASE_URL`, `AI_MODEL`,
  `GITHUB_REPO`, `GITHUB_API_URL`) are returned so a form can prefill them.
- Kubernetes keeps no per-key timestamp, so a write records
  `pagerless.dev/written-at.<KEY>` on the Secret. Both surfaces write the same
  annotation, which is how `updatedAt` survives a CLI write or a Portal write
  equally.
- When `credentials.existingSecret` is set, the chart renders no credentials
  Secret and grants the server no Secret RBAC; the server reads the mounted
  volume. Every write then answers `409` with the machine-readable code
  `settings_externally_managed` and the manager's name, the Portal renders the
  fields read-only, and the CLI refuses with exit code 3 without attempting a
  write.

## Consequences

- A Secret written by the Portal and a Secret written by `kubectl page config`
  have identical keys, so either surface can finish an installation the other
  started.
- Rotating one credential is a merged write; it does not disturb the others.
- A missing Secret is a misconfiguration (HTTP `424` for the Portal, an error
  for the CLI), never a silent create that the chart does not own.
- Secret RBAC stays as narrow as ADR-0037 requires while the settings route
  still works.
