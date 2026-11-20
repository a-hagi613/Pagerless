# ADR-0037: The cluster identity is strictly read-only

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

v1 mutated workloads. `RestartWorkload` deleted pods live, `Scale` and
`PatchResources` needed an authorized `update` on Deployments and
StatefulSets to perform a server-side `DryRunAll`, and a
`ValidatingAdmissionPolicy` existed only to bound that grant to a dry run. A
read-only watcher that also holds `update` is one bug away from changing a
cluster it promised only to observe, and a `pods/exec` grant is a code-execution
path that no evidence-gathering feature needs.

Remediation in v2 is a pull request. The server never writes a workload, so the
grant that authorised the dry run and the policy that bounded it disappear
together with the live Actions.

## Decision

The server's cluster identity can read, and it can write only its own Pages,
the agent Jobs that investigate them, and the two operator Secrets it owns in
the release namespace.

`get`/`list`/`watch` on: `pods`, `services`, `endpoints`, `configmaps`,
`persistentvolumeclaims`, `events` (`core/v1` and `events.k8s.io/v1`),
`deployments`, `replicasets`, `statefulsets`, `daemonsets`, `jobs`, `cronjobs`,
`endpointslices`, `ingresses`, `nodes`, and `namespaces`. `get` on `pods/log`.
`get` on the non-resource URLs `/openapi/v3` and `/openapi/v3/*`.

`create/get/list/watch/delete/patch` on `pages`, `get/update/patch` on
`pages/status`, and `update` on `pages/finalizers`, in the release namespace.
`create/get/list/watch/delete` on `batch/jobs` in the release namespace.
`get` on two Secrets by `resourceNames` (`pagerless-credentials` and
`pagerless-admin-secret`), and `update`/`patch` on
`pagerless-credentials` only.

The one non-read verb outside the release namespace is `create` on
`authentication.k8s.io/tokenreviews`. It is an authentication request, not a
write to any object, and the server cannot verify an agent Job's projected token
without it.

> **Amendment (2026-11-19):** The `tokenreviews` grant is rendered as its own
> ClusterRole and ClusterRoleBinding in both watch modes, including the
> namespace allowlist. Creating a TokenReview only asks the API server whether a
> bearer token is valid; it grants no access to any cluster object, and the
> server cannot authenticate an agent Job's per-Page token without it.

Forbidden, and asserted by tests: any `update`, `patch`, `create`, or `delete`
on a workload, Event, or Node; `pods/exec`, `pods/attach`, `pods/portforward`,
`pods/ephemeralcontainers`; `serviceaccounts/token`; `impersonate`, `escalate`,
`bind`; `nodes/proxy`; any `*/scale`; and any Secret access outside the two
named Secrets.

The grant is proved three ways: a Go chart test compares every rendered rule
against a checked-in allowlist and fails when a resource is added; an envtest
suite asks the API server's authorizer with SubjectAccessReviews; and CI installs
the chart on a fresh kind cluster and runs a `kubectl auth can-i` matrix as the
server's ServiceAccount.

## Consequences

- Proposal validation is in-process determinism plus the Target Repository's own
  Checks. The Page records that no admission evaluation was simulated.
- A misconfigured connector cannot widen the server's access by being given a
  credential; it can only query what it was built to query.
- `configmaps` are readable cluster-wide, and a ConfigMap routinely carries a
  copied credential. Its contents are treated as untrusted and redacted by key
  pattern and value shape before they reach the agent or the Portal.
- `nodes` cannot be namespaced. In allowlist mode the read Role cannot include
  it, so node-pressure evidence is unavailable and the Portal says so.
- The agent Job has no Kubernetes RBAC, so the server, not the agent, performs
  every read on the Page's behalf.

## Alternatives considered

- **Keep `update` for server-side dry runs.** It is an authorized write. A
  product that cannot write has nothing to dry-run, and the cluster's own CI is
  the authority on whether a manifest is valid.
- **Grant Secrets so the agent can explain a mount failure.** Every Secret read
  is a credential-exfiltration path; the missing mount is explainable from the
  Pod spec and the Event.
