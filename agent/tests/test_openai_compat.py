"""The OpenAI-compatible client: request normalisation, response tolerance, and the ladder."""

from __future__ import annotations

import json
from typing import Any

import httpx2
import pytest
from openai import AsyncOpenAI

from pagerless_agent.budget import Budget, BudgetExhaustedError, Limits
from pagerless_agent.config import ModelSettings
from pagerless_agent.model import Message, ModelRequest, Stage, ToolCall, ToolSpec
from pagerless_agent.openai_compat import (
    EMPTY_TOOL_RESULT,
    SUBMIT_OUTPUT,
    OpenAICompatibleProvider,
    ProviderAuthError,
    ProviderRejectedError,
    ProviderTruncatedError,
)
from pagerless_agent.structured import Rung
from tests.mock_openai import MockOpenAI, Profile, Step

SCHEMA: dict[str, Any] = {
    "type": "object",
    "additionalProperties": False,
    "required": ["answer"],
    "properties": {"answer": {"type": "string", "pattern": "^[a-z]+$"}},
}

TOOLS = (
    ToolSpec(
        name="pods_logs",
        description="Read pod logs.",
        input_schema={"type": "object", "properties": {"pod": {"type": "string"}}},
    ),
)


def model_settings(**overrides: Any) -> ModelSettings:
    """Return model settings that point at the in-process mock."""
    values: dict[str, Any] = {
        "base_url": "http://mock-openai.test/v1",
        "api_key": "sk-test-secret",
        "model": "mock-model",
    }
    values.update(overrides)
    return ModelSettings(**values)


def build(
    settings: ModelSettings | None = None,
    mock: MockOpenAI | None = None,
) -> tuple[OpenAICompatibleProvider, MockOpenAI]:
    """Return a provider wired to the mock server."""
    selected = settings or model_settings()
    server = mock or MockOpenAI([])
    http = httpx2.AsyncClient(transport=server.transport())
    sdk = AsyncOpenAI(
        base_url=selected.base_url,
        api_key=selected.api_key,
        max_retries=0,
        http_client=http,
    )
    return OpenAICompatibleProvider(selected, client=sdk), server


def request(
    *,
    stage: Stage = Stage.INVESTIGATE,
    messages: tuple[Message, ...] | None = None,
    tools: tuple[ToolSpec, ...] = (),
    schema: dict[str, Any] | None = None,
    max_output_tokens: int | None = None,
) -> ModelRequest:
    """Return one model request."""
    return ModelRequest(
        stage=stage,
        system="You are the agent.",
        messages=messages if messages is not None else (Message(role="user", content="Go."),),
        tools=tools,
        output_schema=schema,
        max_output_tokens=max_output_tokens,
    )


async def test_one_system_message_first_and_no_reasoning_fields_are_echoed() -> None:
    mock = MockOpenAI(
        [
            Step(content="first answer", reasoning_content="private chain of thought", usage=None),
            Step(content="second answer"),
        ]
    )
    provider, _ = build(mock=mock)

    first = await provider.complete(request())
    second = await provider.complete(
        request(
            messages=(
                Message(role="system", content="a stray system message"),
                Message(role="user", content="Go."),
                Message(role="assistant", content=first.text),
                Message(role="tool", content="", tool_call_id="call_1"),
            )
        )
    )

    assert second.text == "second answer"
    body = mock.requests[1]
    roles = [message["role"] for message in body["messages"]]
    assert roles.count("system") == 1
    assert roles[0] == "system"
    assert "a stray system message" in body["messages"][0]["content"]
    assert "developer" not in roles
    # The provider's private reasoning is never sent back.
    assert all("reasoning_content" not in message for message in body["messages"])
    # An empty tool result is replaced with a typed value several providers accept.
    tool_message = next(message for message in body["messages"] if message["role"] == "tool")
    assert tool_message["content"] == EMPTY_TOOL_RESULT


async def test_the_request_carries_the_dialect_switches() -> None:
    mock = MockOpenAI([Step(content="fine")])
    provider, _ = build(mock=mock)

    await provider.complete(request(tools=TOOLS, max_output_tokens=321))

    body = mock.requests[0]
    assert body["model"] == "mock-model"
    assert body["max_tokens"] == 321
    assert body["temperature"] == 0.0
    assert body["parallel_tool_calls"] is False
    assert body["stream"] is False
    assert body["tool_choice"] == "auto"
    assert body["tools"][0]["function"]["name"] == "pods_logs"
    assert "response_format" not in body


async def test_a_missing_usage_is_estimated_and_marked() -> None:
    mock = MockOpenAI([Step(content="an answer without usage", usage=None)])
    provider, _ = build(mock=mock)

    response = await provider.complete(request())

    assert response.usage.estimated is True
    assert response.usage.input_tokens > 0
    assert response.usage.output_tokens > 0
    assert provider.estimate_input_tokens(request()) > 0


