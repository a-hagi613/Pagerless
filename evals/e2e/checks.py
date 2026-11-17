"""Validate the Executor's exact adapter pull-request head before allowing its merge."""

from __future__ import annotations

import argparse
import base64
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from client import request


def command(arguments: list[str], *, env: dict[str, str] | None = None) -> str:
    result = subprocess.run(arguments, env=env, capture_output=True, text=True, check=False, timeout=90)
    if result.returncode:
        # Git diagnostics may contain headers or credential helpers. Keep them private.
        raise RuntimeError(f"the required check's {Path(arguments[0]).name} command failed")
    return result.stdout.strip()


def git_environment(url: str, token: str, ca_file: str) -> dict[str, str]:
    environment = {
        key: value for key, value in os.environ.items() if not key.startswith(("GIT_", "GH_", "GITHUB_"))
    }
    authorization = base64.b64encode(f"pagerless:{token}".encode()).decode()
    environment.update(
        {
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": "/dev/null",
            "GIT_TERMINAL_PROMPT": "0",
            "GIT_ALLOW_PROTOCOL": "https",
            "GIT_SSL_CAINFO": ca_file,
            "GIT_CONFIG_COUNT": "2",
            "GIT_CONFIG_KEY_0": f"http.{url}.extraHeader",
            "GIT_CONFIG_VALUE_0": f"Authorization: Basic {authorization}",
            "GIT_CONFIG_KEY_1": "credential.helper",
            "GIT_CONFIG_VALUE_1": "",
        }
    )
    return environment


def validate_head(pr: dict[str, Any], environment: dict[str, str]) -> str:
    sha = (pr.get("head") or {}).get("sha", "")
    if not isinstance(sha, str) or not re.fullmatch("[0-9a-f]{40}", sha):
        raise RuntimeError("the adapter pull request has no exact head SHA")
    url = environment["PAGERLESS_E2E_GIT_URL"]
    parsed = urlsplit(url)
    repository = environment["PAGERLESS_E2E_REPOSITORY"]
    if (
        parsed.scheme != "https"
        or parsed.hostname not in {"localhost", "127.0.0.1"}
        or parsed.path != f"/git/{repository}.git"
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
    ):
        raise RuntimeError("required checks may only read the loopback HTTPS adapter repository")
    token = environment["GITHUB_READ_TOKEN"]
    # The GitHub MCP server forwards only GitHub-shaped bearer tokens, and the
    # e2e marker keeps a disposable credential distinguishable from a real one.
    if not token.startswith("ghp_e2e"):
        raise RuntimeError("required checks require the disposable adapter read token")
    kubeconform = environment["PAGERLESS_E2E_KUBECONFORM_BIN"]
    if command([kubeconform, "-v"]) != "v0.8.0":
        raise RuntimeError("the required check must use kubeconform v0.8.0")
    git_env = git_environment(url, token, environment["PAGERLESS_E2E_CA_FILE"])
    with tempfile.TemporaryDirectory(prefix="pagerless-e2e-check-") as directory:
        work = str(Path(directory) / "work")
        command(["git", "clone", "--quiet", "--no-checkout", url, work], env=git_env)
        command(
            ["git", "-C", work, "fetch", "--quiet", "--depth=1", "origin", sha],
            env=git_env,
        )
        command(["git", "-C", work, "checkout", "--quiet", "--detach", sha], env=git_env)
        if command(["git", "-C", work, "rev-parse", "HEAD"], env=git_env) != sha:
            raise RuntimeError("the required check did not check out the pull request head")
        command(
            [
                kubeconform,
                "-strict",
                "-summary",
                "-kubernetes-version",
                "1.37.0",
                str(Path(work) / "workloads"),
            ]
        )
    return sha


def check_remediation(triage: str, timeout: int, environment: dict[str, str]) -> str:
    repository = environment["PAGERLESS_E2E_REPOSITORY"]
    base = environment["PAGERLESS_E2E_API_URL"]
    control = environment["PAGERLESS_E2E_CONTROL_URL"]
    for address, scheme in ((base, "https"), (control, "http")):
        parsed = urlsplit(address)
        if parsed.scheme != scheme or parsed.hostname not in {"localhost", "127.0.0.1"}:
            raise RuntimeError("required checks may only contact the loopback adapter")
    token = environment["PAGERLESS_E2E_TOKEN"]
    if not token.startswith("e2e-write-"):
        raise RuntimeError("required checks require a disposable adapter write token")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        raw = command(
            [
                "kubectl",
                "--context",
                "kind-pagerless",
                "-n",
                "pagerless",
                "get",
                "triage",
                triage,
                "-o",
                "json",
            ]
        )
        status = json.loads(raw).get("status", {})
        if status.get("phase") in {
            "Failed",
            "Rejected",
            "Invalid",
            "Expired",
            "NotRecovered",
        }:
            raise RuntimeError("the Triage ended before its remediation check passed")
        pr_url = (status.get("execution") or {}).get("pullRequestURL", "")
        match = re.fullmatch(rf"https://github\.com/{re.escape(repository)}/pull/([0-9]+)", pr_url)
        if match:
            number = int(match[1])
            pr = request(
                base,
                environment["GITHUB_READ_TOKEN"],
                "GET",
                f"/repos/{repository}/pulls/{number}",
                ca_file=environment["PAGERLESS_E2E_CA_FILE"],
            )
            if pr.get("state") != "open":
                raise RuntimeError("the remediation pull request closed before the required check")
            if pr.get("auto_merge"):
                sha = validate_head(pr, environment)
                merged = request(
                    control,
                    token,
                    "POST",
                    f"/__e2e/checks/{number}",
                    {"status": "success", "sha": sha},
                )
                commit = merged.get("merge_commit_sha", "")
                if not isinstance(commit, str) or not re.fullmatch("[0-9a-f]{40}", commit):
                    raise RuntimeError("the adapter did not report the checked remediation merge")
                return commit
        time.sleep(2)
    raise RuntimeError("the Executor did not open an auto-merging remediation pull request before timeout")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--triage", required=True)
    parser.add_argument("--timeout", type=int, default=300)
    arguments = parser.parse_args()
    try:
        print(check_remediation(arguments.triage, arguments.timeout, dict(os.environ)))
    except (
        KeyError,
        OSError,
        RuntimeError,
        ValueError,
        subprocess.TimeoutExpired,
    ) as error:
        print(f"Remediation manifest check failed: {error}", file=sys.stderr)
        raise SystemExit(1) from None
