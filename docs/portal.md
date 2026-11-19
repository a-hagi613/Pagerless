# The Portal

`pagerless-server` embeds a React single-page application and serves it on its
dashboard listener. The Portal is a view over the same `Page` resources that
`kubectl page` reads, with the same decision path, so the two surfaces cannot
disagree about what a Page is or who decided it.

The chart starts the Portal on the Service's port. It also creates the two
authentication Secrets and mounts them, so no identity provider or database is
required ([ADR-0032](adr/0032-single-operator-local-authentication.md)).

## Enabling it

The Portal is served whenever `--dashboard-bind-address` is set. The chart does
that, and passes the authentication settings explicitly:

| Flag | Chart value | Meaning |
| --- | --- | --- |
| `--dashboard-bind-address=:8080` | `service.port` | The Portal and its API. |
| `--dashboard-base-url` | `dashboard.baseUrl` | Browser-facing URL; scopes the session cookie. |
| `--auth-mode` | `auth.mode` | `token` or `disabled`. |
| `--auth-admin-token-file` | mounted token | The bootstrap token Secret. |
| `--auth-session-key-file` | mounted key | The session signing key Secret. |
| `--auth-bearer-token` | `auth.bearerToken` | Accepts a Bearer token on `/api/*`. |
| `--auth-identity-header` | `auth.proxy.identityHeader` | Attributes decisions in `disabled` mode. |
| `--dashboard-cookie-secure` | `auth.session.secure` | `""` derives it from the base URL. |

`docs/auth.md` documents the sign-in flow, the cookie, CSRF, and the modes.

## Routes

| Route | What it does |
| --- | --- |
| `/` | Lists Pages, most recent first, and refreshes them. It starts from the latest history snapshot of every Page and overlays the live resource, so a terminal Page whose resource the retention controller deleted still appears. |
| `/pages/{name}` | Shows the live steps, Findings with their sources, the Diagnosis, the Proposal with its rendered change, the pull request, the Verification checks, and the Narrative. While the Page is `Proposed` it shows the Approve and Reject controls. |
| `/start` | Starts a Page for a chosen Deployment or StatefulSet, with an optional Hint. A Page whose Fingerprint already has an Active Page returns that Page instead of starting a second one. |
| `/login` | The sign-in form. In `disabled` mode it redirects to `/`. |

The Variant C Portal adds `/settings`, the onboarding destination that reads and
writes the credentials Secret. The remaining routes are documented above.

## API

| Method and path | Meaning |
| --- | --- |
| `GET /api/auth/session` | The signed-in subject, whether the caller may decide, the authentication mode, and the CSRF token. `/api/session` remains an alias for the embedded Portal. |
| `POST /api/auth/login` | Exchanges `{token}` for a signed session cookie. |
| `POST /api/auth/logout` | Revokes the session and clears the cookie. `/logout` remains an alias. |
| `GET /api/pages` | Page summaries, filtered by `phase`, `target`, `trigger`, and `since`. |
| `GET /api/pages/{name}` | One Page record, from the live resource or from history. |
| `GET /api/pages/{name}/events` | Server-Sent Events: steps, Phase changes, and a final snapshot, with a comment every 15 seconds so an idle stream stays open. `/trace` is an alias. |
| `POST /api/pages` | Starts a Page for `{target, hint}` or `{kind, namespace, name, hint}`. |
| `POST /api/pages/{name}/decision` | Records `{decision, proposalHash, reason}`. |
| `GET /api/targets` | The Deployments and StatefulSets that Start Page offers. |
| `GET /api/settings` | The masked credentials: `{configured, fingerprint, updatedAt}` per key, the owner, and whether onboarding is required. |
| `PATCH /api/settings` | Merges `{values}` or the grouped `{ai, repository, connectors}` form into the credentials Secret. |
| `DELETE /api/settings/{key}` | Clears one credential key. |
| `GET /api/version` | The server version and authentication mode. |
| `GET /healthz`, `GET /readyz` | On the health port. `/readyz` stays `200` and lists the subsystems that still need onboarding. |

Every write repeats the session's CSRF token in `X-CSRF-Token` and is answered
with `403` when it is missing or wrong. A decision on a Page that is not
`Proposed` is `409`; a decision whose supplied `proposalHash` no longer matches
the Proposal is `409` with the code `proposal_changed`.

## Settings

A settings write reads the credentials Secret, merges only the submitted keys,
updates with the observed resource version, and retries once on a conflict. It
never creates the Secret and never returns a stored credential. The response is
masked, so a value cannot be recovered from the Portal.

When `credentials.existingSecret` is set, every write is answered with `409`,
the code `settings_externally_managed`, and the manager's name, and the Portal
renders the fields read-only. The CLI refuses with the same message and exit
code `3` ([ADR-0033](adr/0033-one-credentials-secret.md)).

## Decisions

A Portal decision patches the Page's decision annotations with the signed-in
username, the exact Proposal hash, and the optional reason:

```yaml
pagerless.dev/decision: approve
pagerless.dev/decision-proposal: <proposal hash>
pagerless.dev/decided-by: <session subject>
pagerless.dev/decision-reason: <optional reason>
```

The Page controller reconciles those annotations into `status.decision`, which
keeps the Page status written by `pagerless-server` alone. `spec` is immutable
and `status` is a subresource, so a decision cannot change the investigation
([ADR-0031](adr/0031-approval-is-a-decision-on-the-page.md)).