async def test_a_200_with_an_error_body_is_a_provider_error() -> None:
    class ErrorBody(MockOpenAI):
        def handler(self, request: httpx2.Request) -> httpx2.Response:
            return httpx2.Response(200, json={"error": {"message": "the model is overloaded"}})

    provider, _ = build(mock=ErrorBody([]))

    with pytest.raises(ProviderRejectedError, match="overloaded"):
        await provider.complete(request())


async def test_no_choices_and_an_invalid_body_are_provider_errors() -> None:
    class NoChoices(MockOpenAI):
        def handler(self, request: httpx2.Request) -> httpx2.Response:
            return httpx2.Response(200, json={"choices": []})

    class NotJson(MockOpenAI):
        def handler(self, request: httpx2.Request) -> httpx2.Response:
            return httpx2.Response(200, text="<html>gateway</html>")

    for cls in (NoChoices, NotJson):
        provider, _ = build(mock=cls([]))
        with pytest.raises(ProviderRejectedError):
            await provider.complete(request())


async def test_extra_choices_take_the_first() -> None:
    class TwoChoices(MockOpenAI):
        def handler(self, request: httpx2.Request) -> httpx2.Response:
            message = {"role": "assistant", "content": "first"}
            return httpx2.Response(
                200,
                json={
                    "choices": [
                        {"index": 0, "finish_reason": "stop", "message": message},
                        {
                            "index": 1,
                            "finish_reason": "stop",
                            "message": {"role": "assistant", "content": "second"},
                        },
                    ],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1},
                },
            )

    provider, _ = build(mock=TwoChoices([]))
    response = await provider.complete(request())

    assert response.text == "first"


@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        ("", {}),
        ("   ", {}),
        ('{"a": 1}', {"a": 1}),
        ('```json\n{"a": 1}\n```', {"a": 1}),
        ('{"a": 1,}', {"a": 1}),
        ('"{\\"a\\": 1}"', {"a": 1}),
        ("[1, 2]", None),
        ("not json", None),
    ],
)
async def test_tool_arguments_are_repaired_or_returned_as_a_correctable_error(
    raw: str, expected: dict[str, Any] | None
) -> None:
    mock = MockOpenAI([Step(tool_calls=({"name": "pods_logs", "arguments": raw},))])
    provider, _ = build(mock=mock)

    response = await provider.complete(request())
    call = response.tool_calls[0]

    if expected is None:
        assert call.argument_error is not None
        assert call.arguments == {}
    else:
        assert call.argument_error is None
        assert call.arguments == expected


async def test_a_legacy_function_call_becomes_one_tool_call() -> None:
    class Legacy(MockOpenAI):
        def handler(self, request: httpx2.Request) -> httpx2.Response:
            return httpx2.Response(
                200,
                json={
                    "choices": [
                        {
                            "index": 0,
                            "finish_reason": "function_call",
                            "message": {
                                "role": "assistant",
                                "content": None,
                                "function_call": {"name": "pods_logs", "arguments": '{"pod": "p"}'},
                            },
                        }
                    ],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1},
                },
            )

    provider, _ = build(mock=Legacy([]))
    response = await provider.complete(request())

    assert response.text == ""
    assert response.tool_calls == (ToolCall(id="call_0", name="pods_logs", arguments={"pod": "p"}),)


async def test_a_rejected_credential_is_reported_without_the_key() -> None:
    class Unauthorized(MockOpenAI):
        def handler(self, request: httpx2.Request) -> httpx2.Response:
            return httpx2.Response(401, json={"error": {"message": "invalid api key sk-test-secret"}})

    provider, _ = build(mock=Unauthorized([]))

    with pytest.raises(ProviderAuthError) as raised:
        await provider.complete(request())
    assert "mock-openai.test" in str(raised.value)
    assert "sk-test-secret" not in str(raised.value)


async def test_a_rate_limit_retries_and_counts_each_call() -> None:
    mock = MockOpenAI(
        [
            Step(status=429, error="slow down"),
            Step(content="recovered"),
        ]
    )
    provider, _ = build(mock=mock)
    budget = Budget(limits=Limits(model_calls=5))

    response = await provider.complete(request(), budget=budget)

    assert response.text == "recovered"
    assert response.model_calls == 2
    assert budget.model_calls == 2


async def test_transport_retries_stop_when_the_model_call_budget_is_spent() -> None:
    mock = MockOpenAI([Step(status=429), Step(status=429), Step(status=429)])
    provider, _ = build(mock=mock)
    budget = Budget(limits=Limits(model_calls=2))

    with pytest.raises(BudgetExhaustedError, match="model calls"):
        await provider.complete(request(), budget=budget)


