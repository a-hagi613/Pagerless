"""The wire contract parses and re-encodes the checked-in interoperability fixtures."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from pagerless_agent.contract import (
    CallbackResponse,
    ReadyResult,
    StopResult,
    TurnError,
    TurnRequest,
    TurnResult,
    encode,
    validation_message,
)
from tests.support import contract_fixture, request_body


@pytest.mark.parametrize("name", ["turn-request.json", "turn-request-followup.json"])
def test_request_fixture_round_trips(name: str) -> None:
    fixture = contract_fixture(name)
    request = TurnRequest.model_validate(fixture)
    assert encode(request) == fixture


@pytest.mark.parametrize(
    "name",
    [
        "turn-result-completed.json",
        "turn-result-proposal.json",
        "turn-result-failed.json",
    ],
)
def test_result_fixture_round_trips(name: str) -> None:
    fixture = contract_fixture(name)
    result = TurnResult.model_validate(fixture)
    assert encode(result) == fixture


def test_callback_fixture_round_trips() -> None:
    fixture = contract_fixture("tool-callback-response.json")
    response = CallbackResponse.model_validate(fixture)
    assert encode(response) == fixture


def test_stop_and_ready_fixtures_round_trip() -> None:
    stop = contract_fixture("stop-response.json")
    assert encode(StopResult.model_validate(stop)) == stop
    ready = contract_fixture("ready-response.json")
    assert encode(ReadyResult.model_validate(ready)) == ready


def test_camel_case_names_are_exact() -> None:
    result = TurnResult.model_validate(contract_fixture("turn-result-proposal.json"))
    body = encode(result)
    assert set(body) == {"status", "content", "citations", "proposal", "usage"}
    assert set(body["proposal"]) == {"baseSha", "files"}
    assert set(body["usage"]) == {"inputTokens", "outputTokens", "modelCalls", "toolCalls"}
    assert set(body["citations"][0]) == {"callId", "pointer"}


def test_callback_ignores_an_echoed_call_id() -> None:
    response = CallbackResponse.model_validate(
        {"status": "ok", "result": {"data": None}, "redacted": False, "truncated": False, "callId": "c-1"}
    )
    assert response.status == "ok"
    assert "callId" not in encode(response)


def test_namespace_conflict_is_rejected() -> None:
    body = request_body(
        namespace="shop", target={"kind": "Deployment", "name": "checkout", "namespace": "prod"}
    )
    with pytest.raises(ValidationError, match="namespace conflicts"):
        TurnRequest.model_validate(body)


def test_namespace_hint_may_come_from_the_target() -> None:
    request = TurnRequest.model_validate(
        request_body(namespace=None, target={"kind": "Deployment", "name": "checkout", "namespace": "shop"})
    )
    assert request.effective_namespace() == "shop"


def test_last_message_must_be_a_user_question() -> None:
    with pytest.raises(ValidationError, match="last message"):
        TurnRequest.model_validate(
            request_body(messages=[{"role": "assistant", "content": "I answered already."}])
        )
    with pytest.raises(ValidationError, match="last message"):
        TurnRequest.model_validate(request_body(messages=[{"role": "user", "content": "   "}]))


def test_reserved_and_duplicate_tool_names_are_rejected() -> None:
    reserved = request_body(tools=[{"name": "submit_answer", "description": "", "inputSchema": {}}])
    with pytest.raises(ValidationError, match="reserved"):
        TurnRequest.model_validate(reserved)
    repeated = request_body(
        tools=[
            {"name": "read_one", "description": "", "inputSchema": {}},
            {"name": "read_one", "description": "", "inputSchema": {}},
        ]
    )
    with pytest.raises(ValidationError, match="share a name"):
        TurnRequest.model_validate(repeated)


def test_tool_schema_is_bounded() -> None:
    body = request_body(
        tools=[{"name": "read_one", "description": "", "inputSchema": {"description": "x" * 40_000}}]
    )
    with pytest.raises(ValidationError, match="at most"):
        TurnRequest.model_validate(body)


def test_limits_are_bounded() -> None:
    with pytest.raises(ValidationError):
        TurnRequest.model_validate(request_body(limits={"toolCalls": 0}))
    with pytest.raises(ValidationError):
        TurnRequest.model_validate(request_body(limits={"seconds": 86_401}))


def test_fingerprint_is_stable_and_payload_sensitive() -> None:
    first = TurnRequest.model_validate(request_body(message="Why is checkout timing out?"))
    same = TurnRequest.model_validate(request_body(message="Why is checkout timing out?"))
    different = TurnRequest.model_validate(request_body(message="Why is checkout slow?"))
    assert first.fingerprint() == same.fingerprint()
    assert first.fingerprint() != different.fingerprint()


def test_result_status_invariants() -> None:
    completed = TurnResult.model_validate(contract_fixture("turn-result-completed.json"))
    proposal = TurnResult.model_validate(contract_fixture("turn-result-proposal.json")).proposal
    with pytest.raises(ValidationError, match="requires an error"):
        TurnResult(status="failed", content="", usage=completed.usage)
    with pytest.raises(ValidationError, match="only a failed"):
        TurnResult(
            status="completed",
            content="x",
            error=TurnError(code="internal_error", message="x"),
            usage=completed.usage,
        )
    with pytest.raises(ValidationError, match="cannot carry a proposal"):
        TurnResult(
            status="failed",
            content="",
            proposal=proposal,
            error=TurnError(code="internal_error", message="x"),
            usage=completed.usage,
        )


def test_validation_message_is_bounded() -> None:
    with pytest.raises(ValidationError) as caught:
        TurnRequest.model_validate({})
    message = validation_message(caught.value)
    assert len(message) <= 500
    assert "pageId" in message or "turnId" in message
