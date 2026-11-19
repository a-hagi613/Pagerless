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
| `Service` | `pagerless-server` | `ClusterIP` by default; `service.type` changes it. |
| `PersistentVolumeClaim` | `pagerless-data` | The append-only Page history. |
| `ConfigMap` | `pagerless-config` | Non-secret server and CLI configuration: the allowlist and the credentials Secret's name and manager. |
| `ConfigMap` | `pagerless-connectors` | The catalogue rendered from `connectors.*`. |
| `Secret` | `pagerless-admin-secret` | The bootstrap token, in `token` mode only; kept across `helm uninstall` unless `auth.adminSecret.keep` is false. |
| `Secret` | `pagerless-session-key` | The HMAC-SHA256 session signing key, in `token` mode only; kept across `helm uninstall`. |
| `Secret` | `pagerless-credentials` | Every fixed credential key and the four webhook tokens, present and empty; omitted when `credentials.existingSecret` is set. |
| `NetworkPolicy` | `pagerless-agent-egress` | The agent Job's two-destination egress policy; `agentEgress.enabled` controls it. |
| `Ingress` | `pagerless-server` | Rendered only when `ingress.enabled` is true. |
| `ServiceAccount` | `pagerless-server` | Holds the read identity. |
| `ServiceAccount` | `pagerless-agent` | Bound to no Role or ClusterRole; never automounts a token. |
| `ClusterRole`, `ClusterRoleBinding` | `<release>-pagerless-read` | Cluster-wide mode only. |
| `Role`, `RoleBinding` | `<release>-pagerless-read` | One per entry in `rbac.watchNamespaces`, in allowlist mode. |
| `Role`, `RoleBinding` | `pagerless-server` | Pages, the agent Jobs, and the credentials Secret in the release namespace. |
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
`escalate`, or `bind`. Secret access is limited to `get`, `update`, and `patch` on
`pagerless-credentials`, by `resourceNames`, and the rule is omitted when a
secret manager owns the Secret. The bootstrap token and session signing key are
read from their mounted volumes, so they need no API grant.

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
| `images.agent.pullPolicy` | `IfNotPresent` | |
| `images.pullSecrets` | `[]` | Names of imagePullSecrets in the release namespace. |
| `serviceAccount.create` | `true` | Render the server ServiceAccount. |
| `serviceAccount.name` | `pagerless-server` | Fixed; the RBAC and documented checks refer to it. |
| `serviceAccount.annotations` | `{}` | IRSA, Pod Identity, or workload identity bindings. |
| `serviceAccount.automountServiceAccountToken` | `true` | |
| `rbac.create` | `true` | Render the read and release RBAC. |
| `rbac.watchAllNamespaces` | `true` | Cluster-wide read role. Mutually exclusive with `watchNamespaces`. |
| `rbac.watchNamespaces` | `[]` | Allowlist; one read Role per entry. Needs `watchAllNamespaces: false`. |
| `approvers.subjects` | `[]` | Optional binding of `pagerless-approver`; empty renders no binding. |
| `auth.mode` | `token` | `token` or `disabled`; nothing else is accepted. |
| `auth.adminSecret.create` | `true` | Render the bootstrap token Secret; set false to supply it out of band. |
| `auth.adminSecret.name` | `pagerless-admin-secret` | The Secret that holds the bootstrap token. |
| `auth.adminSecret.key` | `token` | The key inside that Secret. |
| `auth.adminSecret.length` | `48` | Length of the generated token. |
| `auth.adminSecret.keep` | `true` | Keep the token Secret across `helm uninstall`. |
| `auth.session.secretName` | `pagerless-session-key` | The session signing key Secret. |
| `auth.session.key` | `key` | The key inside that Secret; rotating it signs every session out. |
| `auth.session.ttl` | `8h` | Lifetime of a signed session cookie. |
| `auth.session.secure` | `""` | Cookie Secure attribute; empty derives it from `dashboard.baseUrl`. |
| `auth.bearerToken` | `false` | Accept a Bearer bootstrap token on `/api/*` for scripted calls. |
| `auth.proxy.identityHeader` | `""` | Trusted header that names the operator in `disabled` mode. |
| `credentials.existingSecret` | `""` | An externally managed Secret; the chart renders no credentials Secret and the settings are read-only. |
| `credentials.secretName` | `pagerless-credentials` | The Secret the chart creates and the Deployment mounts. |
| `credentials.mountPath` | `/var/run/pagerless/credentials` | Read-only mount path. |
| `connectors.logs.nativeKubernetes.enabled` | `true` | Mandatory; the chart refuses to render it as false. |
| `connectors.logs.nativeKubernetes.tailLines`, `limitBytes`, `sinceSeconds`, `maxPods`, `maxContainersPerPod`, `concurrency` | `500`, `262144`, `3600`, `5`, `4`, `8` | Bounds of the native log collector. |
| `connectors.logs.<backend>.enabled` | `false` | `loki`, `grafana`, `cloudwatchLogs`, `datadogLogs`, `opensearch`, `mcp`. |
| `connectors.metrics.<backend>.enabled` | `false` | `prometheus`, `grafana`, `cloudwatch`, `datadog`, `mcp`. |
| `connectors.alertState.<backend>.enabled` | `false` | `alertmanager`, `grafana`. |
| `connectors.<backend>.*` | empty | Each disabled Connector takes its own `url`, query or template, bounds, and `auth` reference. |
| `connectors.subject.rules` | `[]` | Alert-to-Target extraction rules. |
| `connectors.redaction.patterns` | `[]` | Sanitization patterns applied before a bundle is stored. |
| `connectors.alerting.snsTopics` | `[]` | The only SNS topics the receiver accepts; empty refuses every delivery. |
| `connectors.alerting.verifySNSSignature` | `false` | Verify the SNS signature. |
| `connectors.alerting.maxAlerts`, `requestsPerSecond`, `burst` | `50`, `10`, `20` | Receiver bounds. |
| `connectors.allowedHosts` | `[]` | When set, the only hosts a Connector may reach. |
| `connectors.clusterName`, `connectors.environment` | `""` | Values substituted for `{{ .ClusterName }}` and `{{ .Environment }}`. |
| `persistence.size` | `1Gi` | History claim size. |
| `persistence.storageClassName` | `""` | Empty uses the cluster default. |
| `persistence.accessModes` | `[ReadWriteOnce]` | |
| `service.type` | `ClusterIP` | |
| `service.port` | `8080` | The Portal port. |
| `service.reportPort` | `8082` | The agent report API port, separate from the Portal. |
| `dashboard.baseUrl` | `http://localhost:8080` | Browser-facing Portal URL; scopes the session cookie. |
| `ingress.enabled` | `false` | Render an Ingress in front of the Portal. |
| `ingress.className` | `""` | Ingress class. |
| `ingress.host` | `""` | Optional host. |
| `ingress.annotations` | `{}` | Ingress annotations. |
| `ingress.tls` | `[]` | Ingress TLS entries. |
| `resources` | requests `50m`/`64Mi`, limit `256Mi` | |
| `agentEgress.enabled` | `true` | Render the agent Job's egress NetworkPolicy. |
| `agentEgress.modelPort` | `443` | The only remote port the agent Job may reach. |
| `podSecurityContext`, `securityContext` | non-root, read-only root, all capabilities dropped | |
| `replicaCount` | `1` | The server is the single writer of Page status. |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `podLabels`, `commonLabels`, `commonAnnotations` | empty | Standard scheduling and labelling. |

## Notes

- `NOTES.txt` prints the port-forward, the exact command that decodes the
  bootstrap token, the onboarding steps at `/settings`, and the command that
  deletes the bootstrap Secret once sign-in succeeds. In `disabled` mode it
  prints a warning block instead; with an external Secret it prints the required
  key names and the read-only guidance.
- `helm template` fails with a clear message on an impossible watch
  configuration and on `auth.mode: disabled` with an Ingress but no identity
  header, so neither can be installed by accident.
- An unaudited `helm install --set auth.adminPassword=…` fails schema
  validation: `values.schema.json` declares `additionalProperties: false` on
  every object and contains no property that carries secret material.
- The bootstrap token and the session signing key are generated once and reused
  on upgrade through `lookup`, so an upgrade does not rotate either.
