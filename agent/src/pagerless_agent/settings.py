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
    from pagerless_agent.keycloak import TokenProvider
    from pagerless_agent.mcp import MCPServerConfig, ServerName

    ENV_MCP_SERVERS = "PAGERLESS_MCP_SERVERS"

ENV_SERVER_URL = "PAGERLESS_SERVER_URL"
ENV_TOKEN_FILE = "PAGERLESS_TOKEN_FILE"
ENV_TRIAGE = "PAGERLESS_TRIAGE"
ENV_TASK = "PAGERLESS_TASK"
ENV_WORKLOAD_KIND = "PAGERLESS_WORKLOAD_KIND"
ENV_WORKLOAD_NAMESPACE = "PAGERLESS_WORKLOAD_NAMESPACE"
ENV_WORKLOAD_NAME = "PAGERLESS_WORKLOAD_NAME"
ENV_TRIGGERING_POD = "PAGERLESS_TRIGGERING_POD"
ENV_HINT = "PAGERLESS_HINT"
ENV_MODEL_PROVIDER = "PAGERLESS_MODEL_PROVIDER"
ENV_MCP_GATEWAY_URL = "PAGERLESS_MCP_GATEWAY_URL"
_ENV_MCP_SERVERS = "PAGERLESS_MCP_SERVERS"
ENV_KEYCLOAK_ISSUER = "PAGERLESS_KEYCLOAK_ISSUER"
ENV_KEYCLOAK_TOKEN_URL = "PAGERLESS_KEYCLOAK_TOKEN_URL"
ENV_KEYCLOAK_CLIENT_ID = "PAGERLESS_KEYCLOAK_CLIENT_ID"
ENV_KEYCLOAK_TOKEN_FILE = "PAGERLESS_KEYCLOAK_TOKEN_FILE"
ENV_REPLAY_FILE = "PAGERLESS_REPLAY_FILE"
ENV_NARRATIVE_REPLAY_FILE = "PAGERLESS_NARRATIVE_REPLAY_FILE"
ENV_RECORDING_FILE = "PAGERLESS_RECORDING_FILE"
ENV_SANDBOX_OWNER = "PAGERLESS_SANDBOX_OWNER"
ENV_SANDBOX_REPO = "PAGERLESS_SANDBOX_REPO"
ENV_ARGOCD_APPLICATION = "PAGERLESS_ARGOCD_APPLICATION"
ENV_ALLOWED_NAMESPACES = "PAGERLESS_ALLOWED_NAMESPACES"

_ALL_MCP_SERVERS: tuple[ServerName, ...] = ("k8s", "grafana", "argocd", "github")


class ConfigError(Exception):
    """The environment does not describe a valid investigation."""


ModelProviderName = Literal["replay", "anthropic", "bedrock"]

# The values of ENV_TASK: the two kinds of Job the server starts from the agent image.
TASK_INVESTIGATE = "investigate"
TASK_NARRATIVE = "narrative"
TASKS = (TASK_INVESTIGATE, TASK_NARRATIVE)


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
class KeycloakSettings:
    """The realm and federated client that authenticate every MCP call."""

    issuer: str
    token_url: str
    client_id: str
    token_file: Path


