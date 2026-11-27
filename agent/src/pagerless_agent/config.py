"""Configuration of the private investigation agent service.

The Go service owns installation setup and passes the model endpoint, the internal
credential, the callback origin and the ceilings through the environment. Nothing here
reads a Kubernetes object or a GitHub credential: the agent holds neither.

Every value is validated before the server starts. A failure names the variable that is
missing or invalid and never repeats a secret.
"""

from __future__ import annotations

import json
import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import urlsplit

ENV_INTERNAL_TOKEN_FILE = "PAGERLESS_INTERNAL_TOKEN_FILE"
ENV_CALLBACK_ORIGIN = "PAGERLESS_CALLBACK_ORIGIN"
ENV_MAX_CONCURRENCY = "PAGERLESS_AGENT_MAX_CONCURRENCY"
ENV_HOST = "PAGERLESS_AGENT_HOST"
ENV_PORT = "PAGERLESS_AGENT_PORT"
ENV_MAX_REQUEST_BYTES = "PAGERLESS_AGENT_MAX_REQUEST_BYTES"
ENV_TOOL_TIMEOUT = "PAGERLESS_AGENT_TOOL_TIMEOUT_S"
ENV_READINESS_TTL = "PAGERLESS_AGENT_READINESS_TTL_S"
ENV_READINESS_TIMEOUT = "PAGERLESS_AGENT_READINESS_TIMEOUT_S"
ENV_SHUTDOWN_TIMEOUT = "PAGERLESS_AGENT_SHUTDOWN_TIMEOUT_S"

ENV_MODEL_BASE_URL = "PAGERLESS_MODEL_BASE_URL"
ENV_MODEL_NAME = "PAGERLESS_MODEL_NAME"
ENV_MODEL_API_KEY_FILE = "PAGERLESS_MODEL_API_KEY_FILE"
ENV_MODEL_HEADERS = "PAGERLESS_MODEL_HEADERS"
ENV_MODEL_MAX_OUTPUT_TOKENS = "PAGERLESS_MODEL_MAX_OUTPUT_TOKENS"
ENV_MODEL_CONTEXT_TOKENS = "PAGERLESS_MODEL_CONTEXT_TOKENS"
ENV_MODEL_CONNECT_TIMEOUT = "PAGERLESS_MODEL_CONNECT_TIMEOUT_S"
ENV_MODEL_READ_TIMEOUT = "PAGERLESS_MODEL_READ_TIMEOUT_S"
ENV_ALLOW_INSECURE_MODEL_ENDPOINT = "PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT"

ENV_BUDGET_TOOL_CALLS = "PAGERLESS_BUDGET_TOOL_CALLS"
ENV_BUDGET_MODEL_CALLS = "PAGERLESS_BUDGET_MODEL_CALLS"
ENV_BUDGET_INPUT_TOKENS = "PAGERLESS_BUDGET_INPUT_TOKENS"
ENV_BUDGET_SECONDS = "PAGERLESS_BUDGET_SECONDS"

DEFAULT_HOST = "0.0.0.0"
DEFAULT_PORT = 8080
DEFAULT_MAX_CONCURRENCY = 4
DEFAULT_MAX_REQUEST_BYTES = 1_048_576
DEFAULT_TOOL_TIMEOUT_S = 30.0
DEFAULT_READINESS_TTL_S = 300.0
DEFAULT_READINESS_TIMEOUT_S = 10.0
DEFAULT_SHUTDOWN_TIMEOUT_S = 10.0
DEFAULT_MAX_OUTPUT_TOKENS = 4096
DEFAULT_CONTEXT_TOKENS = 32_768
DEFAULT_CONNECT_TIMEOUT_S = 10.0
DEFAULT_READ_TIMEOUT_S = 90.0
DEFAULT_TOOL_CALLS = 15
DEFAULT_MODEL_CALLS = 12
DEFAULT_INPUT_TOKENS = 300_000
DEFAULT_SECONDS = 300.0

MAX_TOKEN_BYTES = 8 * 1024
MAX_HEADERS = 16
MAX_HEADERS_BYTES = 4 * 1024
MAX_CONCURRENCY = 64
MAX_REQUEST_BYTES = 16 * 1024 * 1024
MAX_SECONDS = 86_400.0
MAX_INPUT_BUDGET = 100_000_000

_HEADER_NAME = re.compile(r"^[A-Za-z0-9-]{1,64}$")


class ConfigError(Exception):
    """The environment does not describe a runnable service. It never contains a secret."""


@dataclass(frozen=True)
class ModelSettings:
    """One OpenAI-compatible Chat Completions endpoint."""

    base_url: str
    api_key: str = field(repr=False)
    model: str
    headers: dict[str, str] = field(default_factory=dict, repr=False)
    max_output_tokens: int = DEFAULT_MAX_OUTPUT_TOKENS
    context_tokens: int = DEFAULT_CONTEXT_TOKENS
    connect_timeout_s: float = DEFAULT_CONNECT_TIMEOUT_S
    read_timeout_s: float = DEFAULT_READ_TIMEOUT_S


