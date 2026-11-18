#!/usr/bin/env python3
"""Provision the Pagerless Keycloak realm, clients, group, and Kubernetes identity provider.

The bootstrap is idempotent: it creates what is missing and updates the fields
this repository owns. It reads the master-realm admin password from the
cluster's Secret and writes the dashboard client secret back to a Secret, so no
credential appears in the repository, in argv, or in the output.
"""

import base64
import contextlib
import json
import os
import re
import secrets
import selectors
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

CONTEXT = os.environ.get("PAGERLESS_KUBE_CONTEXT") or "kind-" + os.environ.get("KIND_CLUSTER_NAME", "pagerless")
KEYCLOAK_NAMESPACE = "keycloak"
PAGERLESS_NAMESPACE = "pagerless"
CREDENTIALS_SECRET = "keycloak-credentials"
DASHBOARD_SECRET = "pagerless-keycloak-dashboard"
OPERATOR_SECRET = "pagerless-dashboard-operator"
REALM = "pagerless"
KUBERNETES_ISSUER = "https://kubernetes.default.svc.cluster.local"
AGENT_CLIENT = "pagerless-agent"
AGENT_SERVICE_ACCOUNT = "pagerless-agent"
DASHBOARD_CLIENT = "pagerless-dashboard"
SERVER_CLIENT = "pagerless-server"
APPROVERS_GROUP = "pagerless-approvers"
OPERATOR_USER = os.environ.get("PAGERLESS_DASHBOARD_USER", "").strip() or "pagerless-approver"


def dashboard_redirects():
    redirects = [
        "http://localhost:8080/*",
        "http://127.0.0.1:8080/*",
        "http://devbox:8080/*",
        "http://100.75.231.118:8080/*",
    ]
    custom = os.environ.get("PAGERLESS_DASHBOARD_BASE_URL", "").strip().rstrip("/")
    if custom:
        candidate = custom + "/*"
        if candidate not in redirects:
            redirects.append(candidate)
    return redirects


DASHBOARD_REDIRECTS = dashboard_redirects()


