# ADR-0032: A single operator authenticates locally with a bootstrap token and signed session cookies

- **Status:** Accepted
- **Date:** 2026-11-18
- **Supersedes:** [ADR-0007](0007-keycloak-scope.md) and the identity-provider prerequisite of [ADR-0020](0020-dashboard-approvals-assert-the-approver.md).

## Context

In v1, `pagerless-server` signed dashboard operators in through a bundled Keycloak realm and verified approver group membership against the Keycloak admin API. The agent also exchanged its service account token for a Keycloak token to call `agentgateway`. That design required PostgreSQL and Keycloak to be running before Pagerless could start, preventing a standalone `helm install` on an existing cluster.

With `agentgateway` and the MCP servers replaced by built-in read-only Connectors and an in-process tool broker ([ADR-0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md)), and with approvals recorded as decision annotations on the `Page` ([ADR-0031](0031-approval-is-a-decision-on-the-page.md)), the Portal is the only remaining consumer of user authentication. Pagerless is a single-operator product: one installation watches one Cluster and has one operator account.

## Decision

Authentication requires no external identity provider and no external database.

- `auth.mode` accepts two values: `token` (the default) and `disabled`.
- In `token` mode, the Helm chart generates two Secrets in the release namespace:
  - `pagerless-admin-secret`, containing a 48-character alphanumeric bootstrap token (`helm.sh/resource-policy: keep`, preserved across upgrades via `lookup` or supplied out of band in GitOps via `auth.adminSecret.create: false`).
  - `pagerless-session-key`, containing a 32-byte signing key (`helm.sh/resource-policy: keep`).- `POST /api/auth/login` compares the submitted token against the mounted
  `pagerless-admin-secret` in constant time with a fixed delay and per-source-address
  rate limiting (`429` with `Retry-After` and a warning log on lockout). On
  success, it issues an HTTP-only, `SameSite=Lax`, path-scoped cookie
  (`pagerless_session`) formatted as
  `base64url(payload) "." base64url(HMAC-SHA256(key, base64url(payload)))`.
- The signed payload carries `{v, sub, jti, iat, exp, csrf}`. The server verifies the HMAC signature before parsing the payload.
- Sessions are stateless across pod restarts because the signing key lives in `pagerless-session-key`. Rotating `pagerless-session-key` invalidates all active cookies immediately; deleting `pagerless-admin-secret` after first login disables new token sign-ins without invalidating active cookies.
- `POST /api/auth/logout` revokes the cookie's `jti` in the SQLite `revoked_sessions` table until the cookie's expiry time, so a copied cookie cannot be reused after sign-out.
- Every state-changing `/api/*` request authenticated by cookie must repeat the session's `csrf` claim in `X-CSRF-Token`, verified in constant time (`403` on missing or mismatched tokens). When `auth.bearerToken` is enabled, `Authorization: Bearer <admin-token>` is accepted for scripted API calls and skips the CSRF check for that request alone.
- In `disabled` mode, every request is authenticated as `anonymous` (or the value of `auth.proxy.identityHeader` when an authenticating reverse proxy fronts the server). The chart refuses to render when `ingress.enabled` is true with `auth.mode: disabled` and an empty `auth.proxy.identityHeader`.
- The bootstrap token and the session signing key are read from mounted Secrets, so the server holds no API grant on them. The credentials Secret is the only Secret the server writes; its contract is [ADR-0033](0033-one-credentials-secret.md).

## Consequences

- `helm install pagerless` succeeds on a bare cluster with zero prerequisite services.
- `internal/keycloak` and the OpenID Connect client flow in `internal/dashboardauth` are removed.
- Enterprise single sign-on is supported by placing an authenticating proxy (such as OAuth2-Proxy) in front of `auth.mode: disabled` and setting `auth.proxy.identityHeader`, keeping federation code out of `pagerless-server`.
