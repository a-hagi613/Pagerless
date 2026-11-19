# charts/pagerless

A standalone chart that installs one `pagerless-server` Deployment into one
namespace. It has no external prerequisite and no admission webhook, and its
cluster identity is strictly read-only.

```bash
helm install pagerless oci://ghcr.io/a-hagi613/charts/pagerless \
  --namespace pagerless --create-namespace
```

## Rendered objects

| Kind | Name | Notes |
| --- | --- | --- |
| `CustomResourceDefinition` | `pages.pagerless.dev` | Installed from `crds/`; Helm never upgrades it. |
| `Deployment` | `pagerless-server` | One replica, `Recreate`, because it is the single writer of Page status. |
| `Service` | `pagerless-server` | ClusterIP on the chart's `service.port`. |
| `PersistentVolumeClaim` | `pagerless-data` | The append-only Page history. |
| `ConfigMap` | `pagerless-config` | Non-secret server configuration. |
| `ConfigMap` | `pagerless-connectors` | The connector catalogue that `connectors.seed` declares. |
| `Secret` | `pagerless-credentials` | Every documented key present and empty; omitted when `credentials.existingSecret` is set. |
| `ServiceAccount` | `pagerless-server` | Holds the read identity. |
| `ServiceAccount` | `pagerless-agent` | Bound to no Role or ClusterRole; never automounts a token. |
| `ClusterRole`, `ClusterRoleBinding` | `<release>-pagerless-read` | Cluster-wide mode only. |
| `Role`, `RoleBinding` | `<release>-pagerless-read` | One per entry in `rbac.watchNamespaces`, in allowlist mode. |
| `Role`, `RoleBinding` | `pagerless-server` | Pages, agent Jobs, and the two named Secrets in the release namespace. |
| `Role` | `pagerless-approver` | `get`/`list`/`watch` and `patch` on `pages`. |
| `RoleBinding` | `pagerless-approvers` | Rendered only when `approvers.subjects` is set. |

Nothing is rendered outside the release namespace except the CRD and the
cluster-scoped read role and binding. There is no `MutatingWebhookConfiguration`,
no `ValidatingWebhookConfiguration`, no `ValidatingAdmissionPolicy`, no serving
certificate, and no webhook port.

## Read-only grant

The read role and every allowlisted read Role grant `get`/`list`/`watch` on
pods, services, endpoints, ConfigMaps, PVCs, both Event APIs, `apps` workloads,
`batch` jobs and cron jobs, endpoint slices, and ingresses, plus `get` on
`pods/log`. The cluster-wide role adds nodes, namespaces, `/openapi/v3`, and
`create` on `authentication.k8s.io/tokenreviews`, which authenticates the agent
Job.

No rule grants `update`, `patch`, `create`, or `delete` on a workload, Event, or
Node, or any of `pods/exec`, `pods/attach`, `pods/portforward`,
`pods/ephemeralcontainers`, `serviceaccounts/token`, `*/scale`, `impersonate`,
`escalate`, or `bind`. Secret access is limited to `get` on
`pagerless-credentials` and `pagerless-admin-secret`, and `update`/`patch` on
`pagerless-credentials`, all by `resourceNames`.

A Go test compares the rendered rules with a checked-in allowlist and fails when
a rule widens. See [`docs/adr/0037`](../../docs/adr/0037-read-only-cluster-identity.md).

## Values

| Value | Default | Meaning |
| --- | --- | --- |
| `images.server.repository` | `ghcr.io/a-hagi613/pagerless-server` | Server image. |
| `images.server.tag` | `""` | Empty uses `Chart.appVersion`. |
| `images.server.pullPolicy` | `IfNotPresent` | |
| `images.agent.repository` | `ghcr.io/a-hagi613/pagerless-agent` | Agent Job image. |
| `images.agent.tag` | `""` | Empty uses `Chart.appVersion`. |
| `images.pullSecrets` | `[]` | Names of imagePullSecrets in the release namespace. |
| `serviceAccount.create` | `true` | Render the server ServiceAccount. |
| `serviceAccount.name` | `pagerless-server` | Fixed; the RBAC and documented checks refer to it. |
| `serviceAccount.annotations` | `{}` | IRSA, Pod Identity, or workload identity bindings. |
| `serviceAccount.automountServiceAccountToken` | `true` | |
| `rbac.create` | `true` | Render the read and release RBAC. |
| `rbac.watchAllNamespaces` | `true` | Cluster-wide read role. Mutually exclusive with `watchNamespaces`. |
| `rbac.watchNamespaces` | `[]` | Allowlist; one read Role per entry. Needs `watchAllNamespaces: false`. |
| `approvers.subjects` | `[]` | Optional binding of `pagerless-approver`; empty renders no binding. |
| `credentials.existingSecret` | `""` | An externally managed Secret; the chart renders none. |
| `credentials.secretName` | `pagerless-credentials` | The Secret the chart creates and the Deployment mounts. |
| `credentials.mountPath` | `/var/run/pagerless/credentials` | Read-only mount path. |
| `connectors.seed` | `[]` | Connector entries persisted into `pagerless-connectors`. |
| `persistence.size` | `1Gi` | History claim size. |
| `persistence.storageClassName` | `""` | Empty uses the cluster default. |
| `persistence.accessModes` | `[ReadWriteOnce]` | |
| `service.type` | `ClusterIP` | |
| `service.port` | `8080` | The report API port. |
| `dashboard.baseUrl` | `http://localhost:8080` | Browser-facing URL used by the Portal once it is served. |
| `resources` | requests `50m`/`64Mi`, limit `256Mi` | |
| `podSecurityContext`, `securityContext` | non-root, read-only root, all capabilities dropped | |
| `replicaCount` | `1` | The server is the single writer of Page status. |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `podLabels`, `commonLabels`, `commonAnnotations` | empty | Standard scheduling and labelling. |

## Notes

- The v2.0 chart shell serves the agent report API and the metrics API. The
  Portal and its sign-in arrive with the authentication and Portal slices, which
  add their values and their Secret.
- `helm template` fails with a clear message on an impossible watch
  configuration, so a partially applied mode cannot be installed.
