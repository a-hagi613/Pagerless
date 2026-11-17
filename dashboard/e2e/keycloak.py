#!/usr/bin/env python3
"""Provision the realm, client, and approver for the dashboard smoke test.

The smoke test runs a Keycloak container instead of the cluster's identity
provider, so the browser can complete the authorization-code flow against a
localhost issuer. This script provisions the same dashboard client that
hack/provision-keycloak.py provisions for the cluster, with a redirect URI for
the smoke dashboard, and a user in `pagerless-approvers`. It writes the
generated credentials to a mode-0600 file and prints none of them.
"""

import argparse
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

REALM = "pagerless"
CLIENT = "pagerless-dashboard"
GROUP = "pagerless-approvers"
USERNAME = "smoke-approver"


def request(base, method, path, token=None, payload=None):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            body = response.read().decode()
            return response.status, json.loads(body) if body else None
    except urllib.error.HTTPError as error:
        # Bodies can name users or clients, so report only the status and path.
        error.read()
        return error.code, None


def require(base, method, path, token=None, payload=None):
    status, body = request(base, method, path, token, payload)
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
    req = urllib.request.Request(
        base + "/realms/master/protocol/openid-connect/token",
        data=form,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    with urllib.request.urlopen(req, timeout=30) as response:
        return json.loads(response.read())["access_token"]


def wait_for_keycloak(base, timeout=180):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            status, _ = request(base, "GET", "/realms/master")
            if status == 200:
                return
        except urllib.error.URLError:
            pass
        time.sleep(1)
    raise RuntimeError("Keycloak did not answer its master realm in time")


def ensure_realm(base, token):
    status, realm = request(base, "GET", f"/admin/realms/{REALM}", token)
    if status == 200:
        realm["enabled"] = True
        realm["sslRequired"] = "NONE"
        require(base, "PUT", f"/admin/realms/{REALM}", token, realm)
        return
    require(base, "POST", "/admin/realms", token,
            {"realm": REALM, "enabled": True, "sslRequired": "NONE"})


def ensure_group(base, token):
    _, groups = request(base, "GET", f"/admin/realms/{REALM}/groups?search={GROUP}&exact=true", token)
    group = groups[0] if groups else require(base, "POST", f"/admin/realms/{REALM}/groups", token,
                                             {"name": GROUP})
    return group["id"]


def ensure_client(base, token, redirect):
    _, found = request(base, "GET", f"/admin/realms/{REALM}/clients?clientId={CLIENT}", token)
    secret = secrets.token_hex(32)
    client = found[0] if found else {
        "clientId": CLIENT,
        "enabled": True,
        "publicClient": False,
        "standardFlowEnabled": True,
        "implicitFlowEnabled": False,
        "directAccessGrantsEnabled": False,
        "serviceAccountsEnabled": False,
        "clientAuthenticatorType": "client-secret",
    }
    mappers = [m for m in (client.get("protocolMappers") or []) if m.get("name") != "groups"]
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
        "redirectUris": [redirect + "/*"],
        "webOrigins": [redirect],
        "attributes": {**(client.get("attributes") or {}), "pkce.code.challenge.method": "S256"},
        "protocolMappers": mappers,
    })
    if found:
        require(base, "PUT", f"/admin/realms/{REALM}/clients/{client['id']}", token, client)
    else:
        require(base, "POST", f"/admin/realms/{REALM}/clients", token, client)
    return secret


def ensure_user(base, token, group_id):
    password = secrets.token_urlsafe(24)
    _, found = request(base, "GET", f"/admin/realms/{REALM}/users?username={USERNAME}&exact=true", token)
    user = found[0] if found else {
        "username": USERNAME,
        "enabled": True,
        "emailVerified": True,
        "firstName": "Smoke",
        "lastName": "Approver",
        "email": USERNAME + "@example.com",
        "requiredActions": [],
    }
    if found:
        require(base, "PUT", f"/admin/realms/{REALM}/users/{user['id']}", token, user)
    else:
        require(base, "POST", f"/admin/realms/{REALM}/users", token, user)
        _, created = request(base, "GET", f"/admin/realms/{REALM}/users?username={USERNAME}&exact=true", token)
        user = created[0]
    require(base, "PUT", f"/admin/realms/{REALM}/users/{user['id']}/reset-password", token,
            {"type": "password", "value": password, "temporary": False})
    require(base, "PUT", f"/admin/realms/{REALM}/users/{user['id']}/groups/{group_id}", token)
    return password


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", required=True, help="Keycloak base URL, such as http://127.0.0.1:18080")
    parser.add_argument("--admin-password", required=True)
    parser.add_argument("--redirect", required=True, help="Dashboard origin, such as http://127.0.0.1:18082")
    parser.add_argument("--state-file", required=True)
    args = parser.parse_args()

    base = args.base_url.rstrip("/")
    wait_for_keycloak(base)
    token = admin_token(base, args.admin_password)
    ensure_realm(base, token)
    group_id = ensure_group(base, token)
    secret = ensure_client(base, token, args.redirect.rstrip("/"))
    password = ensure_user(base, token, group_id)

    state = {
        "issuer": base + "/realms/" + REALM,
        "username": USERNAME,
        "password": password,
        "clientSecret": secret,
    }
    descriptor = os.open(args.state_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w") as handle:
        json.dump(state, handle)
    print(f"Provisioned realm {REALM}, client {CLIENT}, and the {USERNAME} approver.")


if __name__ == "__main__":
    try:
        main()
    except (KeyError, RuntimeError, urllib.error.URLError) as error:
        raise SystemExit("Smoke-test provisioning failed: " + str(error)) from None
