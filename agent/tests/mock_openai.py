"""A fixture-backed OpenAI-compatible mock server.

It serves `POST /v1/chat/completions` over an in-process `httpx2.MockTransport`, so a test
exercises the real HTTP boundary the SDK and the agent share with a provider, with no
socket, no network, and no credential. Selection is by `base_url` alone: the agent cannot
tell this server from a real endpoint.

The server supports four ladder profiles by rejecting the parameters a given class of
provider rejects, and it can inject the faults real providers produce: a `400` naming a
rejected parameter, `429`, truncation, and a response with no `usage` at all.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Literal

import httpx2

Profile = Literal["strict", "json_object", "tool", "tool_without_choice", "text", "no_tools"]

PROFILES: tuple[Profile, ...] = ("strict", "json_object", "tool", "tool_without_choice", "text")
SUBMIT_OUTPUT = "submit_output"


@dataclass(frozen=True)
class Step:
    """One scripted answer, or one injected fault."""

    content: str = ""
    payload: dict[str, Any] | None = None
    tool_calls: tuple[dict[str, str], ...] = ()
    finish_reason: str = "stop"
    usage: tuple[int, int] | None = (120, 30)
    status: int = 200
    error: str = "the provider failed"
    reasoning_content: str | None = None


@dataclass
class MockOpenAI:
    """The fixture-backed server: a step queue, a provider profile, and what it received."""

    steps: list[Step]
    profile: Profile = "strict"
    requests: list[dict[str, Any]] = field(default_factory=list)
    answered: list[dict[str, Any]] = field(default_factory=list)
    rejections: list[str] = field(default_factory=list)
    headers: list[dict[str, str]] = field(default_factory=list)
    served: int = 0
    _pending: Step | None = None

    @classmethod
    def from_fixture(cls, path: Path, *, profile: Profile = "strict") -> MockOpenAI:
        """Load a scripted investigation from a JSON fixture."""
        document = json.loads(path.read_text(encoding="utf-8"))
        steps = [
            Step(
                content=item.get("content", ""),
                payload=item.get("payload"),
                tool_calls=tuple(item.get("toolCalls", item.get("tool_calls", ()))),
                finish_reason=item.get("finishReason", "stop"),
                usage=(int(item["usage"][0]), int(item["usage"][1])) if "usage" in item else (120, 30),
                status=item.get("status", 200),
                error=item.get("error", "the provider failed"),
                reasoning_content=item.get("reasoningContent"),
            )
            for item in document["steps"]
        ]
        return cls(steps=steps, profile=profile)

    def transport(self) -> httpx2.MockTransport:
        """Return the transport to hand to an `httpx2.AsyncClient`."""
        return httpx2.MockTransport(self.handler)

    def handler(self, request: httpx2.Request) -> httpx2.Response:
        """Serve one Chat Completions request."""
        if request.url.path not in {"/v1/chat/completions", "/chat/completions"}:
            return httpx2.Response(404, json={"error": {"message": "no such route"}})
        body = json.loads(request.content)
        self.requests.append(body)
        self.headers.append(dict(request.headers))
        rejection = self._rejection(body)
        if rejection is not None:
            self.rejections.append(rejection)
            return _error(400, rejection)
        # A structured stage may be asked twice: once for a tool call, once for JSON in
        # text. Both attempts belong to the same fixture step, so the second reuses it.
        structured = "response_format" in body or any(
            isinstance(tool, dict) and tool.get("function", {}).get("name") == SUBMIT_OUTPUT
            for tool in body.get("tools", [])
        )
        reuse = not structured and not body.get("tools") and self._pending is not None
        if reuse:
            step = self._pending
            assert step is not None
            self._pending = None
        else:
            if self.served >= len(self.steps):
                return _error(500, "the fixture has no step left")
            step = self.steps[self.served]
            self.served += 1
            if structured:
                self._pending = step
        self.answered.append(body)
        if step.status != 200:
            return _error(step.status, step.error)
        return self._answer(step, body)

    def _rejection(self, body: dict[str, Any]) -> str | None:
        """Return the message of a `400` that names a parameter this profile rejects."""
        response_format = body.get("response_format")
        if self.profile == "strict":
            return None
        if self.profile == "no_tools" and "tools" in body:
            return "tools are not supported by this endpoint"
        if isinstance(response_format, dict):
            kind = response_format.get("type")
            if self.profile == "json_object" and kind == "json_object":
                return None
            named = "json_schema" if kind == "json_schema" else "strict"
            return f"response_format is not supported: {kind} is not available ({named})"
        if self.profile in {"text", "tool_without_choice"} and isinstance(body.get("tool_choice"), dict):
            return "tool_choice is not supported; only the default is accepted"
        return None

    def _answer(self, step: Step, body: dict[str, Any]) -> httpx2.Response:
        message: dict[str, Any] = {"role": "assistant", "content": step.content}
        if step.reasoning_content is not None:
            message["reasoning_content"] = step.reasoning_content
        tools = body.get("tools")
        offers_submit = isinstance(tools, list) and any(
            isinstance(tool, dict) and tool.get("function", {}).get("name") == SUBMIT_OUTPUT for tool in tools
        )
        if step.tool_calls:
            message["tool_calls"] = [
                {
                    "id": f"call_{self.served}_{index}",
                    "type": "function",
                    "function": {"name": call["name"], "arguments": call.get("arguments", "{}")},
                }
                for index, call in enumerate(step.tool_calls)
            ]
        elif step.payload is not None and offers_submit and self.profile != "text":
            message["tool_calls"] = [
                {
                    "id": f"call_{self.served}",
                    "type": "function",
                    "function": {
                        "name": SUBMIT_OUTPUT,
                        "arguments": json.dumps(step.payload, sort_keys=True),
                    },
                }
            ]
            message["content"] = ""
        elif step.payload is not None:
            message["content"] = json.dumps(step.payload, sort_keys=True)
        payload: dict[str, Any] = {
            "id": f"chatcmpl-{self.served}",
            "object": "chat.completion",
            "created": 1_700_000_000 + self.served,
            "model": body.get("model", "mock-model"),
            "choices": [{"index": 0, "finish_reason": step.finish_reason, "message": message}],
        }
        if step.usage is not None:
            payload["usage"] = {
                "prompt_tokens": step.usage[0],
                "completion_tokens": step.usage[1],
                "total_tokens": step.usage[0] + step.usage[1],
            }
        return httpx2.Response(200, json=payload)


def _error(status: int, message: str) -> httpx2.Response:
    return httpx2.Response(
        status,
        json={"error": {"message": message, "type": "invalid_request_error", "code": None}},
    )
