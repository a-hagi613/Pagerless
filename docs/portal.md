# The Portal

`pagerless-server` embeds a React single-page application and serves it on its
dashboard listener. The Portal is a view over the same `Page` resources that
`kubectl page` reads, with the same decision path, so the two surfaces cannot
disagree about what a Page is or who decided it.

The chart starts the Portal on the Service's port. In `token` mode, the default,
it also creates the two authentication Secrets and mounts them, so no identity
provider or database is required ([ADR-0032](adr/0032-single-operator-local-authentication.md)).

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
| `/` | The cluster view: a dismissible quickstart, the health matrix, and the severity-ordered inbox. A card expands in place into the root cause, Findings, the live trace, the stored Proposal diff, the pull request, and the decision controls. The list refreshes on a timer. |
| `/settings` | The AI endpoint, the Target Repository, and the Connector credentials, with masked fingerprints, a test action, and `managedBy: external` read-only rendering. An installation that is still onboarding is sent here. |
| `/login` | The bootstrap-token exchange the Portal renders. `auth.mode: disabled` never reaches it. |

The namespace and node tiles are the filter control: a tile narrows the inbox to
its scope, the inbox states that scope in words, and the 40 px brand lockup
clears every filter. There is no separate cluster endpoint: a tile's status and
counts are the Pages whose Target is in that scope.

The Variant C Portal replaces the earlier Pages table and the separate Page
detail and Start Page routes; the inline card carries the same record and opens
a Page for any supported Target kind.

## API

| Method and path | Meaning |
| --- | --- |
| `GET /api/auth/session` | The signed-in subject, whether the caller may decide, the authentication mode, and the CSRF token. `/api/session` remains an alias for the embedded Portal. |
| `POST /api/auth/login` | Exchanges `{token}` for a signed session cookie. |
| `POST /api/auth/logout` | Revokes the session and clears the cookie. `/logout` remains an alias. |
| `GET /api/pages` | Page summaries, filtered by `phase`, `target`, `trigger`, and `since`; each row carries the Page's `severity`, its `proposalHash`, and its `pullRequest` so a card needs no second read. |
| `GET /api/pages/{name}` | One Page record, from the live resource or from history. |
| `GET /api/pages/{name}/events` | Server-Sent Events: steps, Phase changes, and a final snapshot, with a comment every 15 seconds so an idle stream stays open. `/trace` is an alias. |
| `POST /api/pages` | Starts a Page for `{target, hint}` or `{kind, namespace, name, hint}`. |
| `POST /api/pages/{name}/decision` | Records `{decision, proposalHash, reason}`. |
| `GET /api/targets` | The Deployments and StatefulSets that Start Page offers. |
| `GET /api/settings` | The masked credentials: `{configured, fingerprint, updatedAt}` per key, the owner, and whether onboarding is required. |
| `PATCH /api/settings` | Merges `{values}` or the grouped `{ai, repository, connectors}` form into the credentials Secret. |
| `POST /api/settings/test` | Probes `{target}` (`ai`, `github`, or `connector:<name>`) with the stored credentials and returns `{ok, detail}`. |
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
never creates the Secret and never returns a stored credential. Credential
fields are masked; the endpoint URL, model name, repository name, and API URL are
not secrets and are returned so the form can prefill.

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
