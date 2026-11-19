"""The graph end to end against the mock provider and the stand-in broker."""

from __future__ import annotations

import json
from pathlib import Path
from uuid import uuid4

import httpx
import pytest

from pagerless_agent.agent import Agent, Outcome
from pagerless_agent.config import Settings
from pagerless_agent.model import Message
from tests.fakes import StandInBroker
from tests.mock_openai import Step
from tests.support import (
    API_KEY,
    DIAGNOSIS,
    PREAMBLE,
    PROPOSAL,
    TOKEN,
    corrupted,
    investigate,
    manifest,
    provider,
    script,
    settings,
    tool_client,
)


def page(tmp_path: Path) -> tuple[Settings, StandInBroker]:
    """Return mutually consistent settings and a stand-in broker for one Page."""
    uid = str(uuid4())
    return settings(tmp_path, uid=uid), StandInBroker(manifest(uid=uid))


async def test_a_complete_investigation_reports_a_cited_diagnosis_and_one_proposal(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)

    outcome = await investigate(selected, script(), broker)

    assert outcome.kind == "reported"
    assert outcome.phase == "Proposed"
    assert outcome.exit_code == 0
    report = broker.reports[0]
    assert report["page"]["uid"] == selected.page_uid
    assert report["findings"][0]["toolCallId"] == "srv-1"
    assert report["findings"][0]["pointer"] == "/data/lines/0"
    assert report["proposal"]["kind"] == "FilePatch"
    assert report["proposal"]["files"][0]["path"] == "apps/shop/checkout.yaml"
    assert report["proposal"]["files"][0]["updatedContent"].endswith("\n")
    assert report["usage"]["modelCalls"] == 5
    assert report["usage"]["estimated"] is False
    assert report["capabilities"]["structuredOutput"] == {
        "diagnose": "json_schema",
        "propose": "json_schema",
    }
    assert report["capabilities"]["usageSource"] == "provider"

    kinds = [event["kind"] for event in broker.events]
    assert "ToolCall" in kinds
    assert "Finding" in kinds
    assert kinds.count("Progress") >= 2


async def test_every_request_is_attributed_and_idempotent(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)

    await investigate(selected, script(), broker)

    tool_requests = [request for request in broker.requests if "/tools/" in request.url.path]
    assert len(tool_requests) == 1
    request = tool_requests[0]
    uid = selected.page_uid
    assert request.headers["Authorization"] == f"Bearer {TOKEN}"
    assert request.headers["X-Pagerless-Page-Uid"] == uid
    assert request.headers["X-Page-UID"] == uid
    assert request.headers["Idempotency-Key"] == f"{uid}/call_1_0"
    assert broker.missing_idempotency == 0
    prefix = "https://pagerless-server.pagerless.svc:8443/v1/pages/"
    assert all(str(request.url).startswith(prefix) for request in broker.requests)


