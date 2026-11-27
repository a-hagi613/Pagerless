"""Builders shared by the tests: settings, requests, agents and an ASGI client."""

from __future__ import annotations

import json
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from dataclasses import replace
from pathlib import Path
from typing import Any, cast

import httpx
from fastapi import FastAPI

from pagerless_agent.agent import Agent
from pagerless_agent.budget import Limits
from pagerless_agent.callback import ToolExecutor
from pagerless_agent.config import ServiceSettings
from pagerless_agent.contract import TurnRequest, TurnResult
from pagerless_agent.model import ModelProvider
from pagerless_agent.readiness import ModelProbe, ProbeResult
from pagerless_agent.server import create_app
from pagerless_agent.turns import TurnManager
from tests.fakes import (
    API_KEY,
    CALLBACK_ORIGIN,
    TOKEN,
    FakeGateway,
    FakeProvider,
    StubProbe,
)

FIXTURES = Path(__file__).parent / "fixtures"

TOOLS: list[dict[str, Any]] = [
    {
        "name": "pods_logs",
        "description": "Read the bounded logs of one pod.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["namespace", "pod"],
            "properties": {
                "namespace": {"type": "string"},
                "pod": {"type": "string"},
                "previous": {"type": "boolean"},
            },
        },
    },
    {
        "name": "resources_get",
        "description": "Read one Kubernetes object of an allowed kind.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["kind", "name"],
            "properties": {
                "kind": {"type": "string"},
                "name": {"type": "string"},
                "namespace": {"type": "string"},
            },
        },
    },
    {
        "name": "repo_read",
        "description": "Read one file from the configured source repository.",
        "inputSchema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["path"],
            "properties": {"path": {"type": "string"}},
        },
    },
]

QUESTION = "Why is checkout timing out?"


def settings(tmp_path: Path, **overrides: Any) -> ServiceSettings:
    """Return validated settings whose credentials are files under tmp_path."""
    token_file = tmp_path / "internal-token"
    token_file.write_text(TOKEN + "\n", encoding="utf-8")
    key_file = tmp_path / "model-key"
    key_file.write_text(API_KEY + "\n", encoding="utf-8")
    env = {
        "PAGERLESS_INTERNAL_TOKEN_FILE": str(token_file),
        "PAGERLESS_CALLBACK_ORIGIN": CALLBACK_ORIGIN,
        "PAGERLESS_AGENT_MAX_CONCURRENCY": "4",
        "PAGERLESS_MODEL_BASE_URL": "http://mock-openai.pagerless.svc/v1",
        "PAGERLESS_MODEL_NAME": "mock-model",
        "PAGERLESS_MODEL_API_KEY_FILE": str(key_file),
        "PAGERLESS_MODEL_CONTEXT_TOKENS": "32768",
        "PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT": "1",
        "PAGERLESS_BUDGET_TOOL_CALLS": "6",
        "PAGERLESS_BUDGET_MODEL_CALLS": "6",
        "PAGERLESS_BUDGET_INPUT_TOKENS": "300000",
        "PAGERLESS_BUDGET_SECONDS": "60",
        "PAGERLESS_AGENT_TOOL_TIMEOUT_S": "5",
    }
    env.update({key: str(value) for key, value in overrides.items()})
    return ServiceSettings.from_env(env)


def request_body(
    *,
    page_id: str = "p1",
    turn_id: str = "t1",
    message: str = QUESTION,
    messages: list[dict[str, Any]] | None = None,
    tools: list[dict[str, Any]] | None = None,
    namespace: str | None = "shop",
    target: dict[str, Any] | None = None,
    limits: dict[str, Any] | None = None,
    callback_url: str = CALLBACK_ORIGIN,
) -> dict[str, Any]:
    """Return a POST /turn body with camelCase names."""
    return {
        "pageId": page_id,
        "turnId": turn_id,
        "messages": messages if messages is not None else [{"role": "user", "content": message}],
        "namespace": namespace,
        "target": target if target is not None else {"kind": "Deployment", "name": "checkout"},
        "tools": tools if tools is not None else list(TOOLS),
        "callbackUrl": callback_url,
        **({"limits": limits} if limits is not None else {}),
    }


