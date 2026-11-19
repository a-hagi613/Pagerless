"""Configuration validation, and the proof that there are exactly two base URLs."""

from __future__ import annotations

from pathlib import Path
from uuid import uuid4

import pytest

from pagerless_agent.config import BASE_URL_VARIABLES, ConfigError, Settings

PAGE_UID = "0b3f4a2c-1111-2222-3333-444455556666"


def env(tmp_path: Path, **overrides: str) -> dict[str, str]:
    """Return a valid environment, with the overrides applied."""
    token = tmp_path / "token"
    token.write_text("projected-token\n", encoding="utf-8")
    key = tmp_path / "key"
    key.write_text("sk-test-secret\n", encoding="utf-8")
    values = {
        "PAGERLESS_SERVER_URL": "https://pagerless-server.pagerless.svc:8443",
        "PAGERLESS_TOKEN_FILE": str(token),
        "PAGERLESS_PAGE": "checkout-7f2c",
        "PAGERLESS_PAGE_UID": PAGE_UID,
        "PAGERLESS_MODEL_BASE_URL": "https://api.deepseek.com/v1",
        "PAGERLESS_MODEL_NAME": "deepseek-chat",
        "PAGERLESS_MODEL_API_KEY_FILE": str(key),
    }
    values.update(overrides)
    return values


def test_a_valid_environment_yields_exactly_two_base_urls(tmp_path: Path) -> None:
    settings = Settings.from_env(env(tmp_path))

    assert settings.base_urls() == (
        "https://pagerless-server.pagerless.svc:8443",
        "https://api.deepseek.com/v1",
    )
    assert len(settings.base_urls()) == 2
    assert BASE_URL_VARIABLES == ("PAGERLESS_SERVER_URL", "PAGERLESS_MODEL_BASE_URL")
    assert settings.model.api_key == "sk-test-secret"
    assert settings.model.model == "deepseek-chat"
    assert (settings.tool_calls, settings.model_calls, settings.input_tokens, settings.seconds) == (
        15,
        12,
        300_000,
        300,
    )


def test_the_api_key_never_appears_in_the_settings_representation(tmp_path: Path) -> None:
    settings = Settings.from_env(env(tmp_path))

    assert "sk-test-secret" not in repr(settings)
    assert "sk-test-secret" not in repr(settings.model)


def test_no_other_environment_variable_can_introduce_a_third_base_url(tmp_path: Path) -> None:
    settings = Settings.from_env(
        env(
            tmp_path,
            PAGERLESS_KUBERNETES_URL="https://kubernetes.default.svc",
            PAGERLESS_GITHUB_URL="https://api.github.com",
            PAGERLESS_CONNECTOR_LOKI_URL="https://loki",
        )
    )

    assert len(settings.base_urls()) == 2


@pytest.mark.parametrize(
    "overrides",
    [
        {"PAGERLESS_SERVER_URL": ""},
        {"PAGERLESS_TOKEN_FILE": ""},
        {"PAGERLESS_PAGE": ""},
        {"PAGERLESS_PAGE_UID": ""},
        {"PAGERLESS_MODEL_BASE_URL": ""},
        {"PAGERLESS_MODEL_NAME": ""},
        {"PAGERLESS_MODEL_API_KEY_FILE": ""},
    ],
)
def test_a_missing_required_variable_is_named(tmp_path: Path, overrides: dict[str, str]) -> None:
    with pytest.raises(ConfigError, match="missing environment variables"):
        Settings.from_env(env(tmp_path, **overrides))


@pytest.mark.parametrize(
    "url",
    [
        "pagerless-server:8443",
        "ftp://pagerless-server",
        "https://user:password@pagerless-server",
        "https://pagerless-server?x=1",
        "https://pagerless-server#fragment",
        "https://pagerless-server:8443/api",
    ],
)
def test_an_invalid_server_url_is_refused(tmp_path: Path, url: str) -> None:
    with pytest.raises(ConfigError):
        Settings.from_env(env(tmp_path, PAGERLESS_SERVER_URL=url))