async def test_no_credential_reaches_the_report_or_a_model_request(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    mock = script()

    await investigate(selected, mock, broker)

    rendered = json.dumps(broker.reports[0])
    assert API_KEY not in rendered
    assert TOKEN not in rendered
    for body in mock.requests:
        assert API_KEY not in json.dumps(body)


async def test_the_model_gets_one_system_message_and_the_bundle_inside_an_envelope(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    mock = script()

    await investigate(selected, mock, broker)

    first = mock.requests[0]["messages"]
    roles = [message["role"] for message in first]
    assert roles[0] == "system"
    assert roles.count("system") == 1
    task = first[1]["content"]
    assert "<tool_output nonce=" in task
    assert "Context Bundle" in task
    assert "The engineer who opened this Page gave this Hint" in task
    assert "PAGERLESS_" not in task


async def test_a_fabricated_citation_earns_exactly_one_repair(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    fabricated = corrupted(DIAGNOSIS, "/findings/0/toolCallId", "srv-999")
    mock = script(diagnosis_steps=[Step(payload=fabricated), Step(payload=DIAGNOSIS)])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "reported"
    assert mock.served == 6
    assert broker.reports[0]["findings"][0]["toolCallId"] == "srv-1"
    repair = mock.requests[4]["messages"][-1]["content"]
    assert "findings[0].toolCallId" in repair


async def test_a_second_fabricated_citation_fails_the_run(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    fabricated = corrupted(DIAGNOSIS, "/findings/0/toolCallId", "srv-999")
    mock = script(diagnosis_steps=[Step(payload=fabricated), Step(payload=fabricated)])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "failed"
    assert outcome.exit_code == 1
    assert "Diagnosis" in outcome.detail
    assert broker.reports == []


async def test_a_proposal_outside_the_path_policy_earns_one_repair(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    forbidden = corrupted(PROPOSAL, "/files/0/path", ".github/workflows/ci.yaml")
    mock = script(proposal_steps=[Step(payload=forbidden), Step(payload=PROPOSAL)])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "reported"
    assert broker.reports[0]["proposal"]["files"][0]["path"] == "apps/shop/checkout.yaml"


async def test_a_proposal_that_stays_invalid_is_refused(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    forbidden = corrupted(PROPOSAL, "/files", [])
    mock = script(proposal_steps=[Step(payload=forbidden), Step(payload=forbidden)])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "proposal_refused"
    assert outcome.exit_code == 3
    assert broker.reports == []


async def test_a_server_422_earns_one_revision(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    broker.refuse_report(("proposal.files[0].path", "the file moved since the resolution"))
    mock = script(proposal_steps=[Step(payload=PROPOSAL), Step(payload=PROPOSAL)])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "reported"
    assert len(broker.reports) == 1
    revision = mock.requests[-1]["messages"][-1]["content"]
    assert "refused the Proposal" in revision
    assert "the file moved since the resolution" in revision


async def test_a_second_422_gives_up_without_a_third_report(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    broker.refuse_report(("proposal.files", "nope"))
    broker.refuse_report(("proposal.files", "still nope"))
    mock = script(proposal_steps=[Step(payload=PROPOSAL), Step(payload=PROPOSAL)])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "proposal_refused"
    assert outcome.exit_code == 3
    assert broker.report_answers == []


async def test_a_page_that_is_no_longer_investigating_is_rejected(tmp_path: Path) -> None:
    class Refusing(StandInBroker):
        def handler(self, request: httpx.Request) -> httpx.Response:
            self.requests.append(request)
            return httpx.Response(409, json={"message": "the Page is terminal"})

    uid = str(uuid4())
    selected = settings(tmp_path, uid=uid)
    broker = Refusing(manifest(uid=uid))

    outcome = await investigate(selected, script(), broker)

    assert outcome.kind == "rejected"
    assert outcome.exit_code == 4


async def test_a_malformed_tool_call_is_a_model_visible_error_and_the_run_continues(tmp_path: Path) -> None:
    selected, broker = page(tmp_path)
    broken = Step(tool_calls=({"name": "pods_logs", "arguments": '{"namespace": '},))
    fenced = Step(
        tool_calls=(
            {
                "name": "pods_logs",
                "arguments": '```json\n{"namespace": "shop", "pod": "checkout-6d9f8c7b5-2xk4p"}\n```',
            },
        )
    )
    mock = script(investigate_steps=[*PREAMBLE[:1], broken, fenced, *PREAMBLE[1:]])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "reported"
    tool_messages = [message for message in mock.requests[-1]["messages"] if message["role"] == "tool"]
    assert any('"invalid_arguments"' in message["content"] for message in tool_messages)
    assert broker.tool_calls == 2


async def test_a_message_cannot_carry_a_private_reasoning_field() -> None:
    try:
        Message(role="assistant", content="x", reasoning_content="chain of thought")  # type: ignore[call-arg]
    except Exception as error:
        assert "reasoning_content" in str(error)
    else:  # pragma: no cover - the model forbids the field
        raise AssertionError("Message accepted a provider-private field")


async def test_an_agent_can_be_constructed_directly_with_a_fixed_nonce(tmp_path: Path) -> None:
    uid = str(uuid4())
    selected = settings(tmp_path, uid=uid)
    broker = StandInBroker(manifest(uid=uid))
    mock = script()
    agent = Agent(
        settings=selected,
        provider=provider(selected, mock),
        client=tool_client(selected, broker),
        nonce="0123456789abcdef0123456789abcdef",
    )

    outcome: Outcome = await agent.run()

    assert outcome.kind == "reported"
    assert "0123456789abcdef0123456789abcdef" in mock.requests[0]["messages"][1]["content"]


@pytest.mark.parametrize(
    "arguments",
    [
        "",
        '"{\\"namespace\\": \\"shop\\", \\"pod\\": \\"p\\"}"',
        '```json\n{"namespace": "shop", "pod": "p"}\n```',
        '{"namespace": "shop", "pod": "p",}',
        "[1, 2]",
        "42",
    ],
)
async def test_every_malformed_argument_form_continues_the_run(tmp_path: Path, arguments: str) -> None:
    selected, broker = page(tmp_path)
    broken = Step(tool_calls=({"name": "pods_logs", "arguments": arguments},))
    # A model that repairs its own call on the next turn, as the tool error invites.
    repaired = Step(
        tool_calls=(
            {
                "name": "pods_logs",
                "arguments": '{"namespace": "shop", "pod": "checkout-6d9f8c7b5-2xk4p"}',
            },
        )
    )
    mock = script(investigate_steps=[broken, repaired, *PREAMBLE[1:]])

    outcome = await investigate(selected, mock, broker)

    assert outcome.kind == "reported"
    assert broker.reports[0]["findings"][0]["toolCallId"] == "srv-1"


def test_a_tool_call_model_carries_no_argument_error_when_the_arguments_are_valid() -> None:
    from pagerless_agent.model import ToolCall

    assert ToolCall(id="c1", name="pods_logs", arguments={"pod": "p"}).argument_error is None
