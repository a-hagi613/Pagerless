"""Budget exhaustion keeps the Findings and ends the Page as BudgetExhausted."""

from __future__ import annotations

import asyncio
from pathlib import Path
from uuid import uuid4

from pagerless_agent.budget import Budget
from pagerless_agent.config import Settings
from pagerless_agent.model import ModelRequest, ModelResponse, Usage
from pagerless_agent.runtime import run
from tests.fakes import ScriptedProvider, StandInBroker
from tests.mock_openai import MockOpenAI, Step
from tests.support import (
    DIAGNOSIS,
    PREAMBLE,
    PROPOSAL,
    investigate,
    manifest,
    script,
    settings,
    tool_client,
)

TOOL_CALL, FINDING, DONE = PREAMBLE


def pair(tmp_path: Path, *, limits: dict[str, float] | None = None) -> tuple[Settings, StandInBroker]:
    """Return settings and a broker whose manifest carries the given limits."""
    uid = str(uuid4())
    return settings(tmp_path, uid=uid), StandInBroker(manifest(uid=uid, limits=limits))


async def test_exhausting_the_tool_calls_keeps_the_findings_gathered_so_far(tmp_path: Path) -> None:
    selected, broker = pair(
        tmp_path,
        limits={"toolCalls": 1, "modelCalls": 12, "inputTokens": 300000, "seconds": 300},
    )
    second_call = Step(
        tool_calls=(
            {
                "name": "pods_logs",
                "arguments": '{"namespace": "shop", "pod": "checkout-6d9f8c7b5-2xk4p", "previous": false}',
            },
        )
    )
    mock = script(investigate_steps=[TOOL_CALL, FINDING, second_call, DONE])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "budget_exhausted"
    assert outcome.exit_code == 2
    assert "tool calls" in outcome.detail
    assert broker.tool_calls == 1
    assert any(event["kind"] == "Finding" for event in broker.events)
    assert broker.reports == []


async def test_exhausting_the_model_calls_stops_the_run(tmp_path: Path) -> None:
    selected, broker = pair(
        tmp_path, limits={"toolCalls": 15, "modelCalls": 2, "inputTokens": 300000, "seconds": 300}
    )

    outcome = await investigate(selected, script(), broker)

    assert outcome.kind == "budget_exhausted"
    assert "model calls" in outcome.detail
    assert outcome.exit_code == 2


async def test_exhausting_the_input_tokens_stops_before_the_first_call(tmp_path: Path) -> None:
    selected, broker = pair(
        tmp_path, limits={"toolCalls": 15, "modelCalls": 12, "inputTokens": 10, "seconds": 300}
    )
    mock = script()

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "budget_exhausted"
    assert "input tokens" in outcome.detail
    assert mock.served == 0


class SlowProvider(ScriptedProvider):
    """A provider whose answer arrives after the time budget is spent."""

    def __init__(self, delay: float) -> None:
        super().__init__([])
        self.delay = delay

    async def complete(self, request: ModelRequest, *, budget: Budget | None = None) -> ModelResponse:
        await asyncio.sleep(self.delay)
        return ModelResponse(usage=Usage(input_tokens=1, output_tokens=1))


async def test_exhausting_the_wall_clock_stops_the_run(tmp_path: Path) -> None:
    uid = str(uuid4())
    selected = settings(tmp_path, uid=uid)
    broker = StandInBroker(
        manifest(uid=uid, limits={"toolCalls": 15, "modelCalls": 12, "inputTokens": 300000, "seconds": 0.05})
    )

    outcome = await run(selected, provider=SlowProvider(0.5), client=tool_client(selected, broker))

    assert outcome.kind == "budget_exhausted"
    assert "time budget" in outcome.detail
    assert outcome.exit_code == 2


async def test_a_provider_that_omits_usage_cannot_hide_its_spend(tmp_path: Path) -> None:
    selected, broker = pair(tmp_path)
    steps = [
        Step(content=TOOL_CALL.content, tool_calls=TOOL_CALL.tool_calls, usage=None),
        Step(content=FINDING.content, tool_calls=FINDING.tool_calls, usage=None),
        Step(content=DONE.content, usage=None),
        Step(payload=DIAGNOSIS, usage=None),
        Step(payload=PROPOSAL, usage=None),
    ]

    outcome = await investigate(selected, MockOpenAI(steps), broker)

    assert outcome.kind == "reported"
    usage = broker.reports[0]["usage"]
    assert usage["estimated"] is True
    assert usage["inputTokens"] > 0
    assert usage["outputTokens"] > 0
    assert broker.reports[0]["capabilities"]["usageSource"] == "estimated"


async def test_mixed_usage_is_reported_as_mixed(tmp_path: Path) -> None:
    selected, broker = pair(tmp_path)
    steps = [
        Step(content=TOOL_CALL.content, tool_calls=TOOL_CALL.tool_calls),
        Step(content=FINDING.content, tool_calls=FINDING.tool_calls, usage=None),
        Step(content=DONE.content),
        Step(payload=DIAGNOSIS),
        Step(payload=PROPOSAL),
    ]

    outcome = await investigate(selected, MockOpenAI(steps), broker)

    assert outcome.kind == "reported"
    assert broker.reports[0]["capabilities"]["usageSource"] == "mixed"
    assert broker.reports[0]["usage"]["estimated"] is True
