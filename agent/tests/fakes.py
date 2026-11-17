"""Stand-ins for the agent's collaborators."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Callable
from typing import Any

import httpx

from pagerless_agent.bundle import ContextBundle
from pagerless_agent.model import ModelProvider, ModelRequest, ModelResponse, ToolCall, ToolSpec, Usage
from pagerless_agent.settings import Target
from pagerless_agent.tools import ToolResult, UnknownToolError


def usage(input_tokens: int = 1000, output_tokens: int = 100) -> Usage:
    return Usage(input_tokens=input_tokens, output_tokens=output_tokens)


def call(call_id: str, tool: str, /, **arguments: Any) -> ToolCall:
    return ToolCall(id=call_id, name=tool, arguments=arguments)


def respond(
    *tool_calls: ToolCall, text: str = "", structured: dict[str, Any] | None = None, input_tokens: int = 1000
) -> ModelResponse:
    return ModelResponse(
        text=text, tool_calls=tuple(tool_calls), structured=structured, usage=usage(input_tokens)
    )


class ScriptedProvider:
    """A live provider that answers from a script and keeps every request it received."""

    def __init__(self, responses: list[ModelResponse]) -> None:
        self._responses = list(responses)
        self.requests: list[ModelRequest] = []

    async def complete(self, request: ModelRequest) -> ModelResponse:
        self.requests.append(request)
        return self._responses.pop(0)


class RecordedTools:
    """Tools that answer with recorded outputs, keyed by tool name and arguments."""

    def __init__(
        self,
        specs: list[ToolSpec],
        answers: dict[tuple[str, str], str],
        on_call: Callable[[str, dict[str, Any]], None] | None = None,
    ) -> None:
        self._specs = specs
        self.answers = answers
        self._on_call = on_call
        self.calls: list[tuple[str, dict[str, Any]]] = []

    def specs(self) -> list[ToolSpec]:
        return list(self._specs)

    async def call(self, name: str, arguments: dict[str, Any]) -> ToolResult:
        if name not in {s.name for s in self._specs}:
            raise UnknownToolError(name)
        self.calls.append((name, arguments))
        if self._on_call:
            self._on_call(name, arguments)
        key = (name, json.dumps(arguments, sort_keys=True))
        if key not in self.answers:
            return ToolResult(text=f"no recorded answer for {key}", is_error=True)
        return ToolResult(text=self.answers[key])


def tool_key(tool: str, /, **arguments: Any) -> tuple[str, str]:
    return (tool, json.dumps(arguments, sort_keys=True))


class FixedSource:
    """A Context Bundle source that returns one bundle."""

    def __init__(self, bundle: ContextBundle) -> None:
        self.bundle = bundle

    async def gather(self, target: Target) -> ContextBundle:
        return self.bundle


class StandInServer:
    """A stand-in for pagerless-server's agent API, with the contract's status codes.

    It authenticates the bearer token, accepts events while the Triage is
    Investigating, and answers a report with acceptance or Validation errors
    that the test scripts.
    """

    def __init__(self, token: str = "token-1", triage: str = "triage-1") -> None:
        self.token = token
        self.triage = triage
        self.investigating = True
        self.events: list[dict[str, Any]] = []
        self.reports: list[dict[str, Any]] = []
        self.report_answers: list[httpx.Response] = []
        self.requests: list[httpx.Request] = []
        self.unavailable_for = 0

    def refuse_report(self, *errors: tuple[str, str]) -> None:
        body = {"errors": [{"field": f, "message": m} for f, m in errors]}
        self.report_answers.append(httpx.Response(422, json=body))

    def handler(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if self.unavailable_for > 0:
            self.unavailable_for -= 1
            return httpx.Response(503, json={"message": "unavailable"})
        if request.headers.get("Authorization") != f"Bearer {self.token}":
            return httpx.Response(401, json={"message": "unauthenticated"})
        base = f"/v1/triages/{self.triage}"
        body = json.loads(request.content)
        if request.url.path == f"{base}/events":
            if not self.investigating:
                return httpx.Response(409, json={"message": "the Triage is not Investigating"})
            self.events.append(body)
            return httpx.Response(202)
        if request.url.path == f"{base}/report":
            if not self.investigating:
                return httpx.Response(409, json={"message": "the Triage is not Investigating"})
            self.reports.append(body)
            if self.report_answers:
                answer = self.report_answers.pop(0)
                return answer
            self.investigating = False
            return httpx.Response(200, json={"accepted": True, "phase": "Proposed"})
        return httpx.Response(403, json={"message": "forbidden"})

    def transport(self) -> httpx.MockTransport:
        return httpx.MockTransport(self.handler)


class Spy:
    """Wraps a provider and keeps every request it received."""

    def __init__(self, inner: ModelProvider) -> None:
        self._inner = inner
        self.requests: list[ModelRequest] = []

    async def complete(self, request: ModelRequest) -> ModelResponse:
        self.requests.append(request)
        return await self._inner.complete(request)


class HangingTools:
    """Tools whose every call outlasts any budget."""

    def specs(self) -> list[ToolSpec]:
        return [ToolSpec(name="pods_log", description="logs", input_schema={"type": "object"})]

    async def call(self, name: str, arguments: dict[str, Any]) -> ToolResult:
        await asyncio.sleep(60)
        return ToolResult(text="late")
