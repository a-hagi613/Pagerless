# Keycloak

The `keycloak` Argo CD Application renders this chart into the `keycloak`
namespace: Keycloak 26.8.2 in production mode, pinned by image digest, and its
own PostgreSQL 17.12 database. Keycloak needs a database because the
development in-memory store loses the realm on every restart.

The issuer is `http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless`.
Keycloak derives it from `KC_HOSTNAME`, and it must equal both the `iss` that
agentgateway requires and the audience of the agent's projected service account
token. Realm `sslRequired` is `none` because the local endpoint is plain HTTP
inside the cluster.

## Bootstrap

`hack/provision-keycloak.py` runs from `make up` after the Keycloak Application
is healthy. It is idempotent and provisions, through the Admin REST API:

- realm `pagerless`;
- group `pagerless-approvers`;
- identity provider `kubernetes` (`providerId: kubernetes`, issuer
  `https://kubernetes.default.svc.cluster.local`), which authenticates a
  projected service account token;
- confidential client `pagerless-agent` with
  `clientAuthenticatorType: federated-jwt`,
  `jwt.credential.issuer=kubernetes` and
  `jwt.credential.sub=system:serviceaccount:pagerless:pagerless-agent`. The
  client has no secret;
- confidential client `pagerless-dashboard` for #29, with the standard flow,
  PKCE `S256`, redirect URIs `http://localhost:8080/*` and
  `http://127.0.0.1:8080/*`, and a `groups` mapper on the ID token, access
  token and userinfo response;
- confidential client `pagerless-server` with only the service-account flow,
  whose service account holds the `realm-management` roles `view-users` and
  `query-groups`. The admission webhook uses it to confirm that an asserted
  approver is a member of `pagerless-approvers` (ADR-0020).

The script reads the master-realm admin password from Secret
`keycloak/keycloak-credentials`, which `hack/up.sh` generates once and reuses.
It writes the dashboard and server client secrets to
`pagerless/pagerless-keycloak-dashboard` (`client-id`, `client-secret`,
`server-client-id`, `server-client-secret`, `issuer`). No secret is checked in,
printed, or placed in argv.

Keycloak uses its own mounted service account token to read the API server's
OIDC discovery document and JWKS, and trusts the cluster CA through
`KC_TRUSTSTORE_PATHS`.

## Adding approvers

Create users in the realm (the admin console is available through
`kubectl -n keycloak port-forward svc/keycloak 8080:8080`) and add each one to
`pagerless-approvers`. The dashboard reads the group from the ID token to decide
whether to show the Approve and Reject controls, and the admission webhook asks
Keycloak again before it accepts the asserted approver.

## Credentials

The chart reads only Secret `keycloak-credentials` (`admin-password`,
`database-password`). No Keycloak secret is stored in this repository, and the
chart contains no generated password.

## Versions

| Component | Version | Pin |
| --- | --- | --- |
| Keycloak | `26.8.2` | `quay.io/keycloak/keycloak:26.8.2@sha256:c68e286cacd87b942ac1939a0a1481461109d6acd3a57dc496969a319a76bbf0` |
| PostgreSQL | `17.12` | `docker.io/library/postgres:17.12-alpine@sha256:a77aa70e641402a21411b92b721c58d10f23b50ae5fdd2333efd22d166e1e5f7` |

`client-auth-federated:v1` is enabled by default in 26.8.2, so no `KC_FEATURES`
flag is needed.
