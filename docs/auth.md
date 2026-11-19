# Authentication

Pagerless has two front doors with two different credential stories. The CLI uses
the operator's kubeconfig, so the Kubernetes API authenticates it and RBAC
authorises it. The Portal is a browser application, so `pagerless-server` runs a
sign-in flow for it and never gives the browser a cluster credential.

The chart does not enable the Portal. It starts neither the dashboard listener
nor an identity provider; the sign-in and Portal slices own the values that do.
This page documents the sign-in implementation and the modes the v2.0 release
ships and plans, so an operator can review the boundary before exposing it.

## Modes

| Mode | State | Who authenticates |
| --- | --- | --- |
| OpenID Connect | Implemented | An OIDC provider that issues an ID token for the dashboard client. |
| Bootstrap token | Delivered by the onboarding slice | `pagerless-server` verifies a single operator token and creates the session itself. |
| Disabled, behind a proxy | Delivered by the onboarding slice | A trusted reverse proxy authenticates the operator and asserts the identity in a header. |

### OpenID Connect

The server performs the authorization-code flow with PKCE against the configured
issuer, verifies the ID token's signature, issuer, audience, expiry, and nonce,
and reads the user's name and groups from it. The flags are:

| Flag | Meaning |
| --- | --- |
| `--dashboard-bind-address` | Enables the Portal and its API on this address. |
| `--oidc-issuer` | The realm issuer, such as `https://sso.example.com/realms/pagerless`. |
| `--oidc-client-id` | The confidential dashboard client. Default: `pagerless-dashboard`. |
| `--oidc-client-secret-file` | The file that holds the client secret. |
| `--dashboard-base-url` | The browser-facing base URL; the callback is `<base>/auth/callback`. |
| `--approvers-group` | The group whose members may decide a Proposal. Default: `pagerless-approvers`. |
| `--dashboard-cookie-secure` | Marks the cookies `Secure`. Default: `true`. |

The client secret is read on every token request, so rotating the file takes
effect without a restart. The realm is discovered on the first sign-in, so the
server starts before the provider is reachable.

### Disabled, behind a proxy

With authentication disabled, the Portal trusts the identity header of a reverse
proxy it is deployed behind, and refuses to start when no header is configured.
The combination is dangerous, so the chart is expected to fail the render when it
is exposed by an Ingress without a proxy. Until the onboarding slice lands, do not
run the Portal without an OIDC issuer.

## Sessions

A successful sign-in creates a server-side session and sets one opaque cookie:

- The cookie is 32 random bytes, `HttpOnly`, `SameSite=Lax`, and `Secure` unless
  `--dashboard-cookie-secure=false` for a local port-forward. It carries no
  identity: the server maps it to the session it created.
- The session holds the ID token's subject, username, groups, a CSRF token, and
  an expiry. The default lifetime is eight hours, after which the user signs in
  again.
- Sessions live in the server's memory, so restarting the pod signs every user
  out. That is deliberate: a session must not survive a crash that loses the
  identity it was built from.
- Sign-in attempts use a second cookie scoped to `/auth` for ten minutes. It
  holds the state, nonce, and PKCE verifier. It needs no signature: a tampered
  cookie cannot produce a valid ID token for the configured client.
- `POST /logout` requires the CSRF token, deletes the session, and redirects to
  `/signed-out`.

## CSRF

Every state-changing request repeats the session's CSRF token in `X-CSRF-Token`.
The server compares it in constant time. `SameSite=Lax` already stops a cross-site
form from sending the session cookie; the token additionally stops a script that
can make same-site requests from acting without reading the session. A request
without the token, or with the wrong one, is answered with `403` and changes
nothing.

## Who may decide

The Portal records a decision only for a member of the approvers group. The
username from the verified ID token is written to the Page as `decidedBy` next to
the Proposal hash. That record is an audit trail, not an authorisation check:
`pagerless-server` reconciles the decision annotations, and the Page's RBAC is
what stops an unauthorised user from writing them in the first place. See
[ADR-0031](adr/0031-approval-is-a-decision-on-the-page.md).

## Threat model

| Threat | Control |
| --- | --- |
| A stolen browser cookie | The cookie is opaque and `HttpOnly`, expires with the session, and grants only what the signed-in user may do. A stolen cookie is a session theft, not a cluster credential: it cannot read the kubeconfig or the credentials Secret. |
| Cross-site request forgery | `SameSite=Lax` cookies plus a per-session CSRF token on every write. |
| Cross-site scripting | Tokens never reach the browser; the session store is server-side. The Portal renders Page content through React, never as HTML. |
| A forged token | The ID token signature, issuer, audience, expiry, and nonce are verified before a session exists. |
| A tampered sign-in cookie | The flow cookie's state and nonce must match the callback, and the PKCE verifier must complete the exchange. |
| Replay of a callback | The state and nonce are single-use and the flow cookie expires after ten minutes. |
| Exposed over plain HTTP | `--dashboard-cookie-secure=true` marks the cookies `Secure`; the chart does not terminate TLS, so a proxy in front of it must. |
| A compromised Portal pod | The Portal inherits the server's Role: read evidence, Pages in the release namespace, and no write to any workload. A compromised pod can decide a Proposal, not change a cluster. |
| An approval by the wrong person | The approvers group is checked at decision time, and the decision is bound to the Proposal hash it was shown. |

The cluster identity is the real boundary, and it is documented in
[ADR-0037](adr/0037-read-only-cluster-identity.md). Authentication decides who may
read the Portal and who may decide a Proposal; it never widens what the process
can do to the cluster.

## CLI authentication

`kubectl page` sends no request to `pagerless-server`. It reads and patches Pages
through the Kubernetes API with the kubeconfig's identity, so the Kubernetes
authenticator and RBAC decide everything. `Role pagerless-approver` grants
`get`, `list`, `watch`, and `patch` on `pages` and nothing else.
