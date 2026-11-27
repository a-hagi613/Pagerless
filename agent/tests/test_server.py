"""The private HTTP contract: authentication, errors, replay and cancellation."""

from __future__ import annotations

import asyncio
from typing import Any

import pytest

from pagerless_agent.contract import TurnResult, encode
from pagerless_agent.readiness import ProbeResult
from tests.fakes import BlockingProvider, FakeGateway, FakeProvider, StubProbe, response
from tests.support import (
    app_client,
    auth_headers,
    contract_fixture,
    request_body,
    settings,
)


async def test_liveness_does_not_depend_on_readiness(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    probe = StubProbe(ProbeResult(False, "the model endpoint is unreachable"))
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway(), probe=probe) as (
        _,
        client,
    ):
        health = await client.get("/healthz")
        live = await client.get("/livez")
        assert health.status_code == 200
        assert health.json() == {"status": "ok"}
        assert live.status_code == 200
        assert probe.calls == 0


async def test_ready_reports_the_model_check(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    async with app_client(
        selected,
        provider=FakeProvider([]),
        gateway=FakeGateway(),
        probe=StubProbe(ProbeResult(True, "model tool-call probe succeeded")),
    ) as (_, client):
        ready = await client.get("/readyz")
        assert ready.status_code == 200
        assert ready.json() == {
            "status": "ready",
            "checks": [{"name": "model", "ok": True, "reason": "model tool-call probe succeeded"}],
        }


async def test_degraded_readiness_returns_503(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    async with app_client(
        selected,
        provider=FakeProvider([]),
        gateway=FakeGateway(),
        probe=StubProbe(ProbeResult(False, "the model endpoint is unreachable")),
    ) as (_, client):
        ready = await client.get("/readyz")
        assert ready.status_code == 503
        assert ready.json()["status"] == "degraded"
        assert ready.json()["checks"][0]["name"] == "model"


@pytest.mark.parametrize("headers", [{}, {"Authorization": "Bearer wrong"}, {"Authorization": "Basic x"}])
async def test_turn_requires_the_internal_bearer(tmp_path: Any, headers: dict[str, str]) -> None:
    selected = settings(tmp_path)
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post("/turn", json=request_body(), headers=headers)
        assert answered.status_code == 401
        assert answered.json()["error"]["code"] == "unauthorized"
        assert answered.headers["www-authenticate"] == "Bearer"


async def test_authentication_precedes_body_validation(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post("/turn", json={"pageId": "p1"})
        assert answered.status_code == 401
        assert answered.json()["error"]["code"] == "unauthorized"


async def test_a_turn_returns_the_terminal_shape(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider([response(text="Checkout is unavailable.")])
    async with app_client(selected, provider=provider, gateway=FakeGateway()) as (_, client):
        answered = await client.post("/turn", json=request_body(), headers=auth_headers())
        assert answered.status_code == 200
        body = answered.json()
        assert set(body) == {"status", "content", "citations", "usage"}
        assert body["status"] == "completed"
        assert body["content"] == "Checkout is unavailable."
        assert body["citations"] == []
        assert set(body["usage"]) == {"inputTokens", "outputTokens", "modelCalls", "toolCalls"}
        assert "proposal" not in body
        assert "error" not in body


async def test_the_checked_in_request_fixture_is_accepted(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    fixture = contract_fixture("turn-request.json")
    provider = FakeProvider([response(text="An answer.")])
    async with app_client(selected, provider=provider, gateway=FakeGateway()) as (_, client):
        answered = await client.post("/turn", json=fixture, headers=auth_headers())
        assert answered.status_code == 200
        assert answered.json()["status"] == "completed"


async def test_a_malformed_body_is_an_invalid_request(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post("/turn", json={"pageId": "p1"}, headers=auth_headers())
        assert answered.status_code == 400
        assert answered.json()["error"]["code"] == "invalid_request"


async def test_conflicting_namespace_hints_are_refused(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    body = request_body(
        namespace="shop", target={"kind": "Deployment", "name": "checkout", "namespace": "prod"}
    )
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post("/turn", json=body, headers=auth_headers())
        assert answered.status_code == 400
        assert "namespace" in answered.json()["error"]["message"]


@pytest.mark.parametrize(
    "callback_url",
    [
        "http://elsewhere.example:8080",
        "http://pagerless-server.pagerless.svc:8080/other",
        "http://pagerless-server.pagerless.svc:8080?x=1",
        "http://user:pass@pagerless-server.pagerless.svc:8080",
        "https://pagerless-server.pagerless.svc:8080",
    ],
)
async def test_callback_url_must_match_the_configured_origin(tmp_path: Any, callback_url: str) -> None:
    selected = settings(tmp_path)
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post(
            "/turn", json=request_body(callback_url=callback_url), headers=auth_headers()
        )
        assert answered.status_code == 400
        assert answered.json()["error"]["code"] == "invalid_request"


async def test_a_trailing_slash_on_the_callback_origin_is_accepted(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider([response(text="ok")])
    async with app_client(selected, provider=provider, gateway=FakeGateway()) as (_, client):
        answered = await client.post(
            "/turn",
            json=request_body(callback_url="http://pagerless-server.pagerless.svc:8080/"),
            headers=auth_headers(),
        )
        assert answered.status_code == 200


async def test_limits_cannot_raise_the_configured_ceiling(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post(
            "/turn",
            json=request_body(limits={"toolCalls": 7}),
            headers=auth_headers(),
        )
        assert answered.status_code == 400
        assert "lower" in answered.json()["error"]["message"]


async def test_a_second_turn_on_a_page_and_global_capacity_are_distinct(tmp_path: Any) -> None:
    selected = settings(tmp_path, PAGERLESS_AGENT_MAX_CONCURRENCY="1")
    provider = BlockingProvider(response(text="done"))
    async with app_client(selected, provider=provider, gateway=FakeGateway()) as (_, client):
        first = asyncio.create_task(
            client.post("/turn", json=request_body(page_id="p1", turn_id="t1"), headers=auth_headers())
        )
        await provider.started.wait()
        identical = await client.post(
            "/turn", json=request_body(page_id="p1", turn_id="t1"), headers=auth_headers()
        )
        assert identical.status_code == 409
        assert identical.json()["error"]["code"] == "turn_running"
        same_page = await client.post(
            "/turn", json=request_body(page_id="p1", turn_id="t2", message="Another?"), headers=auth_headers()
        )
        assert same_page.status_code == 409
        assert same_page.json()["error"]["code"] == "page_busy"
        different_payload = await client.post(
            "/turn",
            json=request_body(page_id="p1", turn_id="t1", message="Different?"),
            headers=auth_headers(),
        )
        assert different_payload.status_code == 409
        assert different_payload.json()["error"]["code"] == "idempotency_conflict"
        capacity = await client.post(
            "/turn", json=request_body(page_id="p2", turn_id="t2"), headers=auth_headers()
        )
        assert capacity.status_code == 429
        assert capacity.json()["error"]["code"] == "capacity"
        provider.release.set()
        assert (await first).status_code == 200


async def test_an_identical_finished_request_is_replayed(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider([response(text="done")])
    async with app_client(selected, provider=provider, gateway=FakeGateway()) as (_, client):
        first = await client.post("/turn", json=request_body(), headers=auth_headers())
        second = await client.post("/turn", json=request_body(), headers=auth_headers())
        assert first.status_code == 200
        assert second.status_code == 200
        assert first.json() == second.json()
        assert len(provider.requests) == 1


async def test_stop_returns_the_terminal_status_and_is_safe_to_repeat(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = BlockingProvider(response(text="late"))
    async with app_client(selected, provider=provider, gateway=FakeGateway()) as (_, client):
        turn = asyncio.create_task(client.post("/turn", json=request_body(), headers=auth_headers()))
        await provider.started.wait()
        stopped = await client.post("/turns/t1/stop", headers=auth_headers())
        assert stopped.status_code == 200
        assert stopped.json() == {"status": "stopped"}
        answered = await turn
        assert answered.json()["status"] == "stopped"
        finished = await client.post("/turns/t1/stop", headers=auth_headers())
        assert finished.json() == {"status": "finished"}
        unknown = await client.post("/turns/nope/stop", headers=auth_headers())
        assert unknown.json() == {"status": "unknown"}


async def test_stop_requires_the_internal_bearer(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post("/turns/t1/stop")
        assert answered.status_code == 401


async def test_an_oversized_request_is_refused_before_parsing(tmp_path: Any) -> None:
    selected = settings(tmp_path, PAGERLESS_AGENT_MAX_REQUEST_BYTES="1024")
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post(
            "/turn",
            content=b"x" * 2_000,
            headers={**auth_headers(), "Content-Type": "application/json"},
        )
        assert answered.status_code == 413
        assert answered.json()["error"]["code"] == "request_too_large"


async def test_unknown_endpoints_have_the_error_shape(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.get("/nope")
        assert answered.status_code == 404
        assert answered.json()["error"]["code"] == "not_found"


async def test_no_credential_is_echoed_in_an_authentication_error(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    async with app_client(selected, provider=FakeProvider([]), gateway=FakeGateway()) as (_, client):
        answered = await client.post(
            "/turn", json=request_body(), headers={"Authorization": "Bearer super-secret-value"}
        )
        assert answered.status_code == 401
        assert "super-secret-value" not in answered.text
        assert selected.token not in answered.text


async def test_error_response_for_a_conflict_is_not_a_result(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = BlockingProvider(response(text="done"))
    async with app_client(selected, provider=provider, gateway=FakeGateway()) as (_, client):
        first = asyncio.create_task(client.post("/turn", json=request_body(), headers=auth_headers()))
        await provider.started.wait()
        conflict = await client.post("/turn", json=request_body(), headers=auth_headers())
        assert conflict.status_code == 409
        assert set(conflict.json()) == {"error"}
        provider.release.set()
        await first


def test_contract_encoding_matches_the_documented_keys() -> None:
    fixture = TurnResult.model_validate(contract_fixture("turn-result-completed.json"))
    body = encode(fixture)
    assert set(body) == {"status", "content", "citations", "usage"}
