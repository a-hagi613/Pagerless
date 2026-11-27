"""Interoperability checks against the Go side of the documented contract.

The Go service is developed in parallel. These tests pin the two directions this agent
owns: the HTTP tool callback it sends, and the terminal JSON it returns.
"""

from __future__ import annotations

import json
from typing import Any

import httpx

from pagerless_agent.callback import ToolGateway
from pagerless_agent.contract import TurnResult, encode
from tests.fakes import CALLBACK_ORIGIN, TOKEN, FakeGateway, FakeProvider, call, response
from tests.support import contract_fixture, run_turn, settings, turn_request


async def test_the_agent_calls_go_with_the_documented_callback(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    seen: list[dict[str, Any]] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        seen.append(
            {
                "path": request.url.path,
                "authorization": request.headers.get("authorization"),
                "body": json.loads(request.content),
            }
        )
        return httpx.Response(
            200,
            json={
                "status": "ok",
                "result": {"data": {"items": [{"name": "worker-2", "memoryPressure": True}]}},
                "redacted": False,
                "truncated": False,
            },
        )

    gateway = ToolGateway(
        CALLBACK_ORIGIN,
        TOKEN,
        timeout_s=2.0,
        http=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    provider = FakeProvider(
        [
            response(calls=(call("model-call-1", "resources_get", {"kind": "Node", "name": "worker-2"}),)),
            response(
                calls=(
                    call(
                        "model-call-2",
                        "submit_answer",
                        {
                            "content": "worker-2 reports memory pressure.",
                            "citations": [{"callId": "c1", "pointer": "/data/items/0"}],
                        },
                    ),
                )
            ),
        ]
    )
    result = await run_turn(selected, provider, gateway)
    assert result.status == "completed"
    assert seen[0]["path"] == "/internal/turns/t1/tools"
    assert seen[0]["authorization"] == f"Bearer {TOKEN}"
    assert seen[0]["body"] == {
        "callId": "c1",
        "tool": "resources_get",
        "arguments": {"kind": "Node", "name": "worker-2"},
    }
    assert result.citations[0].call_id == "c1"


async def test_the_terminal_result_matches_the_shared_fixture(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider(
        [
            response(calls=(call("m1", "resources_get", {"kind": "Node", "name": "worker-2"}),)),
            response(
                calls=(
                    call(
                        "m2",
                        "submit_answer",
                        {
                            "content": "Checkout calls an upstream that is not accepting connections.",
                            "citations": [{"callId": "c1", "pointer": "/data/items/0"}],
                        },
                    ),
                )
            ),
        ]
    )
    result = await run_turn(selected, provider, FakeGateway())
    fixture = TurnResult.model_validate(contract_fixture("turn-result-completed.json"))
    assert set(encode(result)) == set(encode(fixture))
    assert encode(result)["citations"][0]["callId"] == "c1"


async def test_the_go_turn_request_fixture_runs(tmp_path: Any) -> None:
    fixture = contract_fixture("turn-request.json")
    selected = settings(tmp_path)
    request = turn_request(
        page_id=fixture["pageId"],
        turn_id=fixture["turnId"],
        message=fixture["messages"][-1]["content"],
        tools=fixture["tools"],
        namespace=fixture["namespace"],
        target=fixture["target"],
        callback_url=fixture["callbackUrl"],
        limits=fixture["limits"],
    )
    provider = FakeProvider([response(text="A bounded answer.")])
    result = await run_turn(selected, provider, FakeGateway(), request)
    assert result.status == "completed"
    assert result.content == "A bounded answer."
