"""The authenticated, redirect-free tool callback channel."""

from __future__ import annotations

import asyncio
import json
from typing import Any, cast

import httpx
import pytest

from pagerless_agent.callback import (
    MAX_CALLBACK_BYTES,
    ToolGateway,
    ToolProtocolError,
    ToolRejectedError,
    ToolUnavailableError,
)
from tests.fakes import TOKEN

BASE = "http://pagerless-server.pagerless.svc:8080"


def gateway(handler: Any, *, timeout_s: float = 2.0) -> tuple[ToolGateway, list[httpx.Request]]:
    requests: list[httpx.Request] = []

    async def wrapped(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        result = handler(request)
        if asyncio.iscoroutine(result):
            return cast(httpx.Response, await result)
        return cast(httpx.Response, result)

    client = httpx.AsyncClient(
        transport=httpx.MockTransport(wrapped),
        follow_redirects=False,
    )

    async def sleep(_: float) -> None:
        return None

    return ToolGateway(BASE, TOKEN, timeout_s=timeout_s, http=client, sleep=sleep), requests


def ok(result: Any = None, **flags: Any) -> httpx.Response:
    return httpx.Response(
        200,
        json={
            "status": "ok",
            "result": {"data": {"items": []}} if result is None else result,
            "redacted": flags.get("redacted", False),
            "truncated": False,
        },
    )


async def test_callback_url_headers_and_body() -> None:
    tool, requests = gateway(lambda _: ok())
    outcome = await tool.call(
        turn_id="t1",
        call_id="c1",
        tool="resources_get",
        arguments={"kind": "Node", "name": "worker-2"},
    )
    assert outcome.status == "ok"
    assert outcome.result == {"data": {"items": []}}
    request = requests[0]
    assert request.method == "POST"
    assert request.url.path == "/internal/turns/t1/tools"
    assert request.headers["Authorization"] == f"Bearer {TOKEN}"
    assert json.loads(request.content) == {
        "callId": "c1",
        "tool": "resources_get",
        "arguments": {"kind": "Node", "name": "worker-2"},
    }


@pytest.mark.parametrize("status", ["error", "denied", "timed_out", "truncated"])
async def test_tool_error_statuses_are_returned(status: str) -> None:
    tool, _ = gateway(
        lambda _: httpx.Response(
            200, json={"status": status, "result": {"message": "no"}, "redacted": False, "truncated": False}
        )
    )
    outcome = await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})
    assert outcome.status == status
    assert outcome.is_error is (status != "truncated")


async def test_budget_exhausted_status_needs_no_result() -> None:
    tool, _ = gateway(lambda _: httpx.Response(200, json={"status": "budget_exhausted"}))
    outcome = await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})
    assert outcome.status == "budget_exhausted"


async def test_auth_failure_is_distinct() -> None:
    tool, _ = gateway(lambda _: httpx.Response(401, json={"message": "no"}))
    with pytest.raises(ToolRejectedError):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})


async def test_redirect_is_refused_without_following() -> None:
    tool, requests = gateway(
        lambda _: httpx.Response(302, headers={"location": "http://elsewhere.example/steal"})
    )
    with pytest.raises(ToolProtocolError, match="redirect"):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})
    assert len(requests) == 1


async def test_server_errors_are_retried_with_the_same_call_id() -> None:
    calls = {"count": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["count"] += 1
        return httpx.Response(503) if calls["count"] == 1 else ok()

    tool, requests = gateway(handler)
    outcome = await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={"a": 1})
    assert outcome.status == "ok"
    assert len(requests) == 2
    assert {json.loads(request.content)["callId"] for request in requests} == {"c1"}


async def test_repeated_server_errors_are_unavailable() -> None:
    tool, _ = gateway(lambda _: httpx.Response(503))
    with pytest.raises(ToolUnavailableError):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})


async def test_transport_error_is_retried_once() -> None:
    calls = {"count": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["count"] += 1
        if calls["count"] == 1:
            raise httpx.ConnectError("refused")
        return ok()

    tool, requests = gateway(handler)
    outcome = await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})
    assert outcome.status == "ok"
    assert len(requests) == 2


async def test_timeout_is_visible() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("slow", request=request)

    tool, requests = gateway(handler, timeout_s=0.02)
    with pytest.raises(ToolUnavailableError):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})
    assert len(requests) == 2


async def test_malformed_json_is_a_protocol_error() -> None:
    tool, _ = gateway(lambda _: httpx.Response(200, content=b"not json"))
    with pytest.raises(ToolProtocolError, match="not JSON"):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})


async def test_unknown_status_is_a_protocol_error() -> None:
    tool, _ = gateway(lambda _: httpx.Response(200, json={"status": "maybe", "result": {}}))
    with pytest.raises(ToolProtocolError, match="malformed"):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})


async def test_success_without_a_result_is_a_protocol_error() -> None:
    tool, _ = gateway(lambda _: httpx.Response(200, json={"status": "ok"}))
    with pytest.raises(ToolProtocolError, match="no result"):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})


async def test_oversized_result_is_discarded() -> None:
    tool, _ = gateway(
        lambda _: httpx.Response(
            200,
            content=b"x" * (MAX_CALLBACK_BYTES + 1),
            headers={"content-type": "application/json"},
        )
    )
    with pytest.raises(ToolProtocolError, match="byte limit"):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})


async def test_declared_oversized_result_is_refused_before_reading() -> None:
    tool, _ = gateway(
        lambda _: httpx.Response(
            200,
            content=b"{}",
            headers={"content-length": str(MAX_CALLBACK_BYTES + 1), "content-type": "application/json"},
        )
    )
    with pytest.raises(ToolProtocolError, match="declared"):
        await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})


async def test_redacted_and_truncated_flags_are_preserved() -> None:
    tool, _ = gateway(
        lambda _: httpx.Response(
            200,
            json={"status": "truncated", "result": {"data": {}}, "redacted": True, "truncated": True},
        )
    )
    outcome = await tool.call(turn_id="t1", call_id="c1", tool="x", arguments={})
    assert outcome.redacted is True
    assert outcome.truncated is True
    assert outcome.is_evidence is True
