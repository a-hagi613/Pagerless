"""Deterministic stand-ins for the model endpoint and the Go tool policy."""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from typing import Any

from pagerless_agent.budget import Budget
from pagerless_agent.callback import ToolOutcome
from pagerless_agent.model import ModelRequest, ModelResponse, ToolCall, Usage
from pagerless_agent.readiness import ProbeResult

TOKEN = "internal-test-token"
API_KEY = "test-model-key"
CALLBACK_ORIGIN = "http://pagerless-server.pagerless.svc:8080"


class FakeProvider:
    """Answers from a script and records every request."""

    def __init__(
        self,
        responses: list[ModelResponse | Exception],
        *,
        estimate: int = 1_000,
        before: Callable[[ModelRequest], Awaitable[None]] | None = None,
    ) -> None:
        self._responses = list(responses)
        self._estimate = estimate
        self._before = before
        self.requests: list[ModelRequest] = []

    async def complete(self, request: ModelRequest, *, budget: Budget | None = None) -> ModelResponse:
        self.requests.append(request)
        if budget is not None:
            budget.spend_model_call()
        if self._before is not None:
            await self._before(request)
        if not self._responses:
            raise AssertionError("the fake provider ran out of scripted responses")
        item = self._responses.pop(0)
        if isinstance(item, Exception):
            raise item
        return item

    def estimate_input_tokens(self, request: ModelRequest) -> int:
        return self._estimate


class FakeGateway:
    """Answers tool callbacks from a script and records every call."""

    def __init__(
        self,
        *,
        outcomes: list[ToolOutcome | Exception] | None = None,
        answers: dict[str, ToolOutcome | Callable[[str, dict[str, Any]], ToolOutcome]] | None = None,
        before: Callable[[str, dict[str, Any]], Awaitable[None]] | None = None,
    ) -> None:
        self._outcomes = list(outcomes or [])
        self._answers = dict(answers or {})
        self._before = before
        self.calls: list[dict[str, Any]] = []

    async def call(
        self,
        *,
        turn_id: str,
        call_id: str,
        tool: str,
        arguments: dict[str, Any],
        timeout_s: float | None = None,
    ) -> ToolOutcome:
        self.calls.append(
            {
                "turn_id": turn_id,
                "call_id": call_id,
                "tool": tool,
                "arguments": arguments,
                "timeout_s": timeout_s,
            }
        )
        if self._before is not None:
            await self._before(tool, arguments)
        if self._outcomes:
            item = self._outcomes.pop(0)
            if isinstance(item, Exception):
                raise item
            return item
        answer = self._answers.get(tool)
        if answer is not None:
            return answer(call_id, arguments) if callable(answer) else answer
        return ToolOutcome(
            status="ok",
            result={
                "data": {
                    "tool": tool,
                    "namespace": arguments.get("namespace"),
                    "items": [{"name": "checkout"}],
                },
                "meta": {"tool": tool},
            },
        )

    def calls_to(self, name: str) -> int:
        return sum(1 for call in self.calls if call["tool"] == name)

    async def aclose(self) -> None:
        return None


class StubProbe:
    """A readiness probe fixed to one result."""

    def __init__(self, result: ProbeResult | None = None) -> None:
        self.result = result or ProbeResult(True, "model tool-call probe succeeded")
        self.calls = 0

    async def check(self) -> ProbeResult:
        self.calls += 1
        return self.result


class BlockingProvider(FakeProvider):
    """Blocks on a request until the test releases it."""

    def __init__(self, response: ModelResponse) -> None:
        super().__init__([response])
        self.started = asyncio.Event()
        self.release = asyncio.Event()

    async def complete(self, request: ModelRequest, *, budget: Budget | None = None) -> ModelResponse:
        self.started.set()
        await self.release.wait()
        return await super().complete(request, budget=budget)


def response(
    *,
    text: str = "",
    calls: tuple[ToolCall, ...] = (),
    input_tokens: int = 1_000,
    output_tokens: int = 100,
    estimated: bool = False,
    finish_reason: str | None = None,
) -> ModelResponse:
    """Build one scripted model answer."""
    return ModelResponse(
        text=text,
        tool_calls=calls,
        usage=Usage(input_tokens=input_tokens, output_tokens=output_tokens, estimated=estimated),
        finish_reason=finish_reason,
    )


def call(call_id: str, name: str, arguments: dict[str, Any] | None = None) -> ToolCall:
    """Build one scripted tool call."""
    return ToolCall(id=call_id, name=name, arguments=arguments or {})
