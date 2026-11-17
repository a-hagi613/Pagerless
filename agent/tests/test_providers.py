"""Live model providers exercised through the SDK's HTTP boundary, without paid calls."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

import anthropic
import httpx2
import pytest

from pagerless_agent.model import Message, ModelRequest, Stage, ToolCall, ToolSpec
from pagerless_agent.providers import AnthropicProvider, ProviderError
from tests.support import make_bundle


@dataclass
class Exchange:
    """One model endpoint that captures the request and returns an API response."""

    content: list[dict[str, Any]] = field(default_factory=lambda: [{"type": "text", "text": "Done."}])
    stop_reason: str = "end_turn"
    usage: dict[str, int] = field(
        default_factory=lambda: {
            "input_tokens": 17,
            "cache_read_input_tokens": 2000,
            "cache_creation_input_tokens": 300,
            "output_tokens": 23,
        }
    )
    requests: list[httpx2.Request] = field(default_factory=list)
    status_code: int = 200

    def respond(self, request: httpx2.Request) -> httpx2.Response:
        self.requests.append(request)
        if self.status_code != 200:
            return httpx2.Response(
                self.status_code,
                json={"type": "error", "error": {"type": "api_error", "message": "Unavailable"}},
            )
        return httpx2.Response(
            200,
            json={
                "id": "msg_1",
                "type": "message",
                "role": "assistant",
                "model": "claude-haiku-5-5",
                "content": self.content,
                "stop_reason": self.stop_reason,
                "stop_sequence": None,
                "usage": self.usage,
            },
        )


def request(*, messages: tuple[Message, ...] | None = None) -> ModelRequest:
    return ModelRequest(
        stage=Stage.INVESTIGATE,
        system="Read the evidence and propose only an allowed Action.",
        messages=messages or (Message(role="user", content="Investigate checkout."),),
        tools=(
            ToolSpec(name="pods_log", description="Read pod logs.", input_schema={"type": "object"}),
            ToolSpec(name="events_list", description="Read pod events.", input_schema={"type": "object"}),
        ),
        bundle=make_bundle(),
    )


async def test_anthropic_caches_the_static_prompt_and_counts_all_input_tokens() -> None:
    exchange = Exchange()
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    async with AnthropicProvider(client=client) as provider:
        response = await provider.complete(request())

    assert response.text == "Done."
    assert response.usage.input_tokens == 2317
    assert response.usage.output_tokens == 23
    outbound = exchange.requests[0]
    assert outbound.url.path == "/v1/messages"
    assert outbound.headers["authorization"] == "Bearer test-key"
    body = json.loads(outbound.content)
    assert body["model"] == "claude-haiku-5-5"
    assert body["system"] == [
        {
            "type": "text",
            "text": "Read the evidence and propose only an allowed Action.",
            "cache_control": {"type": "ephemeral"},
        }
    ]
    assert "cache_control" not in body["tools"][0]
    assert body["tools"][-1]["cache_control"] == {"type": "ephemeral"}
    assert body["thinking"] == {"type": "disabled"}
    assert client.is_closed()


@pytest.mark.parametrize("status_code", [401, 500])
async def test_provider_errors_do_not_retry_outside_the_graphs_budget(status_code: int) -> None:
    exchange = Exchange(status_code=status_code)
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    async with AnthropicProvider(client=client) as provider:
        with pytest.raises(anthropic.APIStatusError) as error:
            await provider.complete(request())

    assert error.value.status_code == status_code
    assert len(exchange.requests) == 1
    assert client.is_closed()


@pytest.mark.parametrize(
    "content",
    [
        [{"type": "text", "text": "This is not structured output."}],
        [{"type": "tool_use", "id": "out", "name": "pods_log", "input": {}}],
        [{"type": "tool_use", "id": "out", "name": "pagerless_structured_output", "input": []}],
        [
            {"type": "tool_use", "id": "out1", "name": "pagerless_structured_output", "input": {}},
            {"type": "tool_use", "id": "out2", "name": "pagerless_structured_output", "input": {}},
        ],
    ],
)
async def test_malformed_or_ambiguous_structured_output_is_refused(content: list[dict[str, Any]]) -> None:
    exchange = Exchange(content=content, stop_reason="tool_use")
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    structured_request = request().model_copy(
        update={"stage": Stage.PROPOSE, "output_schema": {"type": "object"}}
    )
    async with AnthropicProvider(client=client) as provider:
        with pytest.raises(ProviderError):
            await provider.complete(structured_request)


async def test_multiple_tool_calls_and_results_keep_their_ids_and_message_roles() -> None:
    exchange = Exchange(
        content=[
            {"type": "text", "text": "Read the previous container logs."},
            {"type": "tool_use", "id": "call_3", "name": "pods_log", "input": {"previous": True}},
        ],
        stop_reason="tool_use",
    )
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    calls = (
        ToolCall(id="call_1", name="pods_log", arguments={"name": "checkout-1"}),
        ToolCall(id="call_2", name="events_list", arguments={"namespace": "sandbox"}),
    )
    history = (
        Message(role="user", content="Investigate checkout."),
        Message(role="assistant", content="Gathering evidence.", tool_calls=calls),
        Message(role="tool", content="OOMKilled", tool_call_id="call_1"),
        Message(role="tool", content="BackOff", tool_call_id="call_2"),
    )
    async with AnthropicProvider(client=client) as provider:
        response = await provider.complete(request(messages=history))

    assert response.text == "Read the previous container logs."
    assert response.tool_calls == (ToolCall(id="call_3", name="pods_log", arguments={"previous": True}),)
    messages = json.loads(exchange.requests[0].content)["messages"]
    assert [message["role"] for message in messages] == ["user", "assistant", "user"]
    assert messages[1]["content"] == [
        {"type": "text", "text": "Gathering evidence."},
        {"type": "tool_use", "id": "call_1", "name": "pods_log", "input": {"name": "checkout-1"}},
        {"type": "tool_use", "id": "call_2", "name": "events_list", "input": {"namespace": "sandbox"}},
    ]
    assert messages[2]["content"] == [
        {"type": "tool_result", "tool_use_id": "call_1", "content": "OOMKilled"},
        {"type": "tool_result", "tool_use_id": "call_2", "content": "BackOff"},
    ]


@pytest.mark.parametrize("stage", [Stage.DIAGNOSE, Stage.PROPOSE])
async def test_schema_output_is_returned_as_structured_data_without_running_external_tools(
    stage: Stage,
) -> None:
    output = {"diagnosis": "The memory limit is insufficient."}
    schema = {"type": "object", "properties": {"diagnosis": {"type": "string"}}, "required": ["diagnosis"]}
    exchange = Exchange(
        content=[
            {"type": "tool_use", "id": "output_1", "name": "pagerless_structured_output", "input": output}
        ],
        stop_reason="tool_use",
    )
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    structured_request = request().model_copy(update={"stage": stage, "output_schema": schema})
    async with AnthropicProvider(client=client) as provider:
        response = await provider.complete(structured_request)

    assert response.structured == output
    assert response.tool_calls == ()
    body = json.loads(exchange.requests[0].content)
    assert len(body["tools"]) == 1
    assert body["tools"][0]["name"] == "pagerless_structured_output"
    assert body["tools"][0]["input_schema"] == schema
    assert body["tools"][0]["cache_control"] == {"type": "ephemeral"}
    assert body["tool_choice"] == {
        "type": "tool",
        "name": "pagerless_structured_output",
        "disable_parallel_tool_use": True,
    }


@pytest.mark.parametrize("stop_reason", ["max_tokens", "refusal", "model_context_window_exceeded"])
async def test_truncated_or_refused_answers_cannot_be_treated_as_completed_output(stop_reason: str) -> None:
    exchange = Exchange(stop_reason=stop_reason)
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    async with AnthropicProvider(client=client) as provider:
        with pytest.raises(ProviderError, match=stop_reason):
            await provider.complete(request())


async def test_bedrock_uses_the_pinned_model_and_signs_the_same_cached_messages_request() -> None:
    from pagerless_agent.providers import BedrockProvider

    exchange = Exchange()
    client = anthropic.AsyncAnthropicBedrock(
        aws_region="us-east-1",
        aws_access_key="test-access",
        aws_secret_key="test-secret",
        aws_session_token="test-session",
        http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond)),
    )
    async with BedrockProvider(client=client) as provider:
        response = await provider.complete(request())

    assert response.text == "Done."
    assert response.usage.input_tokens == 2317
    outbound = exchange.requests[0]
    assert outbound.url.host == "bedrock-runtime.us-east-1.amazonaws.com"
    assert outbound.url.path == "/model/anthropic.claude-haiku-5-5/invoke"
    assert outbound.headers["authorization"].startswith("AWS4-HMAC-SHA256 Credential=test-access/")
    assert outbound.headers["x-amz-security-token"] == "test-session"
    body = json.loads(outbound.content)
    assert "model" not in body
    assert body["system"][0]["cache_control"] == {"type": "ephemeral"}
    assert body["tools"][-1]["cache_control"] == {"type": "ephemeral"}
    assert body["thinking"] == {"type": "disabled"}
    assert client.is_closed()
