"""The OpenAI-compatible provider over a deterministic HTTP transport."""

from __future__ import annotations

import json
from collections.abc import Callable
from typing import Any, cast

import httpx
import pytest
from openai import AsyncOpenAI

from pagerless_agent.budget import Budget, BudgetExhaustedError, Limits
from pagerless_agent.config import ModelSettings
from pagerless_agent.model import Message, ModelRequest, ToolSpec
from pagerless_agent.openai_compat import (
    OpenAICompatibleProvider,
    ProviderAuthError,
    ProviderMalformedError,
    ProviderRejectedError,
    ProviderTimeoutError,
    ProviderTruncatedError,
    ProviderUnavailableError,
)
from tests.fakes import API_KEY

TOOL = ToolSpec(
    name="resources_get",
    description="Read one object.",
    input_schema={"type": "object", "properties": {"kind": {"type": "string"}}},
)


def make_provider(
    handler: Callable[[httpx.Request], httpx.Response],
    *,
    read_timeout: float = 2.0,
) -> tuple[OpenAICompatibleProvider, list[dict[str, Any]]]:
    bodies: list[dict[str, Any]] = []

    def wrapped(request: httpx.Request) -> httpx.Response:
        bodies.append(json.loads(request.content))
        return handler(request)

    settings = ModelSettings(
        base_url="https://models.example/v1",
        api_key=API_KEY,
        model="mock-model",
        max_output_tokens=4096,
        context_tokens=32_768,
        connect_timeout_s=1.0,
        read_timeout_s=read_timeout,
    )
    client = AsyncOpenAI(
        base_url=settings.base_url,
        api_key=settings.api_key,
        max_retries=0,
        http_client=cast(Any, httpx.AsyncClient(transport=httpx.MockTransport(wrapped))),
    )
    return OpenAICompatibleProvider(settings, client=client), bodies


def completion(
    *,
    content: str | None = "ok",
    tool_calls: list[dict[str, Any]] | None = None,
    usage: dict[str, Any] | None = None,
    finish_reason: str = "stop",
) -> httpx.Response:
    message: dict[str, Any] = {"role": "assistant", "content": content}
    if tool_calls is not None:
        message["tool_calls"] = tool_calls
    body: dict[str, Any] = {
        "choices": [{"message": message, "finish_reason": finish_reason}],
    }
    if usage is not None:
        body["usage"] = usage
    return httpx.Response(200, json=body)


def tool_call(
    identifier: str,
    name: str,
    arguments: str,
) -> dict[str, Any]:
    return {
        "id": identifier,
        "type": "function",
        "function": {"name": name, "arguments": arguments},
    }


async def test_request_shape_and_tool_call_parsing() -> None:
    provider, bodies = make_provider(
        lambda _: completion(
            content=None,
            tool_calls=[tool_call("call_1", "resources_get", '{"kind": "Node"}')],
            usage={"prompt_tokens": 11, "completion_tokens": 3},
        )
    )
    crossed = await provider.complete(
        ModelRequest(
            system="You investigate.",
            messages=(Message(role="user", content="Why?"),),
            tools=(TOOL,),
        ),
        budget=Budget(limits=Limits(model_calls=5, input_tokens=1_000_000)),
    )
    assert bodies[0]["model"] == "mock-model"
    assert bodies[0]["messages"][0] == {"role": "system", "content": "You investigate."}
    assert bodies[0]["messages"][1] == {"role": "user", "content": "Why?"}
    assert bodies[0]["tools"][0]["function"]["name"] == "resources_get"
    assert bodies[0]["tool_choice"] == "auto"
    assert bodies[0]["parallel_tool_calls"] is False
    assert bodies[0]["temperature"] == 0.0
    assert bodies[0]["max_tokens"] == 4096
    assert crossed.tool_calls[0].name == "resources_get"
    assert crossed.tool_calls[0].arguments == {"kind": "Node"}
    assert crossed.usage.input_tokens == 11
    assert crossed.usage.output_tokens == 3
    assert crossed.usage.estimated is False


