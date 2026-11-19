"""The configuration of one agent Job, read from the environment the server sets.

`pagerless-server` owns the Job's environment, so the agent never reads a ConfigMap or a
Secret by name. Two values are the only base URLs the agent may contact — the investigation
API and the model endpoint — and nothing else in this package constructs an HTTP client.
The model API key is read from a file, never from an argument or a log line.
"""

from __future__ import annotations

import json
import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import urlsplit
from uuid import UUID

# The only two destinations an agent may reach (spec §2.6). Nothing else is a base URL.
BASE_URL_VARIABLES: tuple[str, str] = ("PAGERLESS_SERVER_URL", "PAGERLESS_MODEL_BASE_URL")

ENV_TASK = "PAGERLESS_TASK"
ENV_SERVER_URL = "PAGERLESS_SERVER_URL"
ENV_TOKEN_FILE = "PAGERLESS_TOKEN_FILE"
ENV_PAGE = "PAGERLESS_PAGE"
ENV_PAGE_UID = "PAGERLESS_PAGE_UID"
ENV_HINT = "PAGERLESS_HINT"
ENV_MODEL_BASE_URL = "PAGERLESS_MODEL_BASE_URL"
ENV_MODEL_NAME = "PAGERLESS_MODEL_NAME"
ENV_MODEL_API_KEY_FILE = "PAGERLESS_MODEL_API_KEY_FILE"
ENV_MODEL_HEADERS = "PAGERLESS_MODEL_HEADERS"
ENV_MODEL_MAX_OUTPUT_TOKENS = "PAGERLESS_MODEL_MAX_OUTPUT_TOKENS"
ENV_MODEL_CONTEXT_TOKENS = "PAGERLESS_MODEL_CONTEXT_TOKENS"
ENV_MODEL_STRUCTURED_OUTPUT = "PAGERLESS_MODEL_STRUCTURED_OUTPUT"
ENV_MODEL_MAX_TOKENS_FIELD = "PAGERLESS_MODEL_MAX_TOKENS_FIELD"
ENV_MODEL_SUPPORTS_TEMPERATURE = "PAGERLESS_MODEL_SUPPORTS_TEMPERATURE"
ENV_MODEL_CONNECT_TIMEOUT = "PAGERLESS_MODEL_CONNECT_TIMEOUT_S"
ENV_MODEL_READ_TIMEOUT = "PAGERLESS_MODEL_READ_TIMEOUT_S"
ENV_ALLOW_INSECURE_MODEL_ENDPOINT = "PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT"
ENV_BUDGET_TOOL_CALLS = "PAGERLESS_BUDGET_TOOL_CALLS"
ENV_BUDGET_MODEL_CALLS = "PAGERLESS_BUDGET_MODEL_CALLS"
ENV_BUDGET_INPUT_TOKENS = "PAGERLESS_BUDGET_INPUT_TOKENS"
ENV_BUDGET_SECONDS = "PAGERLESS_BUDGET_SECONDS"

TASK_INVESTIGATE = "investigate"
TASK_NARRATIVE = "narrative"
TASKS = (TASK_INVESTIGATE, TASK_NARRATIVE)

DEFAULT_MAX_OUTPUT_TOKENS = 4096
DEFAULT_CONTEXT_TOKENS = 32_768
DEFAULT_CONNECT_TIMEOUT_S = 10.0
DEFAULT_READ_TIMEOUT_S = 90.0
MAX_API_KEY_BYTES = 8 * 1024
MAX_HEADERS = 16
MAX_HEADERS_BYTES = 4 * 1024
_HEADER_NAME = re.compile(r"^[A-Za-z0-9-]{1,64}$")


class ConfigError(Exception):
    """The environment does not describe a valid Job. It never contains a secret value."""


