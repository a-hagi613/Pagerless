"""Stand-ins for the agent's two collaborators."""

from __future__ import annotations

import json
from collections.abc import Callable, Mapping
from typing import Any

import httpx

from pagerless_agent.budget import Budget
from pagerless_agent.model import ModelRequest, ModelResponse, Usage

TOKEN = "projected-token-1"


class StandInBroker:
    """A stand-in for the server's tool broker and Page endpoints.

    It authenticates the projected token, checks the Page UID header on every request,
    requires an idempotency key on every tool call, records what it received, and can
    refuse a report with the `422` validation errors a test scripts.
    """

    def __init__(
        self,
        manifest: Mapping[str, Any],
        *,
        token: str = TOKEN,
        answers: Mapping[str, dict[str, Any] | Callable[[str, dict[str, Any]], dict[str, Any]]] | None = None,
        out_of_scope: frozenset[str] = frozenset(),
    ) -> None:
        self.manifest = dict(manifest)
        self.token = token
        self.page = str(self.manifest["page"]["name"])
        self.uid = str(self.manifest["page"]["uid"])
        self.answers = dict(answers or {})
        self.out_of_scope = out_of_scope
        self.requests: list[httpx.Request] = []
        self.events: list[dict[str, Any]] = []
        self.reports: list[dict[str, Any]] = []
        self.narratives: list[dict[str, Any]] = []
        self.report_answers: list[httpx.Response] = []
        self.narrative: dict[str, Any] = {
            "target": {
                "kind": "Deployment",
                "name": "checkout",
                "namespace": "shop",
            },
            "text": "Deployment shop/checkout was OOMKilled. A memory limit increase was proposed.",
        }
        self.tool_calls = 0
        self.missing_idempotency = 0

    # Fixtures

    def refuse_report(self, *errors: tuple[str, str]) -> None:
        """Script the server's next report answer as a `422` with these validation errors."""
        body = {"errors": [{"field": field, "message": message} for field, message in errors]}
        self.report_answers.append(httpx.Response(422, json=body))

    def transport(self) -> httpx.MockTransport:
        """Return the transport to hand to an `httpx.AsyncClient`."""
        return httpx.MockTransport(self.handler)

    def tool_calls_to(self, name: str) -> int:
        """Return how many times the model called one tool."""
        return sum(1 for request in self.requests if request.url.path.endswith(f"/tools/{name}"))

    # HTTP

    def handler(self, request: httpx.Request) -> httpx.Response:
        """Serve one broker request."""
        self.requests.append(request)
        if request.headers.get("Authorization") != f"Bearer {self.token}":
            return httpx.Response(401, json={"message": "unauthenticated"})
        if request.headers.get("X-Pagerless-Page-Uid") != self.uid:
            return httpx.Response(403, json={"message": "the Page UID header disagrees with the token"})
        base = f"/v1/pages/{self.page}"
        path = request.url.path
        if request.method == "GET" and path == f"{base}/investigation":
            return httpx.Response(200, json=self.manifest)
        if request.method == "GET" and path == f"{base}/narrative":
            return httpx.Response(200, json=self.narrative)
        if request.method != "POST":
            return httpx.Response(405, json={"message": "method not allowed"})
        body = json.loads(request.content) if request.content else {}
        if path == f"{base}/events":
            self.events.append(body)
            return httpx.Response(202)
        if path == f"{base}/report":
            if self.report_answers:
                return self.report_answers.pop(0)
            self.reports.append(body)
            return httpx.Response(200, json={"accepted": True, "phase": "Proposed"})
        if path == f"{base}/narrative":
            self.narratives.append(body)
            return httpx.Response(202)
        if path.startswith(f"{base}/tools/"):
            name = path.rsplit("/", 1)[-1]
            if not request.headers.get("Idempotency-Key"):
                self.missing_idempotency += 1
                return httpx.Response(400, json={"error": {"code": "invalid_arguments", "message": "no key"}})
            return self._tool(name, body.get("arguments") or {})
        return httpx.Response(404, json={"message": "no such route"})

    def _tool(self, name: str, arguments: dict[str, Any]) -> httpx.Response:
        if name in self.out_of_scope:
            return httpx.Response(
                403,
                json={
                    "ok": False,
                    "error": {
                        "code": "out_of_scope",
                        "message": f"{name} is outside this Page's scope",
                        "field": "namespace",
                    },
                },
            )
        self.tool_calls += 1
        call_id = f"srv-{self.tool_calls}"
        answer = self.answers.get(name)
        if answer is None:
            payload: dict[str, Any] = {
                "ok": True,
                "data": {"lines": ["allocating 512 MiB", "Killed"], "container": "app", "previous": True},
                "meta": {"tool": name, "callId": call_id, "bytes": 96, "redactions": 0},
            }
        elif callable(answer):
            payload = answer(call_id, arguments)
        else:
            payload = answer
        return httpx.Response(200, json=payload)


class ScriptedProvider:
    """A provider that answers from a script, for unit tests of the graph and the budgets."""

    def __init__(self, responses: list[ModelResponse], *, estimate: int = 1000) -> None:
        self._responses = list(responses)
        self._estimate = estimate
        self.requests: list[ModelRequest] = []

    async def complete(self, request: ModelRequest, *, budget: Budget | None = None) -> ModelResponse:
        self.requests.append(request)
        if budget is not None:
            budget.spend_model_call()
        return self._responses.pop(0)

    def estimate_input_tokens(self, request: ModelRequest) -> int:
        return self._estimate


def response(
    *,
    text: str = "",
    tool_calls: tuple[Any, ...] = (),
    structured: dict[str, Any] | None = None,
    input_tokens: int = 1000,
    output_tokens: int = 100,
    estimated: bool = False,
    rung: Any = None,
) -> ModelResponse:
    """Build one scripted model response."""
    return ModelResponse(
        text=text,
        tool_calls=tool_calls,
        structured=structured,
        usage=Usage(input_tokens=input_tokens, output_tokens=output_tokens, estimated=estimated),
        rung=rung,
    )