@pytest.mark.parametrize("profile", ["strict", "json_object", "tool", "tool_without_choice", "text"])
async def test_every_ladder_profile_reaches_a_valid_structured_answer(profile: Profile) -> None:
    mock = MockOpenAI([Step(payload={"answer": "done"})], profile=profile)
    provider, _ = build(mock=mock)

    response = await provider.complete(request(stage=Stage.DIAGNOSE, schema=SCHEMA))

    assert response.structured == {"answer": "done"}
    expected = {
        "strict": Rung.JSON_SCHEMA,
        "json_object": Rung.JSON_OBJECT,
        "tool": Rung.TOOL,
        "tool_without_choice": Rung.TOOL,
        "text": Rung.TEXT,
    }[profile]
    assert response.rung is expected
    # The successful rung was recorded, so the next call at this stage starts there.
    key = (provider.settings.base_url, provider.settings.model, "diagnose")
    assert provider.cache.cached(key) is expected


async def test_a_rejected_response_format_then_a_rejected_tool_choice_still_yields_a_proposal() -> None:
    mock = MockOpenAI([Step(payload={"answer": "done"})], profile="tool_without_choice")
    provider, _ = build(mock=mock)

    response = await provider.complete(request(stage=Stage.PROPOSE, schema=SCHEMA))

    assert response.structured == {"answer": "done"}
    assert response.rung is Rung.TOOL
    formats = [request_body.get("response_format", {}).get("type") for request_body in mock.requests]
    assert formats == ["json_schema", "json_object", None, None]
    assert mock.requests[-1]["tools"][0]["function"]["name"] == SUBMIT_OUTPUT
    assert mock.requests[-1]["tool_choice"] == "auto"


async def test_the_cached_rung_is_used_on_the_next_call_without_another_downgrade() -> None:
    mock = MockOpenAI([Step(payload={"answer": "one"}), Step(payload={"answer": "two"})], profile="text")
    provider, _ = build(mock=mock)

    first = await provider.complete(request(stage=Stage.DIAGNOSE, schema=SCHEMA))
    second = await provider.complete(request(stage=Stage.DIAGNOSE, schema=SCHEMA))

    assert first.rung is Rung.TEXT
    assert second.rung is Rung.TEXT
    # The second call goes straight to the text rung: no response_format, no tools.
    assert "response_format" not in mock.requests[-1]
    assert "tools" not in mock.requests[-1]


async def test_a_rejected_max_tokens_field_is_retried_with_the_other_name() -> None:
    class NewField(MockOpenAI):
        def handler(self, request: httpx2.Request) -> httpx2.Response:
            body = json.loads(request.content)
            self.requests.append(body)
            if "max_tokens" in body:
                return httpx2.Response(
                    400, json={"error": {"message": "max_tokens is not supported; use max_completion_tokens"}}
                )
            return httpx2.Response(
                200,
                json={
                    "choices": [
                        {
                            "index": 0,
                            "finish_reason": "stop",
                            "message": {"role": "assistant", "content": "ok"},
                        }
                    ],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1},
                },
            )

    provider, _ = build(mock=NewField([]))
    response = await provider.complete(request())

    assert response.text == "ok"
    assert "max_completion_tokens" in provider._body(request(), None, 100)


async def test_a_truncated_structured_answer_is_retried_once_with_a_larger_cap() -> None:
    mock = MockOpenAI(
        [
            Step(payload={"answer": "cut"}, finish_reason="length"),
            Step(payload={"answer": "complete"}),
        ]
    )
    provider, _ = build(mock=mock)
    budget = Budget(limits=Limits(model_calls=5))

    response = await provider.complete(request(schema=SCHEMA, max_output_tokens=64), budget=budget)

    assert response.structured == {"answer": "complete"}
    assert mock.requests[0]["max_tokens"] == 64
    assert mock.requests[1]["max_tokens"] == 128
    assert response.model_calls == 2


async def test_a_second_truncation_fails_the_stage() -> None:
    settings = model_settings(max_output_tokens=64)
    mock = MockOpenAI([Step(payload={"answer": "cut"}, finish_reason="length") for _ in range(3)])
    provider, _ = build(settings=settings, mock=mock)

    with pytest.raises(ProviderTruncatedError):
        await provider.complete(request(schema=SCHEMA, max_output_tokens=64))


async def test_the_last_rung_without_json_fails_the_stage() -> None:
    mock = MockOpenAI([Step(content="I cannot answer in JSON.")], profile="text")
    provider, _ = build(mock=mock)

    with pytest.raises(ProviderRejectedError, match="no usable structured output"):
        await provider.complete(request(stage=Stage.DIAGNOSE, schema=SCHEMA))


async def test_a_fenced_json_answer_survives_the_text_rung() -> None:
    mock = MockOpenAI([Step(content='Here you go:\n```json\n{"answer": "done"}\n```')], profile="text")
    provider, _ = build(mock=mock)

    response = await provider.complete(request(stage=Stage.DIAGNOSE, schema=SCHEMA))

    assert response.structured == {"answer": "done"}
    assert response.rung is Rung.TEXT