async def test_forced_tool_choice_is_used_for_the_probe() -> None:
    provider, bodies = make_provider(
        lambda _: completion(
            content=None,
            tool_calls=[tool_call("probe-1", "readiness_probe", "{}")],
        )
    )
    probe_tool = ToolSpec(
        name="readiness_probe",
        description="Probe.",
        input_schema={"type": "object", "properties": {}},
    )
    response = await provider.complete(
        ModelRequest(
            system="probe",
            messages=(Message(role="user", content="call it"),),
            tools=(probe_tool,),
            forced_tool="readiness_probe",
            purpose="readiness",
        )
    )
    assert bodies[0]["tool_choice"] == {
        "type": "function",
        "function": {"name": "readiness_probe"},
    }
    assert response.tool_calls[0].name == "readiness_probe"


async def test_missing_usage_is_estimated() -> None:
    provider, _ = make_provider(lambda _: completion())
    crossed = await provider.complete(
        ModelRequest(
            system="system",
            messages=(Message(role="user", content="question " * 100),),
            tools=(TOOL,),
        )
    )
    assert crossed.usage.estimated is True
    assert crossed.usage.input_tokens > 0


async def test_list_content_is_joined() -> None:
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "choices": [
                    {
                        "message": {
                            "role": "assistant",
                            "content": [{"type": "text", "text": "one "}, {"type": "text", "text": "two"}],
                        },
                        "finish_reason": "stop",
                    }
                ]
            },
        )

    provider, _ = make_provider(handler)
    crossed = await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))
    assert crossed.text == "one two"


async def test_arguments_are_repaired_and_bad_arguments_are_correctable() -> None:
    provider, _ = make_provider(
        lambda _: completion(
            content=None,
            tool_calls=[
                tool_call("a", "resources_get", '```json\n{"kind": "Node",}\n```'),
                tool_call("b", "resources_get", "not json"),
                tool_call("a", "resources_get", "{}"),
            ],
        )
    )
    crossed = await provider.complete(
        ModelRequest(system="s", messages=(Message(role="user", content="q"),), tools=(TOOL,))
    )
    assert [call.id for call in crossed.tool_calls] == ["a", "b", "a_2"]
    assert crossed.tool_calls[0].arguments == {"kind": "Node"}
    assert crossed.tool_calls[1].argument_error is not None
    assert crossed.tool_calls[2].argument_error is None


@pytest.mark.parametrize(
    "response",
    [
        httpx.Response(200, content=b"not json", headers={"content-type": "application/json"}),
        httpx.Response(200, json={"choices": []}),
        httpx.Response(200, json={"choices": "x"}),
        httpx.Response(200, json={"choices": [{"message": "x"}]}),
        httpx.Response(200, json={"choices": [None]}),
    ],
)
async def test_malformed_provider_bodies_are_visible(response: httpx.Response) -> None:
    provider, _ = make_provider(lambda _: response)
    with pytest.raises(ProviderMalformedError):
        await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))


async def test_an_error_body_without_choices_is_rejected() -> None:
    provider, _ = make_provider(
        lambda _: httpx.Response(200, json={"error": {"message": "bad request from the gateway"}})
    )
    with pytest.raises(ProviderRejectedError, match="bad request"):
        await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))


async def test_auth_failure_is_sanitized() -> None:
    provider, _ = make_provider(
        lambda _: httpx.Response(401, json={"error": {"message": f"bad key {API_KEY}"}})
    )
    with pytest.raises(ProviderAuthError) as caught:
        await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))
    assert API_KEY not in str(caught.value)


async def test_temperature_is_dropped_when_rejected() -> None:
    calls = {"count": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["count"] += 1
        if calls["count"] == 1:
            return httpx.Response(
                400,
                json={"error": {"message": "temperature is not supported for this model"}},
            )
        return completion()

    provider, bodies = make_provider(handler)
    budget = Budget(limits=Limits(model_calls=5, input_tokens=1_000_000))
    crossed = await provider.complete(
        ModelRequest(system="s", messages=(Message(role="user", content="q"),)), budget=budget
    )
    assert crossed.model_calls == 2
    assert "temperature" in bodies[0]
    assert "temperature" not in bodies[1]
    assert budget.model_calls == 2


async def test_max_tokens_is_renamed_when_rejected() -> None:
    calls = {"count": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["count"] += 1
        if calls["count"] == 1:
            return httpx.Response(400, json={"error": {"message": "max_tokens is not supported"}})
        return completion()

    provider, bodies = make_provider(handler)
    await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))
    assert "max_tokens" in bodies[0]
    assert "max_tokens" not in bodies[1]
    assert bodies[1]["max_completion_tokens"] == 4096


