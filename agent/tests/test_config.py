"""Environment validation for the private service."""

from __future__ import annotations

from dataclasses import replace
from pathlib import Path

import pytest

from pagerless_agent.config import (
    ENV_CALLBACK_ORIGIN,
    ENV_INTERNAL_TOKEN_FILE,
    ENV_MAX_CONCURRENCY,
    ENV_MODEL_API_KEY_FILE,
    ENV_MODEL_BASE_URL,
    ENV_MODEL_HEADERS,
    ENV_MODEL_NAME,
    ConfigError,
    ServiceSettings,
)
from tests.fakes import API_KEY, CALLBACK_ORIGIN, TOKEN
from tests.support import settings


def test_valid_settings_are_read(tmp_path: Path) -> None:
    selected = settings(tmp_path)
    assert selected.token == TOKEN
    assert selected.callback_origin == CALLBACK_ORIGIN
    assert selected.model.api_key == API_KEY
    assert selected.model.base_url == "http://mock-openai.pagerless.svc/v1"
    assert selected.model.model == "mock-model"
    assert selected.max_concurrency == 4
    assert selected.port == 8080
    assert selected.budgets.tool_calls == 6


def test_missing_variables_are_named_together(tmp_path: Path) -> None:
    with pytest.raises(ConfigError) as caught:
        ServiceSettings.from_env({})
    message = str(caught.value)
    for name in (
        ENV_INTERNAL_TOKEN_FILE,
        ENV_CALLBACK_ORIGIN,
        ENV_MODEL_BASE_URL,
        ENV_MODEL_NAME,
        ENV_MODEL_API_KEY_FILE,
    ):
        assert name in message


def test_secrets_are_not_in_repr(tmp_path: Path) -> None:
    selected = settings(tmp_path)
    assert TOKEN not in repr(selected)
    assert API_KEY not in repr(selected)
    assert API_KEY not in repr(selected.model)


def test_empty_or_unreadable_credential_is_refused(tmp_path: Path) -> None:
    token_file = tmp_path / "token"
    token_file.write_text("", encoding="utf-8")
    selected = settings(tmp_path)
    env = {
        ENV_INTERNAL_TOKEN_FILE: str(token_file),
        ENV_CALLBACK_ORIGIN: CALLBACK_ORIGIN,
        ENV_MODEL_BASE_URL: "https://models.example/v1",
        ENV_MODEL_NAME: "m",
        ENV_MODEL_API_KEY_FILE: str(tmp_path / "key"),
    }
    (tmp_path / "key").write_text("k", encoding="utf-8")
    with pytest.raises(ConfigError, match="empty"):
        ServiceSettings.from_env(env)
    with pytest.raises(ConfigError, match="cannot be read"):
        ServiceSettings.from_env({**env, ENV_INTERNAL_TOKEN_FILE: str(tmp_path / "missing")})
    assert selected.token == TOKEN


@pytest.mark.parametrize(
    "origin",
    [
        "http://server/path",
        "http://user:pass@server",
        "http://server?query=1",
        "http://server#fragment",
        "ftp://server",
    ],
)
def test_callback_origin_must_be_a_bare_http_origin(tmp_path: Path, origin: str) -> None:
    with pytest.raises(ConfigError, match="origin"):
        settings(tmp_path, PAGERLESS_CALLBACK_ORIGIN=origin)


def test_callback_origin_trailing_slash_is_normalized(tmp_path: Path) -> None:
    selected = settings(tmp_path, PAGERLESS_CALLBACK_ORIGIN=CALLBACK_ORIGIN + "/")
    assert selected.callback_origin == CALLBACK_ORIGIN


def test_insecure_model_endpoint_needs_an_explicit_override(tmp_path: Path) -> None:
    with pytest.raises(ConfigError, match="must be https"):
        settings(
            tmp_path,
            PAGERLESS_MODEL_BASE_URL="http://models.example/v1",
            PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT="0",
        )
    selected = settings(
        tmp_path,
        PAGERLESS_MODEL_BASE_URL="https://models.example/v1",
        PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT="0",
    )
    assert selected.model.base_url == "https://models.example/v1"


@pytest.mark.parametrize("value", ["0", "65", "nope"])
def test_concurrency_is_bounded(tmp_path: Path, value: str) -> None:
    with pytest.raises(ConfigError, match=ENV_MAX_CONCURRENCY):
        settings(tmp_path, **{ENV_MAX_CONCURRENCY: value})


def test_headers_are_bounded_json(tmp_path: Path) -> None:
    selected = settings(tmp_path, **{ENV_MODEL_HEADERS: '{"x-tenant": "a"}'})
    assert selected.model.headers == {"x-tenant": "a"}
    with pytest.raises(ConfigError, match="header name"):
        settings(tmp_path, **{ENV_MODEL_HEADERS: '{"bad header": "a"}'})
    with pytest.raises(ConfigError, match="JSON object"):
        settings(tmp_path, **{ENV_MODEL_HEADERS: "[]"})


def test_multiline_credential_is_refused(tmp_path: Path) -> None:
    token_file = tmp_path / "token"
    token_file.write_text("one\ntwo\n", encoding="utf-8")
    selected = settings(tmp_path)
    env = {
        ENV_INTERNAL_TOKEN_FILE: str(token_file),
        ENV_CALLBACK_ORIGIN: CALLBACK_ORIGIN,
        ENV_MODEL_BASE_URL: "https://models.example/v1",
        ENV_MODEL_NAME: "m",
        ENV_MODEL_API_KEY_FILE: str(tmp_path / "key"),
    }
    (tmp_path / "key").write_text("k", encoding="utf-8")
    with pytest.raises(ConfigError, match="single line"):
        ServiceSettings.from_env(env)
    assert selected.token == TOKEN


def test_replace_keeps_the_rest_of_the_settings(tmp_path: Path) -> None:
    selected = settings(tmp_path)
    changed = replace(selected, max_concurrency=1)
    assert changed.max_concurrency == 1
    assert changed.token == selected.token