@dataclass(frozen=True)
class RuntimeSettings:
    """Provider selection and the authenticated MCP gateway for one agent process."""

    investigation: Settings
    gateway_url: str
    keycloak: KeycloakSettings
    sandbox_owner: str = ""
    sandbox_repository: str = ""
    application: str = "sandbox"
    mcp_servers: tuple[ServerName, ...] = _ALL_MCP_SERVERS
    provider: ModelProviderName = "replay"
    replay_file: Path = Path("/app/recordings/oom.json")
    recording_file: Path | None = None

    @classmethod
    def from_env(cls, env: Mapping[str, str]) -> RuntimeSettings:
        """Refuse missing endpoints or configuration outside the investigation's read scope."""
        investigation = Settings.from_env(env)
        provider = env.get(ENV_MODEL_PROVIDER, "replay")
        if provider not in {"replay", "anthropic", "bedrock"}:
            raise ConfigError(f"{ENV_MODEL_PROVIDER} must be replay, anthropic, or bedrock")
        namespace = investigation.target.namespace
        allowed = env.get(ENV_ALLOWED_NAMESPACES, namespace).split(",")
        if namespace not in {name.strip() for name in allowed}:
            raise ConfigError("the Workload's namespace is not allowlisted")
        mcp_servers = _parse_mcp_servers(env)
        required = [
            ENV_MCP_GATEWAY_URL,
            ENV_KEYCLOAK_ISSUER,
            ENV_KEYCLOAK_TOKEN_URL,
            ENV_KEYCLOAK_CLIENT_ID,
            ENV_KEYCLOAK_TOKEN_FILE,
        ]
        if "github" in mcp_servers:
            required.extend([ENV_SANDBOX_OWNER, ENV_SANDBOX_REPO])
        missing = [name for name in required if not env.get(name)]
        if missing:
            raise ConfigError(f"missing environment variables: {', '.join(missing)}")
        gateway_url = _http_endpoint(ENV_MCP_GATEWAY_URL, env[ENV_MCP_GATEWAY_URL])
        if urlsplit(gateway_url).path not in {"", "/"}:
            raise ConfigError(f"{ENV_MCP_GATEWAY_URL} must be a base URL without a path")
        issuer = _http_endpoint(ENV_KEYCLOAK_ISSUER, env[ENV_KEYCLOAK_ISSUER])
        if issuer.endswith("/") or "/realms/" not in urlsplit(issuer).path:
            raise ConfigError(f"{ENV_KEYCLOAK_ISSUER} must name a realm without a trailing slash")
        token_url = _http_endpoint(ENV_KEYCLOAK_TOKEN_URL, env[ENV_KEYCLOAK_TOKEN_URL])
        if token_url != issuer + "/protocol/openid-connect/token":
            raise ConfigError(f"{ENV_KEYCLOAK_TOKEN_URL} must be the issuer's token endpoint")
        client_id = env[ENV_KEYCLOAK_CLIENT_ID]
        if any(character in client_id for character in " \t\r\n/"):
            raise ConfigError(f"{ENV_KEYCLOAK_CLIENT_ID} must be a single client identifier")
        recording = env.get(ENV_RECORDING_FILE)
        if recording and provider == "replay":
            raise ConfigError("Replay cannot produce a live recording")
        return cls(
            investigation=investigation,
            gateway_url=gateway_url.rstrip("/"),
            keycloak=KeycloakSettings(
                issuer=issuer,
                token_url=token_url,
                client_id=client_id,
                token_file=Path(env[ENV_KEYCLOAK_TOKEN_FILE]),
            ),
            sandbox_owner=env.get(ENV_SANDBOX_OWNER, "") if "github" in mcp_servers else "",
            sandbox_repository=env.get(ENV_SANDBOX_REPO, "") if "github" in mcp_servers else "",
            application=env.get(ENV_ARGOCD_APPLICATION, "sandbox"),
            mcp_servers=mcp_servers,
            provider=cast(ModelProviderName, provider),
            replay_file=Path(env.get(ENV_REPLAY_FILE, "/app/recordings/oom.json")),
            recording_file=Path(recording) if recording else None,
        )

    def servers(self, token_source: TokenProvider) -> tuple[MCPServerConfig, ...]:
        """Return one authenticated gateway route per enabled MCP server."""
        from pagerless_agent.mcp import MCPServerConfig, gateway_endpoints

        enabled = set(self.mcp_servers)
        return tuple(
            MCPServerConfig(name, url, token_source)
            for name, url in gateway_endpoints(self.gateway_url)
            if name in enabled
        )


def _parse_mcp_servers(env: Mapping[str, str]) -> tuple[ServerName, ...]:
    raw = env.get(_ENV_MCP_SERVERS, "").strip()
    if raw:
        seen: list[ServerName] = []
        for item in raw.split(","):
            name = item.strip()
            if not name:
                continue
            if name not in _ALL_MCP_SERVERS:
                raise ConfigError(
                    f"{_ENV_MCP_SERVERS} must contain only k8s, grafana, argocd, or github, not {name!r}"
                )
            if name not in seen:
                seen.append(name)
        if "k8s" not in seen:
            raise ConfigError(f"{_ENV_MCP_SERVERS} must include k8s")
        return tuple(seen)
    if env.get(ENV_SANDBOX_OWNER) or env.get(ENV_SANDBOX_REPO):
        return _ALL_MCP_SERVERS
    if env.get(ENV_ARGOCD_APPLICATION, "").strip():
        return ("k8s", "grafana", "argocd")
    return ("k8s", "grafana")


def __getattr__(name: str) -> str:
    if name == "ENV_MCP_SERVERS":
        return _ENV_MCP_SERVERS
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


def _http_endpoint(name: str, value: str) -> str:
    parsed = urlsplit(value)
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.hostname
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
    ):
        raise ConfigError(f"{name} must be an HTTP(S) endpoint without embedded credentials")
    return value


@dataclass(frozen=True)
class NarrativeSettings:
    """What a Narrative Job needs: where to read and post, and how to call the model."""

    server_url: str
    token_file: Path
    triage: str
    provider: ModelProviderName = "replay"
    replay_file: Path = Path("/app/recordings/narrative.json")
    recording_file: Path | None = None

    @classmethod
    def from_env(cls, env: Mapping[str, str]) -> NarrativeSettings:
        """Read the Narrative Job's settings, refusing a recording that Replay cannot produce."""
        required = [ENV_SERVER_URL, ENV_TOKEN_FILE, ENV_TRIAGE]
        missing = [name for name in required if not env.get(name)]
        if missing:
            raise ConfigError(f"missing environment variables: {', '.join(missing)}")
        provider = env.get(ENV_MODEL_PROVIDER, "replay")
        if provider not in ("replay", "anthropic", "bedrock"):
            raise ConfigError(f"{ENV_MODEL_PROVIDER} must be replay, anthropic, or bedrock")
        recording = env.get(ENV_RECORDING_FILE)
        if recording and provider == "replay":
            raise ConfigError("Replay cannot produce a live recording")
        return cls(
            server_url=env[ENV_SERVER_URL],
            token_file=Path(env[ENV_TOKEN_FILE]),
            triage=env[ENV_TRIAGE],
            provider=cast(ModelProviderName, provider),
            replay_file=Path(env.get(ENV_NARRATIVE_REPLAY_FILE, "/app/recordings/narrative.json")),
            recording_file=Path(recording) if recording else None,
        )