async def test_parallel_tool_calls_is_dropped_when_rejected() -> None:
    calls = {"count": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["count"] += 1
        if calls["count"] == 1:
            return httpx.Response(
                400,
                json={"error": {"message": "parallel_tool_calls is not supported"}},
            )
        return completion()

    provider, bodies = make_provider(handler)
    await provider.complete(
        ModelRequest(system="s", messages=(Message(role="user", content="q"),), tools=(TOOL,))
    )
    assert "parallel_tool_calls" in bodies[0]
    assert "parallel_tool_calls" not in bodies[1]


async def test_a_rejected_forced_tool_choice_falls_back_to_auto() -> None:
    calls = {"count": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["count"] += 1
        if calls["count"] == 1:
            return httpx.Response(400, json={"error": {"message": "tool_choice is not supported"}})
        return completion(
            content=None,
            tool_calls=[tool_call("probe-1", "readiness_probe", "{}")],
        )

    provider, bodies = make_provider(handler)
    probe_tool = ToolSpec(
        name="readiness_probe",
        description="Probe.",
        input_schema={"type": "object", "properties": {}},
    )
    response = await provider.complete(
        ModelRequest(
            system="probe",
            messages=(Message(role="user", content="call it"),),
            tools=(probe_tool,),
            forced_tool="readiness_probe",
            purpose="readiness",
        )
    )
    assert "tool_choice" in bodies[0]
    assert bodies[1]["tool_choice"] == "auto"
    assert response.tool_calls[0].name == "readiness_probe"


async def test_tool_calling_unsupported_is_distinct() -> None:
    provider, _ = make_provider(
        lambda _: httpx.Response(400, json={"error": {"message": "tools are not supported"}})
    )
    with pytest.raises(ProviderRejectedError) as caught:
        await provider.complete(
            ModelRequest(system="s", messages=(Message(role="user", content="q"),), tools=(TOOL,))
        )
    assert caught.value.reason == "tools_unsupported"


async def test_one_server_error_is_retried() -> None:
    calls = {"count": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["count"] += 1
        return httpx.Response(503) if calls["count"] == 1 else completion()

    provider, _ = make_provider(handler)
    crossed = await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))
    assert crossed.text == "ok"
    assert crossed.model_calls == 2


async def test_repeated_server_errors_are_visible() -> None:
    provider, _ = make_provider(lambda _: httpx.Response(503))
    with pytest.raises(ProviderUnavailableError):
        await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))


async def test_readiness_does_not_retry_transport_failures() -> None:
    provider, _ = make_provider(lambda _: httpx.Response(503))
    with pytest.raises(ProviderUnavailableError):
        await provider.complete(
            ModelRequest(
                system="s",
                messages=(Message(role="user", content="q"),),
                purpose="readiness",
            )
        )


async def test_timeout_is_visible_after_bounded_retry() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("slow", request=request)

    provider, _ = make_provider(handler)
    with pytest.raises(ProviderTimeoutError):
        await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))


async def test_provider_message_cannot_leak_the_api_key() -> None:
    provider, _ = make_provider(
        lambda _: httpx.Response(400, json={"error": {"message": f"reject {API_KEY}"}})
    )
    with pytest.raises(ProviderRejectedError) as caught:
        await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))
    assert API_KEY not in str(caught.value)
    assert "redacted" in str(caught.value)


async def test_empty_truncated_answer_is_visible() -> None:
    provider, _ = make_provider(lambda _: completion(content="", finish_reason="length"))
    with pytest.raises(ProviderTruncatedError):
        await provider.complete(ModelRequest(system="s", messages=(Message(role="user", content="q"),)))


async def test_retry_stops_when_the_model_budget_is_spent() -> None:
    provider, _ = make_provider(lambda _: httpx.Response(503))
    budget = Budget(limits=Limits(model_calls=1, input_tokens=1_000_000))
    with pytest.raises(BudgetExhaustedError):
        await provider.complete(
            ModelRequest(system="s", messages=(Message(role="user", content="q"),)), budget=budget
        )


def test_estimate_includes_the_output_reserve() -> None:
    provider, _ = make_provider(lambda _: completion())
    request = ModelRequest(
        system="system",
        messages=(Message(role="user", content="question"),),
        tools=(TOOL,),
        max_output_tokens=512,
    )
    assert provider.estimate_input_tokens(request) > provider.estimate_prompt_tokens(request)
