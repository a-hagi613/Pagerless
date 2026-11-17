"""The agent's configuration, which the server sets as environment variables on the agent Job.

The names match `internal/agentapi/contract.go` and the Job that
`internal/controller/job.go` builds.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Literal, cast

ENV_SERVER_URL = "PAGERLESS_SERVER_URL"
ENV_TOKEN_FILE = "PAGERLESS_TOKEN_FILE"
ENV_TRIAGE = "PAGERLESS_TRIAGE"
ENV_WORKLOAD_KIND = "PAGERLESS_WORKLOAD_KIND"
ENV_WORKLOAD_NAMESPACE = "PAGERLESS_WORKLOAD_NAMESPACE"
ENV_WORKLOAD_NAME = "PAGERLESS_WORKLOAD_NAME"
ENV_TRIGGERING_POD = "PAGERLESS_TRIGGERING_POD"
ENV_HINT = "PAGERLESS_HINT"


class ConfigError(Exception):
    """The environment does not describe a valid investigation."""


@dataclass(frozen=True)
class Target:
    """The Workload under investigation, and what started the Triage."""

    triage: str
    kind: Literal["Deployment", "StatefulSet"]
    namespace: str
    name: str
    triggering_pod: str | None = None
    hint: str | None = None


@dataclass(frozen=True)
class Settings:
    """Where to report, and what to investigate."""

    server_url: str
    token_file: Path
    target: Target

    @classmethod
    def from_env(cls, env: Mapping[str, str]) -> Settings:
        """Read the settings, naming every variable that is missing or invalid."""
        required = [
            ENV_SERVER_URL,
            ENV_TOKEN_FILE,
            ENV_TRIAGE,
            ENV_WORKLOAD_KIND,
            ENV_WORKLOAD_NAMESPACE,
            ENV_WORKLOAD_NAME,
        ]
        missing = [name for name in required if not env.get(name)]
        if missing:
            raise ConfigError(f"missing environment variables: {', '.join(missing)}")
        kind = env[ENV_WORKLOAD_KIND]
        if kind not in ("Deployment", "StatefulSet"):
            raise ConfigError(f"{ENV_WORKLOAD_KIND} must be Deployment or StatefulSet, not {kind!r}")
        return cls(
            server_url=env[ENV_SERVER_URL],
            token_file=Path(env[ENV_TOKEN_FILE]),
            target=Target(
                triage=env[ENV_TRIAGE],
                kind=cast(Literal["Deployment", "StatefulSet"], kind),
                namespace=env[ENV_WORKLOAD_NAMESPACE],
                name=env[ENV_WORKLOAD_NAME],
                triggering_pod=env.get(ENV_TRIGGERING_POD) or None,
                hint=env.get(ENV_HINT) or None,
            ),
        )
