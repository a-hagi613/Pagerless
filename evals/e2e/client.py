"""The host-side client for the isolated Sandbox adapter used by `make break`."""

from __future__ import annotations

import argparse
import json
import os
import ssl
import sys
import time
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.request import Request, build_opener, HTTPSHandler


def request(
    base: str, token: str, method: str, route: str, payload: dict[str, object] | None = None, *,
    ca_file: str | None = None,
) -> dict[str, Any]:
    headers = {"Authorization": f"Bearer {token}", "Accept": "application/json"}
    data = None
    if payload is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(payload).encode()
    req = Request(base + route, data=data, headers=headers, method=method)
    try:
        context = ssl.create_default_context(cafile=ca_file) if ca_file else None
        opener = build_opener(HTTPSHandler(context=context))
        with opener.open(req, timeout=30) as response:
            raw = response.read(1 << 20)
    except HTTPError as error:
        raise RuntimeError(f"adapter request {method} {route} returned HTTP {error.code}") from None
    except URLError:
        raise RuntimeError(f"adapter request {method} {route} failed") from None
    try:
        result = json.loads(raw)
    except json.JSONDecodeError:
        raise RuntimeError(f"adapter request {method} {route} returned invalid JSON") from None
    if not isinstance(result, dict):
        raise RuntimeError(f"adapter request {method} {route} returned an unexpected response")
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("open", "enable", "pass-check", "wait", "reset"))
    parser.add_argument("--repository", required=True)
    parser.add_argument("--branch", default="")
    parser.add_argument("--title", default="")
    parser.add_argument("--body", default="")
    parser.add_argument("--number", type=int, default=0)
    parser.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args()
    base = os.environ["PAGERLESS_E2E_API_URL"].rstrip("/")
    control = os.environ["PAGERLESS_E2E_CONTROL_URL"].rstrip("/")
    token = os.environ["PAGERLESS_E2E_TOKEN"]
    ca_file = os.environ["PAGERLESS_E2E_CA_FILE"]
    repo = args.repository

    if args.command == "open":
        pr = request(base, token, "POST", f"/repos/{repo}/pulls", {
            "title": args.title, "body": args.body, "head": args.branch, "base": "main",
        }, ca_file=ca_file)
        print(pr["number"])
    elif args.command == "enable":
        pr = request(base, token, "GET", f"/repos/{repo}/pulls/{args.number}", ca_file=ca_file)
        request(base, token, "POST", "/graphql", {
            "query": "mutation($id: ID!, $method: PullRequestMergeMethod!) { enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method}) { pullRequest { number } } }",
            "variables": {"id": pr["node_id"], "method": "SQUASH"},
        }, ca_file=ca_file)
    elif args.command == "pass-check":
        request(control, token, "POST", f"/__e2e/checks/{args.number}", {"status": "success"})
    elif args.command == "reset":
        request(control, token, "POST", "/__e2e/reset", {})
    else:
        deadline = time.monotonic() + args.timeout
        while time.monotonic() < deadline:
            pr = request(base, token, "GET", f"/repos/{repo}/pulls/{args.number}", ca_file=ca_file)
            if pr.get("merged") is True and isinstance(pr.get("merge_commit_sha"), str):
                print(pr["merge_commit_sha"])
                return 0
            if pr.get("state") != "open":
                raise RuntimeError("the Scenario pull request closed without merging")
            time.sleep(2)
        raise RuntimeError("the Scenario pull request did not merge before timeout")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (KeyError, RuntimeError, TypeError) as error:
        print(f"E2E adapter client failed: {error}", file=sys.stderr)
        raise SystemExit(1) from None