@dataclass(frozen=True)
class BudgetSettings:
    """The ceilings the service applies to one turn unless a request lowers them."""

    tool_calls: int = DEFAULT_TOOL_CALLS
    model_calls: int = DEFAULT_MODEL_CALLS
    input_tokens: int = DEFAULT_INPUT_TOKENS
    seconds: float = DEFAULT_SECONDS


@dataclass(frozen=True)
class ServiceSettings:
    """Everything the private service needs, and nothing it must fetch itself."""

    token: str = field(repr=False)
    callback_origin: str
    model: ModelSettings
    budgets: BudgetSettings = field(default_factory=BudgetSettings)
    max_concurrency: int = DEFAULT_MAX_CONCURRENCY
    host: str = DEFAULT_HOST
    port: int = DEFAULT_PORT
    max_request_bytes: int = DEFAULT_MAX_REQUEST_BYTES
    tool_timeout_s: float = DEFAULT_TOOL_TIMEOUT_S
    readiness_ttl_s: float = DEFAULT_READINESS_TTL_S
    readiness_timeout_s: float = DEFAULT_READINESS_TIMEOUT_S
    shutdown_timeout_s: float = DEFAULT_SHUTDOWN_TIMEOUT_S

    @classmethod
    def from_env(cls, env: Mapping[str, str]) -> ServiceSettings:
        """Read and validate the environment, naming every variable that is wrong."""
        required = (
            ENV_INTERNAL_TOKEN_FILE,
            ENV_CALLBACK_ORIGIN,
            ENV_MODEL_BASE_URL,
            ENV_MODEL_NAME,
            ENV_MODEL_API_KEY_FILE,
        )
        missing = [name for name in required if not env.get(name, "").strip()]
        if missing:
            raise ConfigError(f"missing environment variables: {', '.join(missing)}")
        token = _read_secret(Path(env[ENV_INTERNAL_TOKEN_FILE]), ENV_INTERNAL_TOKEN_FILE)
        callback_origin = _origin(ENV_CALLBACK_ORIGIN, env[ENV_CALLBACK_ORIGIN])
        return cls(
            token=token,
            callback_origin=callback_origin,
            model=_model_settings(env),
            budgets=BudgetSettings(
                tool_calls=_bounded_int(env, ENV_BUDGET_TOOL_CALLS, DEFAULT_TOOL_CALLS, 1, 1000),
                model_calls=_bounded_int(env, ENV_BUDGET_MODEL_CALLS, DEFAULT_MODEL_CALLS, 1, 1000),
                input_tokens=_bounded_int(
                    env, ENV_BUDGET_INPUT_TOKENS, DEFAULT_INPUT_TOKENS, 1, MAX_INPUT_BUDGET
                ),
                seconds=_bounded_float(env, ENV_BUDGET_SECONDS, DEFAULT_SECONDS, 0.0, MAX_SECONDS),
            ),
            max_concurrency=_bounded_int(
                env, ENV_MAX_CONCURRENCY, DEFAULT_MAX_CONCURRENCY, 1, MAX_CONCURRENCY
            ),
            host=_host(env.get(ENV_HOST, DEFAULT_HOST)),
            port=_bounded_int(env, ENV_PORT, DEFAULT_PORT, 1, 65_535),
            max_request_bytes=_bounded_int(
                env, ENV_MAX_REQUEST_BYTES, DEFAULT_MAX_REQUEST_BYTES, 1_024, MAX_REQUEST_BYTES
            ),
            tool_timeout_s=_bounded_float(env, ENV_TOOL_TIMEOUT, DEFAULT_TOOL_TIMEOUT_S, 0.0, 600.0),
            readiness_ttl_s=_bounded_float(env, ENV_READINESS_TTL, DEFAULT_READINESS_TTL_S, 1.0, 3_600.0),
            readiness_timeout_s=_bounded_float(
                env, ENV_READINESS_TIMEOUT, DEFAULT_READINESS_TIMEOUT_S, 0.1, 120.0
            ),
            shutdown_timeout_s=_bounded_float(
                env, ENV_SHUTDOWN_TIMEOUT, DEFAULT_SHUTDOWN_TIMEOUT_S, 0.1, 120.0
            ),
        )


