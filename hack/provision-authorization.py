#!/usr/bin/env python3
"""Provision alert authentication and bind Approval to the current explicit user."""

import base64
import hashlib
import json
import os
import secrets
import subprocess
import sys

CONTEXT = "kind-" + os.environ.get("KIND_CLUSTER_NAME", "pagerless")
ALERT_SECRET = "pagerless-alertmanager"


def kubectl(*args, document=None):
    result = subprocess.run(
        ["kubectl", "--context", CONTEXT, *args],
        input=None if document is None else json.dumps(document),
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode:
        # API errors may echo submitted Secret data. Never include raw process output.
        raise RuntimeError("kubectl rejected the authorization provisioning request")
    return json.loads(result.stdout) if result.stdout.strip().startswith("{") else None


def existing_alert_token(namespace):
    secret = kubectl(
        "-n",
        namespace,
        "get",
        "secret",
        ALERT_SECRET,
        "--ignore-not-found",
        "-o",
        "json",
    )
    if secret is None:
        return None
    try:
        token = base64.b64decode(secret["data"]["token"], validate=True).decode("ascii")
    except (KeyError, ValueError, UnicodeError):
        raise RuntimeError("the existing alert authentication Secret is invalid") from None
    if len(token) < 32 or any(character.isspace() for character in token):
        raise RuntimeError("the existing alert authentication Secret must contain a nonempty random token")
    return token


def provision_alerts():
    for namespace in ("pagerless", "monitoring"):
        kubectl(
            "apply",
            "-f",
            "-",
            document={
                "apiVersion": "v1",
                "kind": "Namespace",
                "metadata": {"name": namespace},
            },
        )
    server = existing_alert_token("pagerless")
    sender = existing_alert_token("monitoring")
    if server is not None and sender is not None and server != sender:
        raise RuntimeError(
            "the server and Alertmanager authentication Secrets differ; restore the intended token first"
        )
    token = server or sender or secrets.token_urlsafe(48)
    for namespace in ("pagerless", "monitoring"):
        kubectl(
            "apply",
            "-f",
            "-",
            document={
                "apiVersion": "v1",
                "kind": "Secret",
                "type": "Opaque",
                "metadata": {"name": ALERT_SECRET, "namespace": namespace},
                "data": {"token": base64.b64encode(token.encode("ascii")).decode("ascii")},
            },
        )
    print("Provisioned matching alert authentication Secrets.")


def provision_approver():
    identity = kubectl("auth", "whoami", "-o", "json")
    username = (identity or {}).get("status", {}).get("userInfo", {}).get("username", "")
    if (
        not username
        or username == "system:anonymous"
        or username.startswith(("system:serviceaccount:", "system:node:"))
    ):
        raise RuntimeError(
            "Approval bootstrap requires an explicit user identity, not a ServiceAccount or node"
        )
    name = "pagerless-approver-" + hashlib.sha256(username.encode()).hexdigest()[:16]
    role = {
        "apiGroup": "rbac.authorization.k8s.io",
        "kind": "Role",
        "name": "pagerless-approver",
    }
    subjects = [{"apiGroup": "rbac.authorization.k8s.io", "kind": "User", "name": username}]
    existing = kubectl(
        "-n",
        "pagerless",
        "get",
        "rolebinding",
        name,
        "--ignore-not-found",
        "-o",
        "json",
    )
    if existing is not None:
        if existing.get("roleRef") != role or existing.get("subjects") != subjects:
            raise RuntimeError("the existing approver binding differs; refusing to overwrite it")
    else:
        kubectl(
            "create",
            "-f",
            "-",
            document={
                "apiVersion": "rbac.authorization.k8s.io/v1",
                "kind": "RoleBinding",
                "metadata": {"name": name, "namespace": "pagerless"},
                "roleRef": role,
                "subjects": subjects,
            },
        )
    print("Bound pagerless-approver to the current explicit user identity.")


def main():
    if sys.argv[1:] == ["alerts"]:
        provision_alerts()
    elif sys.argv[1:] == ["approver"]:
        provision_approver()
    else:
        raise RuntimeError("usage: provision-authorization.py alerts|approver")


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, ValueError, OSError) as error:
        raise SystemExit("Authorization provisioning failed: " + str(error)) from None
