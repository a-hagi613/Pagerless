"""The agent's configuration, which the server sets as environment variables on the agent Job.

The names match `internal/agentapi/contract.go` and the Job that
`internal/controller/job.go` builds.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Literal, cast
from urllib.parse import urlsplit

if TYPE_CHECKING:
    from pagerless_agent.mcp import MCPServerConfig, ServerName

ENV_SERVER_URL = "PAGERLESS_SERVER_URL"
ENV_TOKEN_FILE = "PAGERLESS_TOKEN_FILE"
ENV_TRIAGE = "PAGERLESS_TRIAGE"
ENV_WORKLOAD_KIND = "PAGERLESS_WORKLOAD_KIND"
ENV_WORKLOAD_NAMESPACE = "PAGERLESS_WORKLOAD_NAMESPACE"
ENV_WORKLOAD_NAME = "PAGERLESS_WORKLOAD_NAME"
ENV_TRIGGERING_POD = "PAGERLESS_TRIGGERING_POD"
ENV_HINT = "PAGERLESS_HINT"
ENV_MODEL_PROVIDER = "PAGERLESS_MODEL_PROVIDER"
ENV_MCP_KUBERNETES_URL = "PAGERLESS_MCP_KUBERNETES_URL"
ENV_MCP_GRAFANA_URL = "PAGERLESS_MCP_GRAFANA_URL"
ENV_MCP_ARGOCD_URL = "PAGERLESS_MCP_ARGOCD_URL"
ENV_MCP_GITHUB_URL = "PAGERLESS_MCP_GITHUB_URL"
ENV_MCP_GRAFANA_TOKEN = "PAGERLESS_MCP_GRAFANA_TOKEN"
ENV_MCP_ARGOCD_TOKEN = "PAGERLESS_MCP_ARGOCD_TOKEN"
ENV_MCP_GITHUB_TOKEN = "PAGERLESS_MCP_GITHUB_TOKEN"
ENV_REPLAY_FILE = "PAGERLESS_REPLAY_FILE"
ENV_RECORDING_FILE = "PAGERLESS_RECORDING_FILE"
ENV_SANDBOX_OWNER = "PAGERLESS_SANDBOX_OWNER"
ENV_SANDBOX_REPO = "PAGERLESS_SANDBOX_REPO"
ENV_ARGOCD_APPLICATION = "PAGERLESS_ARGOCD_APPLICATION"
ENV_ALLOWED_NAMESPACES = "PAGERLESS_ALLOWED_NAMESPACES"


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


@dataclass(frozen=True)
class RuntimeSettings:
    """Provider selection and direct read-only MCP connections for one agent process."""

    investigation: Settings
    servers: tuple[MCPServerConfig, ...]
    sandbox_owner: str
    sandbox_repository: str
    application: str = "sandbox"
    provider: Literal["replay", "anthropic", "bedrock"] = "replay"
    replay_file: Path = Path("/app/recordings/oom.json")
    recording_file: Path | None = None

    @classmethod
    def from_env(cls, env: Mapping[str, str]) -> RuntimeSettings:
        """Refuse missing endpoints or configuration outside the investigation's read scope."""
        from pagerless_agent.mcp import MCPServerConfig

        investigation = Settings.from_env(env)
        provider = env.get(ENV_MODEL_PROVIDER, "replay")
        if provider not in {"replay", "anthropic", "bedrock"}:
            raise ConfigError(f"{ENV_MODEL_PROVIDER} must be replay, anthropic, or bedrock")
        namespace = investigation.target.namespace
        allowed = env.get(ENV_ALLOWED_NAMESPACES, namespace).split(",")
        if namespace not in {name.strip() for name in allowed}:
            raise ConfigError("the Workload's namespace is not allowlisted")
        endpoints: tuple[tuple[ServerName, str, str | None], ...] = (
            ("k8s", ENV_MCP_KUBERNETES_URL, None),
            ("grafana", ENV_MCP_GRAFANA_URL, ENV_MCP_GRAFANA_TOKEN),
            ("argocd", ENV_MCP_ARGOCD_URL, ENV_MCP_ARGOCD_TOKEN),
            ("github", ENV_MCP_GITHUB_URL, ENV_MCP_GITHUB_TOKEN),
        )
        required = [ENV_SANDBOX_OWNER, ENV_SANDBOX_REPO, *(url for _, url, _ in endpoints)]
        missing = [name for name in required if not env.get(name)]
        if missing:
            raise ConfigError(f"missing environment variables: {', '.join(missing)}")
        servers = []
        for name, variable, token_variable in endpoints:
            url = env[variable]
            parsed = urlsplit(url)
            if (
                parsed.scheme not in {"http", "https"}
                or not parsed.hostname
                or parsed.username
                or parsed.password
                or parsed.fragment
            ):
                raise ConfigError(f"{variable} must be an HTTP(S) endpoint without embedded credentials")
            token = env.get(token_variable) if token_variable else None
            if token and any(character in token for character in "\r\n"):
                raise ConfigError(f"{token_variable} contains an invalid bearer token")
            servers.append(MCPServerConfig(name, url, token))
        recording = env.get(ENV_RECORDING_FILE)
        if recording and provider == "replay":
            raise ConfigError("Replay cannot produce a live recording")
        return cls(
            investigation=investigation,
            servers=tuple(servers),
            sandbox_owner=env[ENV_SANDBOX_OWNER],
            sandbox_repository=env[ENV_SANDBOX_REPO],
            application=env.get(ENV_ARGOCD_APPLICATION, "sandbox"),
            provider=cast(Literal["replay", "anthropic", "bedrock"], provider),
            replay_file=Path(env.get(ENV_REPLAY_FILE, "/app/recordings/oom.json")),
            recording_file=Path(recording) if recording else None,
        )