def _model_settings(env: Mapping[str, str]) -> ModelSettings:
    base_url = _http_endpoint(ENV_MODEL_BASE_URL, env[ENV_MODEL_BASE_URL], allow_path=True)
    if urlsplit(base_url).scheme == "http" and env.get(ENV_ALLOW_INSECURE_MODEL_ENDPOINT) != "1":
        raise ConfigError(
            f"{ENV_MODEL_BASE_URL} must be https unless {ENV_ALLOW_INSECURE_MODEL_ENDPOINT}=1 "
            "is set for an in-cluster endpoint"
        )
    model = env[ENV_MODEL_NAME].strip()
    if not model or len(model) > 255 or any(character in model for character in "\r\n"):
        raise ConfigError(f"{ENV_MODEL_NAME} must be one model identifier")
    return ModelSettings(
        base_url=base_url,
        api_key=_read_secret(Path(env[ENV_MODEL_API_KEY_FILE]), ENV_MODEL_API_KEY_FILE),
        model=model,
        headers=_headers(env.get(ENV_MODEL_HEADERS, "")),
        max_output_tokens=_bounded_int(
            env, ENV_MODEL_MAX_OUTPUT_TOKENS, DEFAULT_MAX_OUTPUT_TOKENS, 1, 1_000_000
        ),
        context_tokens=_bounded_int(env, ENV_MODEL_CONTEXT_TOKENS, DEFAULT_CONTEXT_TOKENS, 1_024, 10_000_000),
        connect_timeout_s=_bounded_float(
            env, ENV_MODEL_CONNECT_TIMEOUT, DEFAULT_CONNECT_TIMEOUT_S, 0.1, 600.0
        ),
        read_timeout_s=_bounded_float(env, ENV_MODEL_READ_TIMEOUT, DEFAULT_READ_TIMEOUT_S, 0.1, 600.0),
    )


def _origin(name: str, value: str) -> str:
    """Return a bare HTTP(S) origin, which is the only callback destination allowed."""
    parsed = urlsplit(value)
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.hostname
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
        or any(character in value for character in "\r\n")
    ):
        raise ConfigError(f"{name} must be an HTTP(S) origin without a path, query or credentials")
    return f"{parsed.scheme}://{parsed.netloc}"


def _http_endpoint(name: str, value: str, *, allow_path: bool) -> str:
    parsed = urlsplit(value)
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.hostname
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
        or (not allow_path and parsed.path not in {"", "/"})
        or any(character in value for character in "\r\n")
    ):
        raise ConfigError(f"{name} must be an HTTP(S) endpoint without embedded credentials")
    return value.rstrip("/")


def _host(value: str) -> str:
    host = value.strip()
    if not host or len(host) > 255 or any(character.isspace() for character in host):
        raise ConfigError(f"{ENV_HOST} must be one listen address")
    return host


def _headers(raw: str) -> dict[str, str]:
    if not raw.strip():
        return {}
    if len(raw.encode("utf-8")) > MAX_HEADERS_BYTES:
        raise ConfigError(f"{ENV_MODEL_HEADERS} must be at most {MAX_HEADERS_BYTES} bytes")
    try:
        decoded = json.loads(raw)
    except ValueError as error:
        raise ConfigError(f"{ENV_MODEL_HEADERS} must be a JSON object of string values") from error
    if not isinstance(decoded, dict) or len(decoded) > MAX_HEADERS:
        raise ConfigError(f"{ENV_MODEL_HEADERS} must be a JSON object of at most {MAX_HEADERS} entries")
    headers: dict[str, str] = {}
    for key, value in decoded.items():
        if not isinstance(key, str) or not _HEADER_NAME.fullmatch(key):
            raise ConfigError(f"{ENV_MODEL_HEADERS} has an invalid header name")
        if not isinstance(value, str) or any(character in value for character in "\r\n"):
            raise ConfigError(f"{ENV_MODEL_HEADERS} has a non-string or multi-line header value")
        headers[key] = value
    return headers


def _read_secret(path: Path, name: str) -> str:
    try:
        raw = path.read_bytes()
    except OSError as error:
        raise ConfigError(f"{name} names a file that cannot be read: {error.strerror}") from error
    if len(raw) > MAX_TOKEN_BYTES:
        raise ConfigError(f"the credential in {name} is larger than {MAX_TOKEN_BYTES} bytes")
    try:
        secret = raw.decode("utf-8").strip()
    except UnicodeDecodeError as error:
        raise ConfigError(f"the credential in {name} is not UTF-8") from error
    if not secret:
        raise ConfigError(f"the credential in {name} is empty")
    if any(character in secret for character in "\r\n"):
        raise ConfigError(f"the credential in {name} must be a single line")
    return secret


def _bounded_int(env: Mapping[str, str], name: str, default: int, low: int, high: int) -> int:
    raw = env.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as error:
        raise ConfigError(f"{name} must be an integer") from error
    if value < low or value > high:
        raise ConfigError(f"{name} must be between {low} and {high}")
    return value


def _bounded_float(env: Mapping[str, str], name: str, default: float, low: float, high: float) -> float:
    raw = env.get(name, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError as error:
        raise ConfigError(f"{name} must be a number") from error
    if value <= low or value > high:
        raise ConfigError(f"{name} must be greater than {low:g} and at most {high:g}")
    return value
