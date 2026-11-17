"""Live model providers exercised through the SDK's HTTP boundary, without paid calls."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

import anthropic
import boto3
import httpx2
import pytest
from botocore.stub import Stubber

from pagerless_agent.model import Message, ModelRequest, Stage, ToolCall, ToolSpec
from pagerless_agent.providers import (
    ANTHROPIC_MODEL,
    BEDROCK_MODEL,
    AnthropicProvider,
    BedrockProvider,
    ProviderError,
    _client_config,
    _converse_request,
)
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


def bedrock_client() -> Any:
    """A Converse client with the agent's timeout and retry policy, without reaching AWS."""
    return boto3.client(
        "bedrock-runtime",
        region_name="us-east-1",
        aws_access_key_id="test-access",
        aws_secret_access_key="test-secret",
        aws_session_token="test-session",
        config=_client_config(),
    )


def converse_response(
    content: list[dict[str, Any]] | None = None, *, stop_reason: str = "end_turn"
) -> dict[str, Any]:
    return {
        "output": {"message": {"role": "assistant", "content": content or [{"text": "Done."}]}},
        "stopReason": stop_reason,
        "usage": {
            "inputTokens": 17,
            "cacheReadInputTokens": 2000,
            "cacheWriteInputTokens": 300,
            "outputTokens": 23,
            "totalTokens": 2340,
        },
        "metrics": {"latencyMs": 12},
    }


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


async def test_a_request_caps_output_tokens_below_the_provider_default() -> None:
    exchange = Exchange()
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    narrative = request().model_copy(
        update={
            "stage": Stage.NARRATIVE,
            "tools": (),
            "bundle": None,
            "max_output_tokens": 2000,
        }
    )
    async with AnthropicProvider(client=client) as provider:
        await provider.complete(narrative)

    assert json.loads(exchange.requests[0].content)["max_tokens"] == 2000


async def test_a_request_cannot_raise_the_providers_output_cap() -> None:
    exchange = Exchange()
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    raised = request().model_copy(update={"max_output_tokens": 1_000_000})
    async with AnthropicProvider(client=client) as provider:
        await provider.complete(raised)

    assert json.loads(exchange.requests[0].content)["max_tokens"] == 4096


@pytest.mark.parametrize("stop_reason", ["max_tokens", "refusal", "model_context_window_exceeded"])
async def test_truncated_or_refused_answers_cannot_be_treated_as_completed_output(stop_reason: str) -> None:
    exchange = Exchange(stop_reason=stop_reason)
    client = anthropic.AsyncAnthropic(
        api_key="test-key", http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(exchange.respond))
    )
    async with AnthropicProvider(client=client) as provider:
        with pytest.raises(ProviderError, match=stop_reason):
            await provider.complete(request())


def test_the_live_models_keep_the_anthropic_model_and_pin_the_nova_inference_profile() -> None:
    assert ANTHROPIC_MODEL == "claude-haiku-5-5"
    assert BEDROCK_MODEL == "us.amazon.nova-2-lite-v1:0"


def test_the_bedrock_client_does_not_retry_outside_the_graphs_budget() -> None:
    config = _client_config()

    assert config.retries == {"max_attempts": 0, "mode": "standard"}
    assert config.connect_timeout == 10
    assert config.read_timeout == 60


def test_converse_caches_the_system_prefix_without_a_tool_cache_point() -> None:
    body = _converse_request(request(), max_output_tokens=4096)

    # Nova 2 Lite accepts cache checkpoints in `system` and `messages` only. The
    # checkpoint after the prompt caches the tool definitions with it, and it is
    # the only caching the Bedrock path enables.
    assert body["system"] == [
        {"text": "Read the evidence and propose only an allowed Action."},
        {"cachePoint": {"type": "default"}},
    ]
    assert all("cachePoint" not in tool for tool in body["toolConfig"]["tools"])
    assert "outputConfig" not in body
    assert body["inferenceConfig"] == {"maxTokens": 4096}


def test_converse_structured_output_is_one_forced_tool_and_not_a_json_schema_format() -> None:
    schema = {"type": "object", "properties": {"diagnosis": {"type": "string"}}, "required": ["diagnosis"]}
    structured = request().model_copy(update={"stage": Stage.PROPOSE, "output_schema": schema})

    body = _converse_request(structured, max_output_tokens=4096)

    assert body["toolConfig"] == {
        "tools": [
            {
                "toolSpec": {
                    "name": "pagerless_structured_output",
                    "description": "Return the requested Diagnosis or Proposal as structured data.",
                    "inputSchema": {"json": schema},
                }
            }
        ],
        "toolChoice": {"tool": {"name": "pagerless_structured_output"}},
    }
    # Nova 2 Lite does not support Bedrock structured outputs, so the schema must
    # travel as a forced tool call rather than as `outputConfig.textFormat`.
    assert "outputConfig" not in body


async def test_bedrock_sends_the_converse_request_and_counts_cached_input_tokens() -> None:
    client = bedrock_client()
    stubber = Stubber(client)
    stubber.add_response(
        "converse", converse_response(), _converse_request(request(), max_output_tokens=4096)
    )
    with stubber:
        async with BedrockProvider(client=client) as provider:
            response = await provider.complete(request())

    stubber.assert_no_pending_responses()
    assert response.text == "Done."
    assert response.usage.input_tokens == 2317
    assert response.usage.output_tokens == 23


