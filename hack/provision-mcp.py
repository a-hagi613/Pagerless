#!/usr/bin/env python3
"""Provision local read-only MCP identities without putting credentials in argv or logs."""

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
import urllib.request


CONTEXT = "kind-" + os.environ.get("KIND_CLUSTER_NAME", "pagerless")
NAMESPACE = "pagerless"


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
        raise RuntimeError("kubectl failed while provisioning MCP identities: " + detail)
    return json.loads(result.stdout) if result.stdout.strip().startswith("{") else None


def existing_secret(name, namespace=NAMESPACE):
    result = kubectl("-n", namespace, "get", "secret", name, "--ignore-not-found", "-o", "json")
    if result is None:
        return {}
    return {key: base64.b64decode(value).decode() for key, value in result.get("data", {}).items()}


def apply_secret(name, values):
    kubectl("apply", "-f", "-", data={
        "apiVersion": "v1", "kind": "Secret", "type": "Opaque",
        "metadata": {"name": name, "namespace": NAMESPACE},
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
            deadline = time.monotonic() + 30
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


def request(base, path, *, method="GET", body=None, authorization):
    headers = {"Authorization": authorization, "Accept": "application/json"}
    payload = None
    if body is not None:
        headers["Content-Type"] = "application/json"
        payload = json.dumps(body).encode()
    req = urllib.request.Request(base + path, payload, headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            data = response.read(1 << 20)
    except urllib.error.HTTPError as error:
        # Response bodies can contain credentials. Report only the HTTP status and path.
        raise RuntimeError(f"{method} {path} returned HTTP {error.code}") from None
    return json.loads(data) if data else {}


def grafana_token():
    saved = existing_secret("mcp-backend-tokens").get("grafana")
    with forward("monitoring", "kube-prometheus-stack-grafana", 80) as base:
        if saved:
            request(base, "/api/datasources", authorization="Bearer " + saved)
            return saved
        admin = existing_secret("kube-prometheus-stack-grafana", "monitoring")
        authorization = "Basic " + base64.b64encode(
            (admin["admin-user"] + ":" + admin["admin-password"]).encode()
        ).decode()
        accounts = request(base, "/api/serviceaccounts/search?query=pagerless-mcp", authorization=authorization)
        account = next((a for a in accounts["serviceAccounts"] if a["name"] == "pagerless-mcp"), None)
        if account is None:
            account = request(base, "/api/serviceaccounts", method="POST", authorization=authorization,
                              body={"name": "pagerless-mcp", "role": "Viewer", "isDisabled": False})
        elif account["role"] != "Viewer":
            raise RuntimeError("the existing pagerless-mcp Grafana service account must have the Viewer role")
        token = request(base, f"/api/serviceaccounts/{account['id']}/tokens", method="POST",
                        authorization=authorization,
                        body={"name": "pagerless-" + secrets.token_hex(8), "secondsToLive": 0})
        return token["key"]


def argocd_token():
    saved = existing_secret("argocd-read").get("token")
    with forward("argocd", "argocd-server", 80) as base:
        if saved:
            request(base, "/api/v1/applications/sandbox", authorization="Bearer " + saved)
            return saved
        password = existing_secret("argocd-initial-admin-secret", "argocd").get("password")
        if not password:
            raise RuntimeError("argocd-read is missing and the initial Argo CD admin Secret is unavailable")
        session = request(base, "/api/v1/session", method="POST", authorization="",
                          body={"username": "admin", "password": password})
        token = request(base, "/api/v1/account/pagerless/token", method="POST",
                        authorization="Bearer " + session["token"], body={"expiresIn": 0})
        return token["token"]


def main():
    grafana = grafana_token()
    argocd = argocd_token()
    apply_secret("mcp-backend-tokens", {"grafana": grafana})
    apply_secret("argocd-read", {"token": argocd})
    callers = existing_secret("mcp-client-tokens")
    apply_secret("mcp-client-tokens", {
        "grafana": callers.get("grafana") or secrets.token_hex(32),
        "argocd": callers.get("argocd") or secrets.token_hex(32),
        "github": os.environ["GITHUB_READ_TOKEN"],
    })
    provider = os.environ.get("MODEL_PROVIDER", "replay")
    credentials = {}
    if provider == "anthropic":
        credentials["ANTHROPIC_API_KEY"] = os.environ["ANTHROPIC_API_KEY"]
    elif provider == "bedrock":
        for name in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"):
            if os.environ.get(name):
                credentials[name] = os.environ[name]
    apply_secret("pagerless-agent-credentials", credentials)
    owner = os.environ.get("PAGERLESS_SANDBOX_OWNER", "")
    repo = os.environ.get("PAGERLESS_SANDBOX_REPO", "")
    if not owner and not repo:
        slug = os.environ["SANDBOX_URL"].removeprefix("https://github.com/").removesuffix("/").removesuffix(".git")
        parts = slug.split("/")
        if len(parts) != 2:
            raise RuntimeError("set PAGERLESS_SANDBOX_OWNER and PAGERLESS_SANDBOX_REPO for a non-GitHub Sandbox URL")
        owner, repo = parts
    if any(not re.fullmatch(r"[A-Za-z0-9_.-]+", value) or value in (".", "..") for value in (owner, repo)):
        raise RuntimeError("the Sandbox owner and repository must each be a single nonempty GitHub path segment")
    values = {
        "PAGERLESS_MODEL_PROVIDER": provider,
        "PAGERLESS_MCP_KUBERNETES_URL": "http://mcp-kubernetes.pagerless.svc:8080/mcp",
        "PAGERLESS_MCP_GRAFANA_URL": "http://mcp-grafana.pagerless.svc:8000/mcp",
        "PAGERLESS_MCP_ARGOCD_URL": "http://mcp-argocd.pagerless.svc:3000/mcp",
        "PAGERLESS_MCP_GITHUB_URL": os.environ.get(
            "PAGERLESS_MCP_GITHUB_URL", "http://mcp-github.pagerless.svc:8082/readonly"
        ) or "http://mcp-github.pagerless.svc:8082/readonly",
        "PAGERLESS_REPLAY_FILE": os.environ.get("PAGERLESS_REPLAY_FILE", "/app/recordings/oom.json"),
        "PAGERLESS_NARRATIVE_REPLAY_FILE": os.environ.get(
            "PAGERLESS_NARRATIVE_REPLAY_FILE", "/app/recordings/narrative.json"
        ),
        "PAGERLESS_SANDBOX_OWNER": owner,
        "PAGERLESS_SANDBOX_REPO": repo,
        "PAGERLESS_ARGOCD_APPLICATION": "sandbox",
        "PAGERLESS_ALLOWED_NAMESPACES": "sandbox",
        "PAGERLESS_SANDBOX_URL": os.environ["SANDBOX_URL"],
    }
    for name in ("PAGERLESS_RECORDING_FILE", "PAGERLESS_RECORDING_PVC", "AWS_REGION"):
        if os.environ.get(name):
            values[name] = os.environ[name]
    kubectl("apply", "-f", "-", data={
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "pagerless-agent-config", "namespace": NAMESPACE}, "data": values,
    })
    print("Provisioned read-only MCP accounts, caller tokens, and agent configuration.")


if __name__ == "__main__":
    try:
        main()
    except (KeyError, RuntimeError, urllib.error.URLError) as error:
        raise SystemExit("MCP provisioning failed: " + str(error)) from None
