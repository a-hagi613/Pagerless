# ADR-0001: Only pagerless-server writes Page status

- **Status:** Accepted; the resource was renamed from Triage to Page by [ADR-0028](0028-pages-replace-triages.md)
- **Date:** 2026-11-16

## Context

The agent must be read-only, yet its Diagnosis and Proposal have to reach the Triage, and the dashboard needs a live stream of agent steps. Humans, alert receivers, and the watcher also create Pages.

## Decision

The agent holds no Kubernetes write permissions. It sends step events and its final report over HTTP to `pagerless-server`, which authenticates the agent with its projected service account token.

Humans (through the CLI or dashboard) and the Alertmanager receiver may only create a Page, setting its Target, Fingerprint, first Trigger, and an optional Hint. The spec is immutable after creation, enforced by a CEL validation rule. Only `pagerless-server` writes Page status.

## Consequences

- The agent cannot alter a Proposal after Validation.
- A single channel carries both live steps and the final report.
- `pagerless-server` is the single point of write coordination (see ADR-0005).

## Alternatives considered

- **Agent patches `pages/status`.** Simplest, but the agent could rewrite its own validated Proposal.
- **Agent creates an immutable `PageReport` resource.** Safe, but live step streaming would require a second channel.