def test_plain_http_to_a_model_requires_the_in_cluster_opt_in(tmp_path: Path) -> None:
    with pytest.raises(ConfigError, match="ALLOW_INSECURE"):
        Settings.from_env(env(tmp_path, PAGERLESS_MODEL_BASE_URL="http://vllm.pagerless.svc:8000/v1"))

    settings = Settings.from_env(
        env(
            tmp_path,
            PAGERLESS_MODEL_BASE_URL="http://vllm.pagerless.svc:8000/v1",
            PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT="1",
        )
    )
    assert settings.model.base_url == "http://vllm.pagerless.svc:8000/v1"


def test_the_page_uid_must_be_a_uuid(tmp_path: Path) -> None:
    with pytest.raises(ConfigError, match="PAGERLESS_PAGE_UID must be a UUID"):
        Settings.from_env(env(tmp_path, PAGERLESS_PAGE_UID="not-a-uuid"))


def test_the_task_must_be_known(tmp_path: Path) -> None:
    with pytest.raises(ConfigError, match="investigate or narrative"):
        Settings.from_env(env(tmp_path, PAGERLESS_TASK="benchmark"))


def test_model_headers_must_be_bounded_json_without_line_breaks(tmp_path: Path) -> None:
    valid = Settings.from_env(env(tmp_path, PAGERLESS_MODEL_HEADERS='{"HTTP-Referer": "https://acme.test"}'))
    assert valid.model.headers == {"HTTP-Referer": "https://acme.test"}

    for raw in ("not json", "[]", '{"X-Bad": 1}', '{"X-Bad": "a\\nb"}'):
        with pytest.raises(ConfigError, match="PAGERLESS_MODEL_HEADERS"):
            Settings.from_env(env(tmp_path, PAGERLESS_MODEL_HEADERS=raw))

    too_many = "{" + ", ".join(f'"X-{index}": "v"' for index in range(17)) + "}"
    with pytest.raises(ConfigError, match="at most 16 entries"):
        Settings.from_env(env(tmp_path, PAGERLESS_MODEL_HEADERS=too_many))


def test_the_api_key_file_must_be_a_small_single_line(tmp_path: Path) -> None:
    empty = tmp_path / "empty"
    empty.write_text("\n", encoding="utf-8")
    multiline = tmp_path / "multiline"
    multiline.write_text("first\nsecond\n", encoding="utf-8")
    huge = tmp_path / "huge"
    huge.write_text("k" * 9000, encoding="utf-8")

    for path in (empty, multiline, huge, tmp_path / "absent"):
        with pytest.raises(ConfigError, match="PAGERLESS_MODEL_API_KEY_FILE"):
            Settings.from_env(env(tmp_path, PAGERLESS_MODEL_API_KEY_FILE=str(path)))


@pytest.mark.parametrize(
    ("name", "value"),
    [
        ("PAGERLESS_MODEL_STRUCTURED_OUTPUT", "magic"),
        ("PAGERLESS_MODEL_MAX_TOKENS_FIELD", "tokens"),
        ("PAGERLESS_MODEL_SUPPORTS_TEMPERATURE", "maybe"),
        ("PAGERLESS_BUDGET_TOOL_CALLS", "0"),
        ("PAGERLESS_BUDGET_MODEL_CALLS", "many"),
        ("PAGERLESS_BUDGET_SECONDS", "-1"),
    ],
)
def test_an_invalid_optional_setting_is_refused(tmp_path: Path, name: str, value: str) -> None:
    with pytest.raises(ConfigError, match=name):
        Settings.from_env(env(tmp_path, **{name: value}))


def test_the_dialect_switches_are_read(tmp_path: Path) -> None:
    settings = Settings.from_env(
        env(
            tmp_path,
            PAGERLESS_MODEL_MAX_TOKENS_FIELD="max_completion_tokens",
            PAGERLESS_MODEL_SUPPORTS_TEMPERATURE="false",
            PAGERLESS_MODEL_STRUCTURED_OUTPUT="json_object",
            PAGERLESS_MODEL_CONTEXT_TOKENS="8192",
            PAGERLESS_PAGE_UID=str(uuid4()),
        )
    )

    assert settings.model.max_tokens_field == "max_completion_tokens"
    assert settings.model.supports_temperature is False
    assert settings.model.structured_output == "json_object"
    assert settings.model.context_tokens == 8192