@dataclass(frozen=True)
class ModelSettings:
    """Everything one OpenAI-compatible endpoint needs, and nothing provider-specific."""

    base_url: str
    api_key: str = field(repr=False)
    model: str
    headers: dict[str, str] = field(default_factory=dict)
    max_output_tokens: int = DEFAULT_MAX_OUTPUT_TOKENS
    context_tokens: int = DEFAULT_CONTEXT_TOKENS
    structured_output: str = "auto"
    max_tokens_field: str = "auto"
    supports_temperature: bool | None = None
    connect_timeout_s: float = DEFAULT_CONNECT_TIMEOUT_S
    read_timeout_s: float = DEFAULT_READ_TIMEOUT_S

    @property
    def cache_key(self) -> tuple[str, str]:
        """Return the identity under which the structured-output rung is cached."""
        return (self.base_url, self.model)


@dataclass(frozen=True)
class Settings:
    """One Job: where to report, which Page, and how to reach the model."""

    task: str
    server_url: str
    token_file: Path
    page: str
    page_uid: str
    model: ModelSettings
    hint: str | None = None
    tool_calls: int = 15
    model_calls: int = 12
    input_tokens: int = 300_000
    seconds: float = 300.0

    @property
    def is_investigation(self) -> bool:
        """Return whether this Job investigates a Page rather than writing its Narrative."""
        return self.task == TASK_INVESTIGATE

    def base_urls(self) -> tuple[str, str]:
        """Return the only two base URLs this Job may contact."""
        return (self.server_url, self.model.base_url)

    @classmethod
    def from_env(cls, env: Mapping[str, str]) -> Settings:
        """Read and validate the environment, naming every variable that is missing or invalid."""
        task = env.get(ENV_TASK, TASK_INVESTIGATE)
        if task not in TASKS:
            raise ConfigError(f"{ENV_TASK} must be investigate or narrative, not {task!r}")
        required = [ENV_SERVER_URL, ENV_TOKEN_FILE, ENV_PAGE, ENV_PAGE_UID]
        missing = [name for name in required if not env.get(name)]
        if missing:
            raise ConfigError(f"missing environment variables: {', '.join(missing)}")
        page_uid = env[ENV_PAGE_UID]
        _require_uuid(ENV_PAGE_UID, page_uid)
        page = env[ENV_PAGE]
        if len(page) > 253:
            raise ConfigError(f"{ENV_PAGE} must be at most 253 characters")
        server_url = _http_endpoint(ENV_SERVER_URL, env[ENV_SERVER_URL])
        if urlsplit(server_url).path not in {"", "/"}:
            raise ConfigError(f"{ENV_SERVER_URL} must be a base URL without a path")
        return cls(
            task=task,
            server_url=server_url,
            token_file=Path(env[ENV_TOKEN_FILE]),
            page=page,
            page_uid=page_uid,
            model=_model_settings(env),
            hint=env.get(ENV_HINT) or None,
            tool_calls=_positive_int(env, ENV_BUDGET_TOOL_CALLS, 15),
            model_calls=_positive_int(env, ENV_BUDGET_MODEL_CALLS, 12),
            input_tokens=_positive_int(env, ENV_BUDGET_INPUT_TOKENS, 300_000),
            seconds=_positive_float(env, ENV_BUDGET_SECONDS, 300.0),
        )


