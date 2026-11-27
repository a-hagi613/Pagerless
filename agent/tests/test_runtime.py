"""The packaged entry point and configuration failure path."""

from __future__ import annotations

import logging
from pathlib import Path

import pytest

from pagerless_agent import __version__
from pagerless_agent.runtime import main
from tests.support import settings


def test_the_packaged_version_comes_from_metadata() -> None:
    assert __version__
    assert __version__ != "0.1.0"


def test_an_invalid_environment_fails_before_listening(
    caplog: pytest.LogCaptureFixture,
) -> None:
    with caplog.at_level(logging.ERROR):
        assert main({}) == 2
    assert "PAGERLESS_INTERNAL_TOKEN_FILE" in caplog.text


def test_a_valid_environment_builds_settings(tmp_path: Path) -> None:
    selected = settings(tmp_path)
    assert selected.port == 8080
    assert selected.callback_origin.endswith(":8080")
