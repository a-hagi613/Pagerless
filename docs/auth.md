# Authentication

Pagerless has two front doors with two credential stories. `kubectl page` uses
the operator's kubeconfig, so the Kubernetes API authenticates it and RBAC
authorises it. The Portal is a browser application, so `pagerless-server` signs
the operator in and never gives the browser a cluster credential.

Authentication has no external prerequisite: no identity provider, no database,
and no gateway. The decisions are [ADR-0032](adr/0032-single-operator-local-authentication.md)
(sessions) and [ADR-0033](adr/0033-one-credentials-secret.md) (credentials).

## Modes

`auth.mode` accepts exactly two values.

| Mode | Who authenticates | When to use it |
| --- | --- | --- |
| `token` (default) | `pagerless-server` verifies one bootstrap token and issues a signed session cookie. | Any installation, including a laptop port-forward. |
| `disabled` | Nothing: every request is `anonymous`, or attributed by an authenticating proxy's identity header. | Only behind a proxy that authenticates the operator, such as OAuth2-Proxy. |

`disabled` is not a convenience switch. The chart refuses to render it with
`ingress.enabled` and an empty `auth.proxy.identityHeader`, because that
combination would publish an anonymous approval surface. It also prints a
warning block in the release notes, and the server logs a warning at startup.

## Token mode

The chart generates two Secrets in the release namespace:

| Secret | Contents | Kept across `helm uninstall` |
| --- | --- | --- |
| `pagerless-admin-secret` | `token`: 48 alphanumerics. | Yes, unless `auth.adminSecret.keep=false`. |
| `pagerless-session-key` | `key`: at least 32 bytes of HMAC-SHA256 key material. | Yes. |

Both are reused on upgrade through `lookup`, so an upgrade does not rotate the
token or sign every browser out. In GitOps, set `auth.adminSecret.create=false`
and supply the token Secret out of band; a sync then never rotates it.

The operator signs in once:

1. Port-forward the Portal:
   `kubectl -n pagerless port-forward svc/pagerless-server 8080:8080`.
2. Print the token:
   `kubectl -n pagerless get secret pagerless-admin-secret -o go-template='{{.data.token | base64decode}}{{"\n"}}'`.
3. Open `http://localhost:8080/login` and paste it.
4. Delete the Secret once you no longer need it:
   `kubectl -n pagerless delete secret pagerless-admin-secret`.

Deleting the token Secret stops new token sign-ins and does not invalidate an
existing browser session, because the session is verified with
`pagerless-session-key`. Rotating that key instead signs every session out.

The token and the key are read from their mounted Secrets, so the server holds
no `get` grant on either. The only Secrets RBAC is `get`, `update`, and `patch`
on the credentials Secret, by `resourceNames`.

## Sessions

`POST /api/auth/login` compares the submitted token in constant time, after a
fixed delay, and limits failures per source address: five attempts in five
minutes lock that address out for five minutes with `429`, `Retry-After`, and a
warning log. Timing therefore reveals neither the token nor how close a guess
was.

A successful sign-in sets one cookie:

- The cookie is `HttpOnly`, `SameSite=Lax`, path-scoped to `/`, and `Secure`
  whenever `dashboard.baseUrl` is `https://` (or `auth.session.secure` says so
  explicitly). Its lifetime is `auth.session.ttl`, eight hours by default.
- Its value is `base64url(payload) "." base64url(HMAC-SHA256(key, payload))`.
  The payload carries the cookie version, the subject, a session identifier
  (`jti`), the issue and expiry times, and a CSRF token. The server verifies the
  signature before it parses the payload.
- Sessions are stateless across pod restarts because the signing key is a
  Secret. There is no server-side session table.
- `POST /api/auth/logout` records the `jti` in the SQLite `revoked_sessions`
  table until the cookie expires, so a copied cookie stops working after
  sign-out.

`GET /api/auth/session` returns the signed-in subject, whether the caller may
decide, and the CSRF token. It never returns the cookie or the key.

## CSRF

Every state-changing `/api/*` request authenticated by the cookie must repeat
the session's CSRF token in `X-CSRF-Token`. It is compared in constant time, and
a missing or wrong token is `403` and changes nothing. `SameSite=Lax` already
stops a cross-site form from sending the cookie; the token stops a script that
can make same-site requests from acting without reading the session.

A Bearer bootstrap token is accepted on `/api/*` only when `auth.bearerToken` is
enabled. That request skips the CSRF check for itself alone, which is what makes
a scripted runbook possible. A Bearer header is never combined with a cookie.

## Disabled mode

With `auth.mode: disabled`, every request resolves to the synthetic user
`anonymous`, or to the value of `auth.proxy.identityHeader` when the operator
sets it (for example `X-Auth-Request-User`). The header attributes the action in
the audit trail; it never authorises one. Anyone who can reach the Portal can
record a decision, so the Ingress render guard exists.

## Who may decide

In `token` mode the single operator account may decide, and the session subject
is written to the Page as `decidedBy`. In `disabled` mode the proxy identity, or
`anonymous`, is written. In kubeconfig mode the CLI declares the kubeconfig
user. Approval authority is `patch` on `pages`; `Role pagerless-approver` grants
exactly that ([ADR-0031](adr/0031-approval-is-a-decision-on-the-page.md)).
