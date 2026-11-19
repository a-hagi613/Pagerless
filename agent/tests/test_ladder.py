"""An investigation completes for every structured-output profile, offline."""

from __future__ import annotations

from pathlib import Path
from uuid import uuid4

import pytest

from pagerless_agent.structured import Rung
from tests.fakes import StandInBroker
from tests.mock_openai import MockOpenAI, Profile
from tests.support import fixture, investigate, manifest, settings


@pytest.mark.parametrize(
    ("profile", "rung"),
    [
        ("strict", Rung.JSON_SCHEMA),
        ("json_object", Rung.JSON_OBJECT),
        ("tool", Rung.TOOL),
        ("tool_without_choice", Rung.TOOL),
        ("text", Rung.TEXT),
    ],
)
async def test_a_complete_investigation_for_each_ladder_profile(
    tmp_path: Path, profile: Profile, rung: Rung
) -> None:
    uid = str(uuid4())
    # No configuration beyond the endpoint: the profile is the provider's, not a setting.
    selected = settings(tmp_path, uid=uid)
    broker = StandInBroker(manifest(uid=uid))
    mock = MockOpenAI.from_fixture(fixture("scripts", "oom.json"), profile=profile)

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "reported"
    assert outcome.phase == "Proposed"
    assert broker.tool_calls == 1
    assert broker.reports[0]["proposal"]["kind"] == "FilePatch"
    capabilities = broker.reports[0]["capabilities"]["structuredOutput"]
    assert capabilities == {"diagnose": rung.value, "propose": rung.value}
    assert len(mock.requests) >= 5


async def test_a_strict_provider_is_asked_for_json_schema_first(tmp_path: Path) -> None:
    uid = str(uuid4())
    selected = settings(tmp_path, uid=uid)
    broker = StandInBroker(manifest(uid=uid))
    mock = MockOpenAI.from_fixture(fixture("scripts", "oom.json"), profile="strict")

    await investigate(selected, mock, broker)

    formats = [request.get("response_format") for request in mock.requests]
    assert formats[0] is None  # the investigate stage asks for tools, not JSON
    first = formats[3]
    assert isinstance(first, dict)
    assert first["type"] == "json_schema"
    assert first["json_schema"]["strict"] is True
    assert first["json_schema"]["schema"]["additionalProperties"] is False


async def test_a_json_mode_provider_never_sees_a_json_schema(tmp_path: Path) -> None:
    uid = str(uuid4())
    selected = settings(tmp_path, uid=uid)
    broker = StandInBroker(manifest(uid=uid))
    mock = MockOpenAI.from_fixture(fixture("scripts", "oom.json"), profile="json_object")

    await investigate(selected, mock, broker)

    # Both structured stages succeeded through JSON mode, after one downgrade each.
    for request in mock.answered[-2:]:
        assert request["response_format"]["type"] == "json_object"
    assert sum(1 for rejection in mock.rejections if "json_schema" in rejection) == 2
    # The schema travels in the prompt instead.
    assert "JSON Schema" in mock.answered[-2]["messages"][-1]["content"]


async def test_a_text_only_provider_receives_no_response_format_and_no_tools_for_output(
    tmp_path: Path,
) -> None:
    uid = str(uuid4())
    selected = settings(tmp_path, uid=uid)
    broker = StandInBroker(manifest(uid=uid))
    mock = MockOpenAI.from_fixture(fixture("scripts", "oom.json"), profile="text")

    await investigate(selected, mock, broker)

    # The text rung is reached after `response_format` and a forced `tool_choice` are refused.
    for request in (mock.answered[-3], mock.answered[-1]):
        assert "response_format" not in request
        assert "tools" not in request
    assert "Respond with exactly one JSON object" in mock.answered[-3]["messages"][-1]["content"]
    assert any("tool_choice" in rejection for rejection in mock.rejections)