async def test_bedrock_returns_the_forced_tool_call_as_structured_data() -> None:
    output = {"diagnosis": "The memory limit is insufficient."}
    schema = {"type": "object", "properties": {"diagnosis": {"type": "string"}}, "required": ["diagnosis"]}
    structured = request().model_copy(update={"stage": Stage.DIAGNOSE, "output_schema": schema})
    client = bedrock_client()
    stubber = Stubber(client)
    stubber.add_response(
        "converse",
        converse_response(
            [
                {
                    "toolUse": {
                        "toolUseId": "output_1",
                        "name": "pagerless_structured_output",
                        "input": output,
                    }
                }
            ],
            stop_reason="tool_use",
        ),
        _converse_request(structured, max_output_tokens=4096),
    )
    with stubber:
        async with BedrockProvider(client=client) as provider:
            response = await provider.complete(structured)

    stubber.assert_no_pending_responses()
    assert response.structured == output
    assert response.tool_calls == ()
    assert response.text == ""


@pytest.mark.parametrize(
    "content",
    [
        [{"text": "This is not structured output."}],
        [{"toolUse": {"toolUseId": "out", "name": "pods_log", "input": {}}}],
        [
            {
                "toolUse": {
                    "toolUseId": "out",
                    "name": "pagerless_structured_output",
                    "input": [],
                }
            }
        ],
        [
            {"toolUse": {"toolUseId": "out1", "name": "pagerless_structured_output", "input": {}}},
            {"toolUse": {"toolUseId": "out2", "name": "pagerless_structured_output", "input": {}}},
        ],
    ],
)
async def test_bedrock_refuses_malformed_or_ambiguous_structured_output(
    content: list[dict[str, Any]],
) -> None:
    structured = request().model_copy(update={"stage": Stage.PROPOSE, "output_schema": {"type": "object"}})
    client = bedrock_client()
    stubber = Stubber(client)
    stubber.add_response(
        "converse",
        converse_response(content, stop_reason="tool_use"),
        _converse_request(structured, max_output_tokens=4096),
    )
    with stubber:
        async with BedrockProvider(client=client) as provider:
            with pytest.raises(ProviderError):
                await provider.complete(structured)


async def test_bedrock_translates_tool_results_into_converse_content_blocks() -> None:
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
    client = bedrock_client()
    stubber = Stubber(client)
    stubber.add_response(
        "converse",
        converse_response(
            [
                {"text": "Read the previous container logs."},
                {"toolUse": {"toolUseId": "call_3", "name": "pods_log", "input": {"previous": True}}},
            ],
            stop_reason="tool_use",
        ),
        _converse_request(request(messages=history), max_output_tokens=4096),
    )
    with stubber:
        async with BedrockProvider(client=client) as provider:
            response = await provider.complete(request(messages=history))

    stubber.assert_no_pending_responses()
    assert response.text == "Read the previous container logs."
    assert response.tool_calls == (ToolCall(id="call_3", name="pods_log", arguments={"previous": True}),)
    body = _converse_request(request(messages=history), max_output_tokens=4096)
    assert body["messages"] == [
        {"role": "user", "content": [{"text": "Investigate checkout."}]},
        {
            "role": "assistant",
            "content": [
                {"text": "Gathering evidence."},
                {"toolUse": {"toolUseId": "call_1", "name": "pods_log", "input": {"name": "checkout-1"}}},
                {
                    "toolUse": {
                        "toolUseId": "call_2",
                        "name": "events_list",
                        "input": {"namespace": "sandbox"},
                    }
                },
            ],
        },
        {
            "role": "user",
            "content": [
                {"toolResult": {"toolUseId": "call_1", "content": [{"text": "OOMKilled"}]}},
                {"toolResult": {"toolUseId": "call_2", "content": [{"text": "BackOff"}]}},
            ],
        },
    ]


@pytest.mark.parametrize(("requested", "expected"), [(2000, 2000), (1_000_000, 4096)])
async def test_bedrock_caps_output_tokens_below_the_provider_default(requested: int, expected: int) -> None:
    narrative = request().model_copy(
        update={
            "stage": Stage.NARRATIVE,
            "tools": (),
            "bundle": None,
            "max_output_tokens": requested,
        }
    )
    body = _converse_request(narrative, max_output_tokens=4096)
    client = bedrock_client()
    stubber = Stubber(client)
    stubber.add_response("converse", converse_response(), body)
    with stubber:
        async with BedrockProvider(client=client) as provider:
            await provider.complete(narrative)

    stubber.assert_no_pending_responses()
    assert body["inferenceConfig"] == {"maxTokens": expected}
    assert "toolConfig" not in body


@pytest.mark.parametrize(
    "stop_reason",
    ["max_tokens", "content_filtered", "malformed_tool_use", "model_context_window_exceeded"],
)
async def test_bedrock_refuses_truncated_or_refused_answers(stop_reason: str) -> None:
    client = bedrock_client()
    stubber = Stubber(client)
    stubber.add_response(
        "converse",
        converse_response(stop_reason=stop_reason),
        _converse_request(request(), max_output_tokens=4096),
    )
    with stubber:
        async with BedrockProvider(client=client) as provider:
            with pytest.raises(ProviderError, match=stop_reason):
                await provider.complete(request())
