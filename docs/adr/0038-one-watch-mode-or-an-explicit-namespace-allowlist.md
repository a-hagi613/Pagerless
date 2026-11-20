# ADR-0038: One watch mode or an explicit namespace allowlist

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

Pagerless exists to find failures wherever they happen, which argues for one
cluster-wide read role. A platform team that runs a shared cluster often cannot
grant a cluster-wide role to a namespace-scoped tool, and wants the identity
confined to the namespaces it owns. Both requirements are legitimate, and a
configuration that silently satisfies neither is worse than either.

## Decision

The chart renders exactly two watch modes, and they are mutually exclusive:

- **Mode A, `rbac.watchAllNamespaces: true` (default):** one ClusterRole and one
  ClusterRoleBinding grant the read rules cluster-wide.
- **Mode B, `rbac.watchAllNamespaces: false` with a non-empty
  `rbac.watchNamespaces`:** each listed namespace receives an equivalent read
  Role and RoleBinding, with the cluster-scoped read rules omitted: no `nodes`,
  no `namespaces`, and no `/openapi/v3`. The separate agent-auth ClusterRole and
  ClusterRoleBinding (ADR-0037 amendment) still grant `tokenreviews` in both
  modes.

`helm template` fails with a clear message when both are set, or when the
allowlist is empty and cluster-wide watching is off. The chart never renders a
partially applied mode.

## Consequences

- Mode B loses node-pressure evidence, which cannot be namespaced; the Portal
  reports the gap rather than showing an empty Finding.
- Mode B loses `/openapi/v3` schema validation, which is a non-resource URL, so
  a `Role` cannot grant it.
- Agent authentication is identical in both modes: the separate ClusterRole and
  ClusterRoleBinding grant only `create` on `authentication.k8s.io/tokenreviews`,
  which is how the server validates the per-Page agent token and which grants no
  access to any cluster object.
- The release-namespace Role grants `pods` read so that the server can read the
  agent pod that presents a token in either mode.
- The rendered rules are compared against a checked-in allowlist in a Go test,
  so adding a resource is a reviewed act in both modes.

## Alternatives considered

- **Infer the mode.** A non-empty `watchNamespaces` could silently switch to
  Mode B. It would make a partially applied configuration possible, so the
  template refuses instead.
- **One namespaced Role per namespace plus a cluster-wide read role.** It grants
  more than Mode A and less than Mode B, which is the configuration that looks
  safe and is not.
