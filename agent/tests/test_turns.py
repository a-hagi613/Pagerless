"""Turn concurrency, replay, conflict and cancellation semantics."""

from __future__ import annotations

import asyncio
from typing import Any

import pytest

from pagerless_agent.contract import Citation, TurnResult
from pagerless_agent.turns import TurnCapacityError, TurnConflictError, TurnManager
from tests.fakes import BlockingProvider, FakeGateway, FakeProvider, call, response
from tests.support import settings, turn_request


class Clock:
    """A manually advanced monotonic clock."""

    def __init__(self) -> None:
        self.now = 100.0

    def __call__(self) -> float:
        return self.now


def manager(
    selected: Any,
    provider: FakeProvider,
    gateway: FakeGateway | None = None,
    *,
    clock: Clock | None = None,
    completed_ttl_s: float = 600.0,
    max_completed: int = 256,
) -> TurnManager:
    return TurnManager(
        selected,
        provider,
        gateway or FakeGateway(),
        clock=clock or Clock(),
        completed_ttl_s=completed_ttl_s,
        max_completed=max_completed,
    )


async def test_a_completed_turn_is_replayed_without_another_call(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider([response(text="done")])
    turns = manager(selected, provider)
    request = turn_request()
    first = await turns.submit(request)
    second = await turns.submit(request)
    assert first.status == "completed"
    assert second.content == first.content
    assert len(provider.requests) == 1


async def test_a_changed_payload_for_a_finished_turn_is_a_conflict(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider([response(text="done")])
    turns = manager(selected, provider)
    await turns.submit(turn_request())
    with pytest.raises(TurnConflictError) as caught:
        await turns.submit(turn_request(message="A different question?"))
    assert caught.value.code == "idempotency_conflict"
    assert len(provider.requests) == 1


async def test_a_second_turn_on_the_same_page_is_refused(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = BlockingProvider(response(text="done"))
    turns = manager(selected, provider)
    first = asyncio.create_task(turns.submit(turn_request(turn_id="t1")))
    await provider.started.wait()
    with pytest.raises(TurnConflictError) as caught:
        await turns.submit(turn_request(turn_id="t2", message="Another question?"))
    assert caught.value.code == "page_busy"
    provider.release.set()
    await first


async def test_an_identical_active_turn_is_reported_as_running(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = BlockingProvider(response(text="done"))
    turns = manager(selected, provider)
    first = asyncio.create_task(turns.submit(turn_request()))
    await provider.started.wait()
    with pytest.raises(TurnConflictError) as caught:
        await turns.submit(turn_request())
    assert caught.value.code == "turn_running"
    provider.release.set()
    await first


async def test_reusing_a_turn_id_for_a_different_payload_is_a_conflict(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = BlockingProvider(response(text="done"))
    turns = manager(selected, provider)
    first = asyncio.create_task(turns.submit(turn_request(turn_id="t1")))
    await provider.started.wait()
    with pytest.raises(TurnConflictError) as caught:
        await turns.submit(turn_request(turn_id="t1", message="Different?"))
    assert caught.value.code == "idempotency_conflict"
    provider.release.set()
    await first


async def test_global_capacity_is_bounded(tmp_path: Any) -> None:
    selected = settings(tmp_path, PAGERLESS_AGENT_MAX_CONCURRENCY="1")
    provider = BlockingProvider(response(text="done"))
    turns = manager(selected, provider)
    first = asyncio.create_task(turns.submit(turn_request(page_id="p1", turn_id="t1")))
    await provider.started.wait()
    with pytest.raises(TurnCapacityError):
        await turns.submit(turn_request(page_id="p2", turn_id="t2"))
    provider.release.set()
    await first
    assert turns.active_turns == 0


async def test_stop_returns_stopped_and_then_finished(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = BlockingProvider(response(text="late"))
    turns = manager(selected, provider)
    task = asyncio.create_task(turns.submit(turn_request()))
    await provider.started.wait()
    assert (await turns.stop("t1")).status == "stopped"
    result = await task
    assert result.status == "stopped"
    assert (await turns.stop("t1")).status == "finished"
    assert (await turns.stop("unknown")).status == "unknown"


async def test_shutdown_cancels_running_turns(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = BlockingProvider(response(text="late"))
    turns = manager(selected, provider)
    task = asyncio.create_task(turns.submit(turn_request()))
    await provider.started.wait()
    await turns.shutdown()
    result = await asyncio.wait_for(task, timeout=1)
    assert result.status == "stopped"


async def test_request_limits_lower_the_service_ceiling(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider(
        [
            response(calls=(call("m1", "resources_get", {"kind": "Node", "name": "a"}),)),
            response(calls=(call("m2", "resources_get", {"kind": "Node", "name": "b"}),)),
        ]
    )
    gateway = FakeGateway()
    turns = manager(selected, provider, gateway)
    result = await turns.submit(turn_request(limits={"toolCalls": 1}))
    assert result.status == "failed"
    assert result.error is not None
    assert result.error.code == "budget_exhausted"
    assert len(gateway.calls) == 1


async def test_a_completed_result_expires(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    clock = Clock()
    provider = FakeProvider([response(text="one"), response(text="two")])
    turns = manager(selected, provider, clock=clock, completed_ttl_s=10)
    first = await turns.submit(turn_request())
    clock.now += 11
    second = await turns.submit(turn_request())
    assert first.content == "one"
    assert second.content == "two"
    assert len(provider.requests) == 2


async def test_the_completed_cache_is_bounded(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider([response(text="one"), response(text="two"), response(text="three")])
    turns = manager(selected, provider, max_completed=1)
    await turns.submit(turn_request(page_id="p1", turn_id="t1"))
    await turns.submit(turn_request(page_id="p2", turn_id="t2"))
    third = await turns.submit(turn_request(page_id="p1", turn_id="t1"))
    assert third.content == "three"
    assert len(provider.requests) == 3


async def test_the_response_is_a_defensive_copy(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    provider = FakeProvider([response(text="done")])
    turns = manager(selected, provider)
    first = await turns.submit(turn_request())
    first.citations.append(Citation(call_id="injected"))
    second = await turns.submit(turn_request())
    assert second.content == "done"
    assert second.citations == []
    assert isinstance(second, TurnResult)