def kubectl(*args, data=None):
    result = subprocess.run(
        ["kubectl", "--context", CONTEXT, *args],
        input=None if data is None else json.dumps(data),
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode:
        detail = "the API rejected a Secret" if data and data.get("kind") == "Secret" else result.stderr.strip()
        raise RuntimeError("kubectl failed while provisioning the Keycloak realm: " + detail)
    return json.loads(result.stdout) if result.stdout.strip().startswith("{") else None


def existing_secret(name, namespace):
    result = kubectl("-n", namespace, "get", "secret", name, "--ignore-not-found", "-o", "json")
    if result is None:
        return {}
    return {key: base64.b64decode(value).decode() for key, value in result.get("data", {}).items()}


def apply_secret(name, namespace, values):
    kubectl("apply", "-f", "-", data={
        "apiVersion": "v1", "kind": "Secret", "type": "Opaque",
        "metadata": {"name": name, "namespace": namespace},
        "data": {key: base64.b64encode(value.encode()).decode() for key, value in values.items()},
    })


@contextlib.contextmanager
def forward(namespace, service, remote_port):
    process = subprocess.Popen(
        ["kubectl", "--context", CONTEXT, "-n", namespace, "port-forward", "svc/" + service,
         "0:" + str(remote_port), "--address=127.0.0.1"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
    )
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise RuntimeError("port-forward exited for " + service)
                if not selector.select(timeout=1):
                    continue
                line = process.stdout.readline()
                match = re.search(r"Forwarding from 127\.0\.0\.1:(\d+)", line)
                if match:
                    yield "http://127.0.0.1:" + match.group(1)
                    return
            raise RuntimeError("timed out forwarding " + service)
    finally:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        process.stdout.close()


class Admin:
    """The Keycloak Admin REST API, without logging request or response bodies."""

    def __init__(self, base, token):
        self.base = base
        self.token = token

    def call(self, method, path, payload=None):
        url = self.base + path
        data = json.dumps(payload).encode() if payload is not None else None
        request = urllib.request.Request(url, data=data, method=method)
        request.add_header("Content-Type", "application/json")
        request.add_header("Authorization", "Bearer " + self.token)
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                body = response.read().decode()
                return response.status, json.loads(body) if body else None
        except urllib.error.HTTPError as error:
            # Bodies can name users or clients. Report only the status and path.
            error.read()
            return error.code, None

    def require(self, method, path, payload=None):
        status, body = self.call(method, path, payload)
        if status not in (200, 201, 204):
            raise RuntimeError(f"{method} {path} returned HTTP {status}")
        return body


def admin_token(base, password):
    form = urllib.parse.urlencode({
        "grant_type": "password",
        "client_id": "admin-cli",
        "username": "admin",
        "password": password,
    }).encode()
    request = urllib.request.Request(
        base + "/realms/master/protocol/openid-connect/token",
        data=form,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.loads(response.read())["access_token"]
    except urllib.error.HTTPError as error:
        error.read()
        raise RuntimeError(f"the Keycloak admin token request returned HTTP {error.code}") from None


def ensure_realm(admin):
    status, realm = admin.call("GET", f"/admin/realms/{REALM}")
    if status == 200:
        realm["enabled"] = True
        realm["sslRequired"] = "NONE"
        admin.require("PUT", f"/admin/realms/{REALM}", realm)
        return
    admin.require("POST", "/admin/realms", {"realm": REALM, "enabled": True, "sslRequired": "NONE"})


def ensure_group(admin):
    _, groups = admin.call("GET", f"/admin/realms/{REALM}/groups?search={APPROVERS_GROUP}&exact=true")
    if not groups:
        admin.require("POST", f"/admin/realms/{REALM}/groups", {"name": APPROVERS_GROUP})
        _, groups = admin.call("GET", f"/admin/realms/{REALM}/groups?search={APPROVERS_GROUP}&exact=true")
    if not groups:
        raise RuntimeError(f"Keycloak did not create the {APPROVERS_GROUP} group")
    return groups[0]["id"]


def ensure_operator_user(admin, group_id):
    saved = existing_secret(OPERATOR_SECRET, PAGERLESS_NAMESPACE)
    username = OPERATOR_USER
    password = saved.get("password") or secrets.token_urlsafe(18)
    query = urllib.parse.quote(username)
    _, found = admin.call("GET", f"/admin/realms/{REALM}/users?username={query}&exact=true")
    user = found[0] if found else {
        "username": username,
        "enabled": True,
        "emailVerified": True,
        "firstName": "Pagerless",
        "lastName": "Approver",
        "email": f"{username}@pagerless.local",
        "requiredActions": [],
    }
    user.update({
        "enabled": True,
        "emailVerified": True,
        "firstName": user.get("firstName") or "Pagerless",
        "lastName": user.get("lastName") or "Approver",
        "email": user.get("email") or f"{username}@pagerless.local",
        "requiredActions": [],
    })
    if found:
        admin.require("PUT", f"/admin/realms/{REALM}/users/{user['id']}", user)
    else:
        admin.require("POST", f"/admin/realms/{REALM}/users", user)
        _, created = admin.call("GET", f"/admin/realms/{REALM}/users?username={query}&exact=true")
        if not created:
            raise RuntimeError(f"Keycloak did not create the {username} user")
        user = created[0]
    admin.require(
        "PUT",
        f"/admin/realms/{REALM}/users/{user['id']}/reset-password",
        {"type": "password", "value": password, "temporary": False},
    )
    admin.require("PUT", f"/admin/realms/{REALM}/users/{user['id']}/groups/{group_id}")
    return username, password


def ensure_kubernetes_identity_provider(admin):
    path = f"/admin/realms/{REALM}/identity-provider/instances/kubernetes"
    status, provider = admin.call("GET", path)
    if status == 200:
        provider["config"] = {**(provider.get("config") or {}), "issuer": KUBERNETES_ISSUER}
        admin.require("PUT", path, provider)
        return
    admin.require("POST", f"/admin/realms/{REALM}/identity-provider/instances", {
        "alias": "kubernetes",
        "providerId": "kubernetes",
        "enabled": True,
        "config": {"issuer": KUBERNETES_ISSUER},
    })


def clients(admin, client_id):
    _, found = admin.call("GET", f"/admin/realms/{REALM}/clients?clientId={urllib.parse.quote(client_id)}")
    return found or []


def ensure_agent_client(admin, subject):
    found = clients(admin, AGENT_CLIENT)
    if found:
        client = found[0]
    else:
        admin.require("POST", f"/admin/realms/{REALM}/clients", {
            "clientId": AGENT_CLIENT,
            "enabled": True,
            "publicClient": False,
            "serviceAccountsEnabled": True,
            "clientAuthenticatorType": "federated-jwt",
        })
        client = clients(admin, AGENT_CLIENT)[0]
    client.update({
        "enabled": True,
        "publicClient": False,
        "standardFlowEnabled": False,
        "implicitFlowEnabled": False,
        "directAccessGrantsEnabled": False,
        "serviceAccountsEnabled": True,
        # The agent proves its identity with the projected service account
        # token; no client secret exists.
        "clientAuthenticatorType": "federated-jwt",
        "attributes": {
            **(client.get("attributes") or {}),
            "jwt.credential.issuer": "kubernetes",
            "jwt.credential.sub": subject,
        },
    })
    admin.require("PUT", f"/admin/realms/{REALM}/clients/{client['id']}", client)


def ensure_dashboard_client(admin):
    saved = existing_secret(DASHBOARD_SECRET, PAGERLESS_NAMESPACE)
    found = clients(admin, DASHBOARD_CLIENT)
    if found:
        client = found[0]
    else:
        client = {
            "clientId": DASHBOARD_CLIENT,
            "enabled": True,
            "publicClient": False,
            "standardFlowEnabled": True,
            "implicitFlowEnabled": False,
            "directAccessGrantsEnabled": False,
            "serviceAccountsEnabled": False,
            "clientAuthenticatorType": "client-secret",
            "redirectUris": DASHBOARD_REDIRECTS,
            "webOrigins": [redirect.removesuffix("/*") for redirect in DASHBOARD_REDIRECTS],
        }
    secret = saved.get("client-secret") or client.get("secret") or ""
    if not secret or set(secret) == {"*"}:
        secret = secrets.token_hex(32)
    mappers = [
        mapper for mapper in (client.get("protocolMappers") or []) if mapper.get("name") != "groups"
    ]
    mappers.append({
        "name": "groups",
        "protocol": "openid-connect",
        "protocolMapper": "oidc-group-membership-mapper",
        "config": {
            "claim.name": "groups",
            "full.path": "false",
            "access.token.claim": "true",
            "id.token.claim": "true",
            "userinfo.token.claim": "true",
        },
    })
    client.update({
        "enabled": True,
        "publicClient": False,
        "standardFlowEnabled": True,
        "implicitFlowEnabled": False,
        "directAccessGrantsEnabled": False,
        "clientAuthenticatorType": "client-secret",
        "secret": secret,
        "redirectUris": DASHBOARD_REDIRECTS,
        "webOrigins": [redirect.removesuffix("/*") for redirect in DASHBOARD_REDIRECTS],
        "attributes": {
            **(client.get("attributes") or {}),
            "pkce.code.challenge.method": "S256",
        },
        "protocolMappers": mappers,
    })
    if found:
        admin.require("PUT", f"/admin/realms/{REALM}/clients/{client['id']}", client)
    else:
        admin.require("POST", f"/admin/realms/{REALM}/clients", client)
    return secret


def ensure_server_client(admin):
    """Create the client whose service account reads approver group membership.

    The admission webhook asks this client's service account whether an asserted
    approver belongs to the approvers group, so the client needs exactly the
    realm-management roles that read users and groups (ADR-0020). It has no
    browser flow and no other permission.
    """
    saved = existing_secret(DASHBOARD_SECRET, PAGERLESS_NAMESPACE)
    found = clients(admin, SERVER_CLIENT)
    if found:
        client = found[0]
    else:
        client = {
            "clientId": SERVER_CLIENT,
            "enabled": True,
            "publicClient": False,
            "standardFlowEnabled": False,
            "implicitFlowEnabled": False,
            "directAccessGrantsEnabled": False,
            "serviceAccountsEnabled": True,
            "clientAuthenticatorType": "client-secret",
        }
    secret = saved.get("server-client-secret") or client.get("secret") or ""
    if not secret or set(secret) == {"*"}:
        secret = secrets.token_hex(32)
    client.update({
        "enabled": True,
        "publicClient": False,
        "standardFlowEnabled": False,
        "implicitFlowEnabled": False,
        "directAccessGrantsEnabled": False,
        "serviceAccountsEnabled": True,
        "clientAuthenticatorType": "client-secret",
        "secret": secret,
    })
    if found:
        admin.require("PUT", f"/admin/realms/{REALM}/clients/{client['id']}", client)
    else:
        admin.require("POST", f"/admin/realms/{REALM}/clients", client)
        client = clients(admin, SERVER_CLIENT)[0]

    service_account = admin.require(
        "GET", f"/admin/realms/{REALM}/clients/{client['id']}/service-account-user")
    realm_management = clients(admin, "realm-management")
    if not realm_management:
        raise RuntimeError("the realm has no realm-management client")
    management_id = realm_management[0]["id"]
    roles = [
        admin.require("GET", f"/admin/realms/{REALM}/clients/{management_id}/roles/{name}")
        for name in ("view-users", "query-groups")
    ]
    admin.require(
        "POST",
        f"/admin/realms/{REALM}/users/{service_account['id']}/role-mappings/clients/{management_id}",
        roles,
    )
    return secret


def main():
    credentials = existing_secret(CREDENTIALS_SECRET, KEYCLOAK_NAMESPACE)
    if "admin-password" not in credentials:
        raise RuntimeError(f"Secret {KEYCLOAK_NAMESPACE}/{CREDENTIALS_SECRET} has no admin-password")
    subject = f"system:serviceaccount:{PAGERLESS_NAMESPACE}:{AGENT_SERVICE_ACCOUNT}"
    with forward(KEYCLOAK_NAMESPACE, "keycloak", 8080) as base:
        admin = Admin(base, admin_token(base, credentials["admin-password"]))
        ensure_realm(admin)
        group_id = ensure_group(admin)
        ensure_kubernetes_identity_provider(admin)
        ensure_agent_client(admin, subject)
        client_secret = ensure_dashboard_client(admin)
        server_secret = ensure_server_client(admin)
        operator_username, operator_password = ensure_operator_user(admin, group_id)
        issuer = f"http://keycloak.{KEYCLOAK_NAMESPACE}.svc.cluster.local:8080/realms/{REALM}"
    apply_secret(DASHBOARD_SECRET, PAGERLESS_NAMESPACE, {
        "client-id": DASHBOARD_CLIENT,
        "client-secret": client_secret,
        "server-client-id": SERVER_CLIENT,
        "server-client-secret": server_secret,
        "issuer": issuer,
    })
    apply_secret(OPERATOR_SECRET, PAGERLESS_NAMESPACE, {
        "username": operator_username,
        "password": operator_password,
    })
    print(
        f"Provisioned realm {REALM}, group {APPROVERS_GROUP}, operator {operator_username}, "
        "and the agent, dashboard, and server clients."
    )


if __name__ == "__main__":
    try:
        main()
    except (KeyError, RuntimeError, urllib.error.URLError) as error:
        raise SystemExit("Keycloak provisioning failed: " + str(error)) from None
