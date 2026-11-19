# Upgrading from v1

v2.0 is a replacement, not an upgrade. The resource changed name, the plugin
changed name, the chart lost most of its values, and there is no conversion
between v1 objects and v2 objects. This page lists the whole break so that an
operator can plan it rather than discover it.

Do not run `helm upgrade` from a v1 release to this chart. Uninstall v1, delete
the v1 CRDs, and install v2 as a new release. The sections below explain what
each step removes.

## Renamed resource and plugin

| v1 | v2 |
| --- | --- |
| `Triage` (`triages.pagerless.dev/v1alpha1`) | `Page` (`pages.pagerless.dev/v1alpha1`, shortname `pg`) |
| `TriageApproval` (`triageapprovals.pagerless.dev/v1alpha1`) | No resource: an Approval is a decision recorded on the Page (ADR-0031) |
| `kubectl triage` (`kubectl-triage`) | `kubectl page` (`kubectl-page`) |
| `Triage` phase `Executing` (the Executor applied the change) | `Page` phase `PullRequestOpen`: the change is a pull request, and `Verifying` follows a merge |

There is no alias: `kubectl triage` is not installed with v2, and the API server
does not serve `triages.pagerless.dev` once the v1 CRDs are gone.

## CRD replacement

v2 installs `pages.pagerless.dev` from `charts/pagerless/crds/pagerless.dev_pages.yaml`
and serves nothing under the v1 group. Helm never deletes a CRD, so removing the
v1 API is an explicit step:

```bash
kubectl delete crd triages.pagerless.dev triageapprovals.pagerless.dev
```

Deleting a CRD deletes every custom resource of that kind. v1 Triages and
TriageApprovals are therefore gone, and no v2 Page is created in their place.
Export anything you need to keep before you run either command.

## No migration

Nothing is carried across:

- **v1 objects are not converted.** A v2 controller never reads a Triage, and no
  job rewrites one into a Page.
- **Decisions are not carried across.** A v1 approval bound to a Triage hash has
  no meaning for a Page whose Proposal was rendered by a different engine.
- **The history database starts empty.** The v2 append-only history is written by
  `pagerless-server` on the new claim; the v1 database is not imported.
- **The credentials move.** v1 spread its credentials over six Secrets that the
  chart refused to create; v2 reads one `pagerless-credentials` Secret with six
  documented keys. Re-enter the values; do not copy the old Secret names.

## Removed values

Every value below existed in the v1 chart and does not exist in v2. Helm rejects
an unknown value only when a schema is shipped, so remove them from your values
file before you install; passing them is silently ignored, which is worse.

### Components that v2 does not install

| Removed value | Why |
| --- | --- |
| `images.executor.repository`, `images.executor.tag`, `images.executor.pullPolicy` | The Executor is deleted; the pull request engine runs in `pagerless-server` (ADR-0030). |
| `agent.provider`, `agent.awsRegion` | The model provider is an OpenAI-compatible endpoint in the credentials Secret, not a chart value. |
| `agent.replayFile`, `agent.narrativeReplayFile` | Replay recordings are an evaluation fixture, not a chart value. |
| `mcp.gatewayUrl`, `mcp.servers`, `mcp.bundled.enabled`, `mcp.githubHost`, `mcp.backendTokensSecretName`, `mcp.clientTokensSecretName` | No MCP server and no gateway is installed; the agent reads through the server's tool broker. |
| `oidc.issuer`, `oidc.tokenUrl`, `oidc.keycloakBaseUrl`, `oidc.dashboardClientId`, `oidc.serverClientId`, `oidc.agentClientId`, `oidc.secretName` | Keycloak is not installed, and the agent no longer receives a cluster identity. |
| `dashboard.cookieSecure`, `dashboard.approversGroup` | Portal sign-in is enabled by the chart. Use `auth.session.secure`; the portal has one operator account, so there is no approvers group. `dashboard.baseUrl` remains. |
| `argocd.application`, `argocd.url`, `argocd.secretName` | Argo CD is a verification Connector, configured out of band, not a chart prerequisite. |
| `alertmanager.url`, `alertmanager.secretName` | Alertmanager is a Connector, configured out of band (ADR-0037). |
| `grafana.browserUrl`, `grafana.mcpUrl`, `grafana.workloadDashboardUid`, `grafana.logsDashboardUid` | The Portal is the product's surface; no Grafana instance is embedded. |
| `sandbox.url`, `sandbox.branch`, `sandbox.owner`, `sandbox.repo`, `sandbox.githubRepository`, `sandbox.readSecretName`, `sandbox.writeSecretName` | The Target Repository is provenance or `GITHUB_REPO` in the credentials Secret; the separate read and write tokens are gone because Pagerless only opens pull requests. |

### Values whose meaning changed

| Removed value | Replacement |
| --- | --- |
| `workloadNamespaces` | `rbac.watchNamespaces` with `rbac.watchAllNamespaces=false`. |
| `createWorkloadNamespaces` | Create the namespaces yourself; Pagerless never writes outside its release namespace. |
| `persistence.enabled` | The history claim is always rendered; `persistence.size` and `persistence.accessModes` size it. |
| `secrets.createPlaceholders` | The chart always creates `pagerless-credentials` with every key present and empty, unless `credentials.existingSecret` names another Secret. |

## Access changes

- v1 held `update`, `patch`, `create`, and `delete` on workloads so it could
  remediate in place, plus `pods/exec`-class permissions for its MCP servers. v2
  holds none of them: the read grant is compared against an allowlist on every
  pull request (ADR-0037).
- v1's admission webhooks and their serving certificate are gone. v2 serves no
  webhook, so installing it needs less privilege than v1.
- v1 bound a separate executor and an agent identity. v2 runs one binary and an
  agent Job with no Kubernetes RBAC (ADR-0030).

## Procedure

1. Export what you need from v1. There is no import.
2. `helm uninstall <v1 release> --namespace <v1 namespace>`
3. `kubectl delete crd triages.pagerless.dev triageapprovals.pagerless.dev`
4. Install v2 into a fresh namespace, following
   [`docs/install.md`](install.md).
5. Sign in with the bootstrap token from the release notes, then finish onboarding at `/settings`; [`docs/onboarding.md`](onboarding.md) covers the alternatives.
6. Replace `kubectl triage <verb>` with `kubectl page <verb>` in scripts and
   runbooks; [`docs/cli.md`](cli.md) documents the verbs and exit codes.

Step 3 cannot be undone. A v1 release can be reinstalled, but its Triages cannot
be recovered after the CRD is deleted.
