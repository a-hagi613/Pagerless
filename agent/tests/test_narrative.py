"""The Narrative Job: one model call, no tools, and a posted Narrative."""

from __future__ import annotations

from pathlib import Path
from uuid import uuid4

from pagerless_agent.config import Settings
from pagerless_agent.model import ModelProvider
from pagerless_agent.narrative import run_narrative
from pagerless_agent.toolclient import ToolClient
from tests.fakes import StandInBroker
from tests.mock_openai import MockOpenAI, Step
from tests.support import manifest, provider, settings, tool_client


def narrative_agent(
    tmp_path: Path, steps: list[Step]
) -> tuple[Settings, ModelProvider, ToolClient, StandInBroker, MockOpenAI]:
    """Return the settings, provider, and client for one Narrative Job."""
    uid = str(uuid4())
    selected = settings(tmp_path, uid=uid, task="narrative")
    broker = StandInBroker(manifest(uid=uid))
    mock = MockOpenAI(steps)
    return selected, provider(selected, mock), tool_client(selected, broker), broker, mock


async def test_the_narrative_is_one_plain_call_with_no_tools(tmp_path: Path) -> None:
    selected, mock_provider, client, broker, mock = narrative_agent(
        tmp_path, [Step(content="The checkout Deployment was OOMKilled and a limit increase was proposed.")]
    )

    outcome = await run_narrative(selected, provider=mock_provider, client=client)

    assert outcome.kind == "written"
    assert outcome.exit_code == 0
    assert broker.narratives == [
        {"narrative": "The checkout Deployment was OOMKilled and a limit increase was proposed."}
    ]
    request = mock.requests[0]
    assert "tools" not in request
    assert "response_format" not in request
    assert request["messages"][0]["role"] == "system"
    assert "<tool_output nonce=" in request["messages"][1]["content"]
    assert selected.model.max_output_tokens >= 2000


async def test_an_empty_narrative_is_a_failure_and_is_not_posted(tmp_path: Path) -> None:
    selected, mock_provider, client, broker, _ = narrative_agent(tmp_path, [Step(content="   ")])

    outcome = await run_narrative(selected, provider=mock_provider, client=client)

    assert outcome.kind == "failed"
    assert outcome.exit_code == 1
    assert broker.narratives == []
