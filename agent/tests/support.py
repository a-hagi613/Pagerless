"""Builders shared by the agent tests: a manifest, settings, and a provider on the mock."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any
from uuid import uuid4

import httpx
import httpx2
from openai import AsyncOpenAI

from pagerless_agent.agent import Agent, Outcome
from pagerless_agent.config import Settings
from pagerless_agent.openai_compat import OpenAICompatibleProvider
from pagerless_agent.runtime import run
from pagerless_agent.structured import LadderCache
from pagerless_agent.toolclient import ToolClient
from tests.fakes import StandInBroker
from tests.mock_openai import MockOpenAI, Step

FIXTURES = Path(__file__).parent / "fixtures"
PAGE = "checkout-7f2c"
TOKEN = "projected-token-1"
API_KEY = "test-model-key"

DEFAULT_TOOLS: list[dict[str, Any]] = [
    {
        "name": "pods_logs",
        "description": "Read a pod's logs. Use previous=true first for a crash loop.",
        "input_schema": {
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
        "name": "object_get",
        "description": "Read one Kubernetes object.",
        "input_schema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["apiVersion", "kind", "namespace", "name"],
            "properties": {
                "apiVersion": {"type": "string"},
                "kind": {"type": "string"},
                "namespace": {"type": "string"},
                "name": {"type": "string"},
            },
        },
    },
    {
        "name": "repo_file",
        "description": "Read one file from the Target Repository.",
        "input_schema": {
            "type": "object",
            "additionalProperties": False,
            "required": ["path"],
            "properties": {"path": {"type": "string"}},
        },
    },
]


def fixture(*parts: str) -> Path:
    """Return the path of one checked-in test fixture."""
    return FIXTURES.joinpath(*parts)


def investigation_fixture() -> dict[str, Any]:
    """Return the OOMKilled investigation fixture as a dictionary."""
    document: dict[str, Any] = json.loads(fixture("investigation.json").read_text(encoding="utf-8"))
    return document


_OOM = MockOpenAI.from_fixture(fixture("scripts", "oom.json"))
# The scripted investigation, in the order the graph consumes it.
PREAMBLE: list[Step] = list(_OOM.steps[:3])
DIAGNOSIS: dict[str, Any] = _OOM.steps[3].payload or {}
PROPOSAL: dict[str, Any] = _OOM.steps[4].payload or {}
assert DIAGNOSIS and PROPOSAL


def script(
    *,
    investigate_steps: list[Step] | None = None,
    diagnosis_steps: list[Step] | None = None,
    proposal_steps: list[Step] | None = None,
    profile: str = "strict",
) -> MockOpenAI:
    """Return one scripted investigation, with any of its three stages replaced."""
    return MockOpenAI(
        [
            *(PREAMBLE if investigate_steps is None else investigate_steps),
            *([Step(payload=DIAGNOSIS)] if diagnosis_steps is None else diagnosis_steps),
            *([Step(payload=PROPOSAL)] if proposal_steps is None else proposal_steps),
        ],
        profile=profile,  # type: ignore[arg-type]
    )


def corrupted(payload: dict[str, Any], path: str, value: object) -> dict[str, Any]:
    """Return a deep copy of a payload with one JSON Pointer set to a value."""
    copy: dict[str, Any] = json.loads(json.dumps(payload))
    node: Any = copy
    tokens = path.lstrip("/").split("/")
    for token in tokens[:-1]:
        node = node[int(token)] if isinstance(node, list) else node[token]
    last = tokens[-1]
    if isinstance(node, list):
        node[int(last)] = value
    else:
        node[last] = value
    return copy


def manifest(
    *,
    uid: str | None = None,
    page: str = PAGE,
    context: dict[str, Any] | None = None,
    hint: str | None = None,
    tools: list[dict[str, Any]] | None = None,
    limits: dict[str, Any] | None = None,
    repositories: list[dict[str, str]] | None = None,
) -> dict[str, Any]:
    """Return one investigation manifest, as `GET /v1/pages/{page}/investigation` serves it."""
    source = investigation_fixture()
    return {
        "page": {"name": page, "uid": uid or str(uuid4()), "namespace": "pagerless"},
        "target": {
            "kind": "Deployment",
            "name": "checkout",
            "namespace": "shop",
            "uid": "0b3f4a2c-1111-2222-3333-444455556666",
            "generation": 42,
        },
        "trigger": {"kind": "Watcher", "reason": "CrashLoopBackOff"},
        "hint": hint if hint is not None else source["hint"],
        "scope": {
            "namespaces": ["shop"],
            "repositories": repositories
            if repositories is not None
            else [{"owner": "acme", "name": "gitops", "ref": "main"}],
            "connectors": ["loki-prod"],
            "kinds": ["Deployment", "Pod", "Event", "ReplicaSet"],
        },
        "context": context if context is not None else source["context"],
        "tools": tools if tools is not None else list(DEFAULT_TOOLS),
        "limits": limits if limits is not None else dict(source["limits"]),
        "capabilities": {"structuredOutput": "unknown"},
    }


def settings(
    tmp_path: Path,
    *,
    page: str = PAGE,
    uid: str | None = None,
    tool_calls: int = 15,
    model_calls: int = 12,
    input_tokens: int = 300_000,
    seconds: float = 300.0,
    context_tokens: int = 32_768,
    structured_output: str = "auto",
    task: str = "investigate",
) -> Settings:
    """Return settings for one Job, with the token and model key written to files."""
    token_file = tmp_path / "token"
    token_file.write_text(TOKEN + "\n", encoding="utf-8")
    key_file = tmp_path / "model-key"
    key_file.write_text(API_KEY + "\n", encoding="utf-8")
    env = {
        "PAGERLESS_TASK": task,
        "PAGERLESS_SERVER_URL": "https://pagerless-server.pagerless.svc:8443",
        "PAGERLESS_TOKEN_FILE": str(token_file),
        "PAGERLESS_PAGE": page,
        "PAGERLESS_PAGE_UID": uid or str(uuid4()),
        "PAGERLESS_MODEL_BASE_URL": "http://mock-openai.pagerless.svc/v1",
        "PAGERLESS_MODEL_NAME": "mock-model",
        "PAGERLESS_MODEL_API_KEY_FILE": str(key_file),
        "PAGERLESS_MODEL_CONTEXT_TOKENS": str(context_tokens),
        "PAGERLESS_MODEL_STRUCTURED_OUTPUT": structured_output,
        "PAGERLESS_ALLOW_INSECURE_MODEL_ENDPOINT": "1",
        "PAGERLESS_BUDGET_TOOL_CALLS": str(tool_calls),
        "PAGERLESS_BUDGET_MODEL_CALLS": str(model_calls),
        "PAGERLESS_BUDGET_INPUT_TOKENS": str(input_tokens),
        "PAGERLESS_BUDGET_SECONDS": str(seconds),
    }
    return Settings.from_env(env)


def provider(
    settings: Settings,
    mock: MockOpenAI,
    *,
    cache: LadderCache | None = None,
    http: httpx2.AsyncClient | None = None,
) -> OpenAICompatibleProvider:
    """Return the real provider wired to the fixture-backed mock server."""
    transport_client = http or httpx2.AsyncClient(transport=mock.transport())
    sdk = AsyncOpenAI(
        base_url=settings.model.base_url,
        api_key=settings.model.api_key,
        max_retries=0,
        http_client=transport_client,
    )
    return OpenAICompatibleProvider(settings.model, client=sdk, cache=cache or LadderCache())


def tool_client(settings: Settings, broker: StandInBroker) -> ToolClient:
    """Return a tool broker client wired to the stand-in server."""
    http = httpx.AsyncClient(transport=broker.transport())
    return ToolClient(
        settings.server_url,
        settings.page,
        settings.page_uid,
        settings.token_file,
        http=http,
    )


async def investigate(
    settings: Settings,
    mock: MockOpenAI,
    broker: StandInBroker,
    *,
    provider_cache: LadderCache | None = None,
) -> Outcome:
    """Run one investigation against the mock provider and the stand-in broker."""
    selected = provider(settings, mock, cache=provider_cache)
    client = tool_client(settings, broker)
    try:
        return await run(settings, provider=selected, client=client)
    finally:
        await client.aclose()


def agent(
    settings: Settings,
    mock: MockOpenAI,
    broker: StandInBroker,
    *,
    cache: LadderCache | None = None,
) -> Agent:
    """Return an Agent wired to the mocks, for tests that inspect graph internals."""
    return Agent(
        settings=settings,
        provider=provider(settings, mock, cache=cache),
        client=tool_client(settings, broker),
    )
