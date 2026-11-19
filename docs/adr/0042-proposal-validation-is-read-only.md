# ADR-0042: Proposal Validation is read-only and does not simulate admission

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

v1 rendered each change and sent it to the API server with `DryRunAll`, which required `get` and `update` on Deployments and StatefulSets and a dedicated policy to bound that grant. That grant was the only write authority the cluster identity held, and it was there only to answer a question the repository's own Checks answer better: whether the manifest is acceptable.

Admission, quota, and defaulting are cluster policy, not repository truth. A dry run against the live cluster also makes Validation depend on the cluster being reachable, and it evaluates the change against the cluster's current webhooks rather than against the Target Repository's review process.

## Decision

Proposal Validation is deterministic and read-only. It runs cheap checks before expensive ones:

1. File count, path, size, digest, and base-blob bounds.
2. The path policy: no repository automation, CI configuration, chart wiring, Kustomize wiring, templates, CRDs, admission webhooks, RBAC, service accounts, Secrets, encryption configuration, namespace and quota objects, network policies, or kubeconfigs.
3. Strict YAML with document, node, and depth budgets: no duplicate keys, no unknown tags, no aliases.
4. A kind allowlist with an always-denied set.
5. Identity preservation: the document set is unchanged, and `apiVersion`, `kind`, `metadata.name`, and `metadata.namespace` are byte-identical to the base.
6. Forbidden differences, and permitted ones recorded as warnings.
7. The API server's own `GET /openapi/v3` schemas, cached per group and version, with unknown fields refused as fields the API server would prune. A schema that is unavailable degrades the Proposal instead of blocking it.
8. The chart's `values.schema.json` for a Helm values edit.
9. The derived Diff bounds, because the server derives the Diff and a model-supplied patch cannot be inconsistent with the base.

The package contains no mutating verb and no dry run. The Validation report says in one line that admission, quota, and defaulting were not evaluated and that the repository's Checks are authoritative.

## Consequences

- The cluster identity needs no write grant, and Validation works without a cluster.
- A degraded Proposal is approvable but never eligible for an unattended draft.
- A field the API server would prune is an error rather than a silent drop, because a human reviews the exact manifest.
