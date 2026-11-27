"""Cached, bounded model readiness."""

from __future__ import annotations

import asyncio

import pytest

from pagerless_agent.budget import BudgetExhaustedError
from pagerless_agent.model import ModelRequest
from pagerless_agent.openai_compat import (
    ProviderAuthError,
    ProviderMalformedError,
    ProviderRejectedError,
    ProviderTimeoutError,
    ProviderTruncatedError,
    ProviderUnavailableError,
)
from pagerless_agent.readiness import ModelProbe
from tests.fakes import FakeProvider, call, response


class Clock:
    """A manually advanced monotonic clock."""

    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now


def probe(provider: FakeProvider, clock: Clock | None = None, *, ttl: float = 300.0) -> ModelProbe:
    return ModelProbe(
        provider,
        ttl_s=ttl,
        timeout_s=1.0,
        clock=clock or Clock(),
    )


async def test_success_requires_a_real_tool_call() -> None:
    provider = FakeProvider([response(calls=(call("probe", "readiness_probe"),))])
    result = await probe(provider).check()
    assert result.ok is True
    assert result.reason == "model tool-call probe succeeded"
    request = provider.requests[0]
    assert request.forced_tool == "readiness_probe"
    assert request.purpose == "readiness"
    assert [tool.name for tool in request.tools] == ["readiness_probe"]


async def test_a_text_only_answer_is_degraded() -> None:
    provider = FakeProvider([response(text="all good")])
    result = await probe(provider).check()
    assert result.ok is False
    assert result.reason == "the model did not call the readiness tool"


@pytest.mark.parametrize(
    ("error", "reason"),
    [
        (ProviderAuthError("bad key"), "the model endpoint rejected the credentials"),
        (
            ProviderRejectedError("no tools", reason="tools_unsupported"),
            "the model endpoint does not support tool calls",
        ),
        (ProviderTimeoutError("slow"), "the model tool-call probe timed out"),
        (ProviderUnavailableError("down"), "the model endpoint is unreachable"),
        (ProviderMalformedError("bad"), "the model endpoint returned a malformed response"),
        (ProviderTruncatedError("short"), "the model endpoint truncated the probe"),
        (BudgetExhaustedError("model calls", "spent"), "the probe exceeded its call budget"),
    ],
)
async def test_provider_failures_have_static_reasons(error: Exception, reason: str) -> None:
    provider = FakeProvider([error])
    result = await probe(provider).check()
    assert result.ok is False
    assert result.reason == reason
    assert "http" not in result.reason
    assert "test-model-key" not in result.reason


async def test_a_successful_probe_is_cached() -> None:
    clock = Clock()
    provider = FakeProvider(
        [
            response(calls=(call("probe", "readiness_probe"),)),
            response(calls=(call("probe", "readiness_probe"),)),
        ]
    )
    selected = probe(provider, clock)
    assert (await selected.check()).ok
    clock.now = 100.0
    assert (await selected.check()).ok
    assert len(provider.requests) == 1
    clock.now = 301.0
    assert (await selected.check()).ok
    assert len(provider.requests) == 2


async def test_a_failure_backs_off_before_probing_again() -> None:
    clock = Clock()
    provider = FakeProvider(
        [
            ProviderAuthError("bad"),
            ProviderAuthError("bad"),
            response(calls=(call("probe", "readiness_probe"),)),
        ]
    )
    selected = probe(provider, clock)
    assert (await selected.check()).ok is False
    clock.now = 1.0
    assert (await selected.check()).ok is False
    assert len(provider.requests) == 1
    clock.now = 6.0
    assert (await selected.check()).ok is False
    assert len(provider.requests) == 2
    clock.now = 16.0
    assert (await selected.check()).ok is True
    assert len(provider.requests) == 3
    clock.now = 100.0
    assert (await selected.check()).ok is True
    assert len(provider.requests) == 3


async def test_concurrent_probes_share_one_call() -> None:
    gate = asyncio.Event()

    async def before(_: ModelRequest) -> None:
        await gate.wait()

    provider = FakeProvider([response(calls=(call("probe", "readiness_probe"),))], before=before)
    selected = probe(provider)
    first = asyncio.create_task(selected.check())
    await asyncio.sleep(0)
    second = asyncio.create_task(selected.check())
    await asyncio.sleep(0)
    gate.set()
    results = await asyncio.gather(first, second)
    assert [result.ok for result in results] == [True, True]
    assert len(provider.requests) == 1


async def test_a_slow_probe_times_out_and_is_degraded() -> None:
    async def before(_: ModelRequest) -> None:
        await asyncio.sleep(0.3)

    provider = FakeProvider([response(calls=(call("probe", "readiness_probe"),))], before=before)
    selected = ModelProbe(provider, ttl_s=300.0, timeout_s=0.05)
    result = await selected.check()
    assert result.ok is False
    assert result.reason == "the model tool-call probe timed out"
