"""A thin, explicit kubectl client for the evaluation harness.

Every call names its context or kubeconfig, so a harness run can never fall back
to whatever context the developer's shell happens to select. Failures carry the
operation and a bounded diagnostic, never an environment dump.
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path
from typing import Any


class KubectlError(RuntimeError):
    """A kubectl call failed or returned something the harness cannot use."""


def default_context() -> str:
    cluster = os.environ.get("KIND_CLUSTER_NAME", "pagerless")
    return f"kind-{cluster}"


class Kubectl:
    """Runs kubectl against one cluster with one explicit identity."""

    def __init__(self, context: str | None = None, kubeconfig: Path | None = None) -> None:
        self.context = context or default_context()
        self.kubeconfig = kubeconfig

    def _base(self) -> list[str]:
        arguments = ["kubectl"]
        if self.kubeconfig is not None:
            arguments += ["--kubeconfig", str(self.kubeconfig)]
        else:
            arguments += ["--context", self.context]
        return arguments

    def run(
        self,
        *arguments: str,
        input_text: str | None = None,
        timeout: int = 120,
        check: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        command = [*self._base(), *arguments]
        try:
            result = subprocess.run(
                command,
                input=input_text,
                capture_output=True,
                text=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise KubectlError(f"{' '.join(arguments)} could not run: {error}") from None
        if check and result.returncode:
            raise KubectlError(
                f"kubectl {' '.join(arguments)} failed with exit {result.returncode}: "
                f"{_bounded(result.stderr)}"
            )
        return result

    def text(self, *arguments: str, **kwargs: Any) -> str:
        return self.run(*arguments, **kwargs).stdout.strip()

    def json(self, *arguments: str, **kwargs: Any) -> Any:
        raw = self.run(*arguments, **kwargs).stdout
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            raise KubectlError(f"kubectl {' '.join(arguments)} returned invalid JSON") from None

    def get_json(self, resource: str, name: str, namespace: str | None = None) -> dict[str, Any]:
        arguments = ["get", resource, name, "-o", "json"]
        if namespace:
            arguments += ["-n", namespace]
        payload = self.json(*arguments)
        if not isinstance(payload, dict):
            raise KubectlError(f"kubectl get {resource}/{name} did not return an object")
        return payload

    def apply(self, document: dict[str, Any]) -> None:
        self.run("apply", "-f", "-", input_text=json.dumps(document))

    def create(self, document: dict[str, Any]) -> None:
        self.run("create", "-f", "-", input_text=json.dumps(document))

    def cluster_ready(self) -> None:
        result = self.run("get", "--raw", "/readyz", timeout=30, check=False)
        if result.returncode or result.stdout.strip() != "ok":
            raise KubectlError(f"the {self.context} cluster is not reachable. Create it with make up.")

    def context_exists(self) -> bool:
        result = self.run("config", "get-contexts", "-o", "name", timeout=30, check=False)
        return self.context in result.stdout.split()


def _bounded(text: str, limit: int = 800) -> str:
    collapsed = " ".join(text.split())
    return collapsed if len(collapsed) <= limit else collapsed[: limit - 1] + "…"