def _model_settings(env: Mapping[str, str]) -> ModelSettings:
    required = [ENV_MODEL_BASE_URL, ENV_MODEL_NAME, ENV_MODEL_API_KEY_FILE]
    missing = [name for name in required if not env.get(name)]
    if missing:
        raise ConfigError(f"missing environment variables: {', '.join(missing)}")
    base_url = _http_endpoint(ENV_MODEL_BASE_URL, env[ENV_MODEL_BASE_URL])
    if urlsplit(base_url).scheme == "http" and env.get(ENV_ALLOW_INSECURE_MODEL_ENDPOINT) != "1":
        raise ConfigError(
            f"{ENV_MODEL_BASE_URL} must be https unless {ENV_ALLOW_INSECURE_MODEL_ENDPOINT}=1 "
            "is set for an in-cluster endpoint"
        )
    model = env[ENV_MODEL_NAME].strip()
    if not model or len(model) > 255 or any(character in model for character in "\r\n"):
        raise ConfigError(f"{ENV_MODEL_NAME} must be one model identifier")
    structured = env.get(ENV_MODEL_STRUCTURED_OUTPUT, "auto")
    if structured not in {"auto", "json_schema", "json_object", "tool", "text"}:
        raise ConfigError(
            f"{ENV_MODEL_STRUCTURED_OUTPUT} must be auto, json_schema, json_object, tool, or text"
        )
    tokens_field = env.get(ENV_MODEL_MAX_TOKENS_FIELD, "auto")
    if tokens_field not in {"auto", "max_tokens", "max_completion_tokens"}:
        raise ConfigError(f"{ENV_MODEL_MAX_TOKENS_FIELD} must be auto, max_tokens, or max_completion_tokens")
    temperature = env.get(ENV_MODEL_SUPPORTS_TEMPERATURE, "auto")
    if temperature not in {"auto", "true", "false"}:
        raise ConfigError(f"{ENV_MODEL_SUPPORTS_TEMPERATURE} must be auto, true, or false")
    return ModelSettings(
        base_url=base_url,
        api_key=_read_secret(Path(env[ENV_MODEL_API_KEY_FILE]), ENV_MODEL_API_KEY_FILE),
        model=model,
        headers=_headers(env.get(ENV_MODEL_HEADERS, "")),
        max_output_tokens=_positive_int(env, ENV_MODEL_MAX_OUTPUT_TOKENS, DEFAULT_MAX_OUTPUT_TOKENS),
        context_tokens=_positive_int(env, ENV_MODEL_CONTEXT_TOKENS, DEFAULT_CONTEXT_TOKENS),
        structured_output=structured,
        max_tokens_field=tokens_field,
        supports_temperature=None if temperature == "auto" else temperature == "true",
        connect_timeout_s=_positive_float(env, ENV_MODEL_CONNECT_TIMEOUT, DEFAULT_CONNECT_TIMEOUT_S),
        read_timeout_s=_positive_float(env, ENV_MODEL_READ_TIMEOUT, DEFAULT_READ_TIMEOUT_S),
    )


def _http_endpoint(name: str, value: str) -> str:
    parsed = urlsplit(value)
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.hostname
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
        or "\n" in value
        or "\r" in value
    ):
        raise ConfigError(f"{name} must be an HTTP(S) endpoint without embedded credentials")
    return value.rstrip("/")


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
    if len(raw) > MAX_API_KEY_BYTES:
        raise ConfigError(f"the key in {name} is larger than {MAX_API_KEY_BYTES} bytes")
    try:
        secret = raw.decode("utf-8").strip()
    except UnicodeDecodeError as error:
        raise ConfigError(f"the key in {name} is not UTF-8") from error
    if not secret:
        raise ConfigError(f"the key in {name} is empty")
    if any(character in secret for character in "\r\n"):
        raise ConfigError(f"the key in {name} must be a single line")
    return secret


def _require_uuid(name: str, value: str) -> None:
    try:
        UUID(value)
    except ValueError as error:
        raise ConfigError(f"{name} must be a UUID") from error


def _positive_int(env: Mapping[str, str], name: str, default: int) -> int:
    raw = env.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as error:
        raise ConfigError(f"{name} must be an integer") from error
    if value <= 0:
        raise ConfigError(f"{name} must be greater than zero")
    return value


def _positive_float(env: Mapping[str, str], name: str, default: float) -> float:
    raw = env.get(name, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError as error:
        raise ConfigError(f"{name} must be a number") from error
    if value <= 0:
        raise ConfigError(f"{name} must be greater than zero")
    return value