def turn_request(**kwargs: Any) -> TurnRequest:
    """Return a validated request from the builder."""
    return TurnRequest.model_validate(request_body(**kwargs))


def budgets(settings: ServiceSettings, request: TurnRequest | None = None) -> Limits:
    """Return the limits the manager would use."""
    configured = settings.budgets
    requested = request.limits if request is not None else None
    if requested is None:
        return Limits(
            tool_calls=configured.tool_calls,
            model_calls=configured.model_calls,
            input_tokens=configured.input_tokens,
            seconds=configured.seconds,
        )
    return Limits(
        tool_calls=min(requested.tool_calls or configured.tool_calls, configured.tool_calls),
        model_calls=min(requested.model_calls or configured.model_calls, configured.model_calls),
        input_tokens=min(requested.input_tokens or configured.input_tokens, configured.input_tokens),
        seconds=min(requested.seconds or configured.seconds, configured.seconds),
    )


def sequence(prefix: str = "c") -> Callable[[], str]:
    """Return a deterministic call-id factory."""
    counter = 0

    def next_id() -> str:
        nonlocal counter
        counter += 1
        return f"{prefix}{counter}"

    return next_id


def build_agent(
    settings: ServiceSettings,
    provider: ModelProvider,
    gateway: ToolExecutor,
    request: TurnRequest | None = None,
    *,
    call_ids: Callable[[], str] | None = None,
    tool_timeout_s: float = 5.0,
) -> Agent:
    """Return an Agent wired to the fakes."""
    selected = request or turn_request()
    return Agent(
        request=selected,
        provider=provider,
        gateway=gateway,
        limits=budgets(settings, selected),
        tool_timeout_s=tool_timeout_s,
        context_tokens=settings.model.context_tokens,
        max_output_tokens=settings.model.max_output_tokens,
        nonce="run-nonce-0123456789abcdef",
        call_id_factory=call_ids or sequence(),
    )


async def run_turn(
    settings: ServiceSettings,
    provider: FakeProvider,
    gateway: ToolExecutor,
    request: TurnRequest | None = None,
    *,
    call_ids: Callable[[], str] | None = None,
    tool_timeout_s: float = 5.0,
) -> TurnResult:
    """Run one turn through the real graph."""
    agent = build_agent(
        settings,
        provider,
        gateway,
        request,
        call_ids=call_ids,
        tool_timeout_s=tool_timeout_s,
    )
    return await agent.run()


@asynccontextmanager
async def app_client(
    settings: ServiceSettings,
    *,
    provider: FakeProvider,
    gateway: FakeGateway,
    probe: StubProbe | None = None,
    manager: TurnManager | None = None,
) -> AsyncIterator[tuple[FastAPI, httpx.AsyncClient]]:
    """Return an ASGI client for the service with injected fakes."""
    selected_probe = cast(
        ModelProbe, probe or StubProbe(ProbeResult(True, "model tool-call probe succeeded"))
    )
    app = create_app(settings, provider=provider, gateway=gateway, probe=selected_probe, manager=manager)
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://agent") as client:
        yield app, client


def auth_headers(token: str = TOKEN) -> dict[str, str]:
    """Return the internal bearer header."""
    return {"Authorization": f"Bearer {token}"}


def contract_fixture(name: str) -> dict[str, Any]:
    """Return one checked-in wire fixture."""
    return cast(dict[str, Any], json.loads((FIXTURES / "contract" / name).read_text(encoding="utf-8")))


def replace_settings(settings: ServiceSettings, **changes: Any) -> ServiceSettings:
    """Return a copy with one field changed."""
    return replace(settings, **changes)
