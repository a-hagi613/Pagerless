"""Live model providers: Claude Haiku 5.5 on the Anthropic API and Nova 2 Lite on Bedrock.

The Anthropic provider speaks the Messages protocol. The Bedrock provider speaks
the Converse API, because Amazon Nova does not accept the Messages protocol. Both
adapters preserve tool calls and count every input token, including cache reads
and writes, against the graph's investigation budget.
"""

from __future__ import annotations

import asyncio
from types import TracebackType
from typing import Any, Protocol, Self, cast

import boto3
from anthropic import AsyncAnthropic, omit
from anthropic.types import MessageParam, TextBlockParam, ToolParam, ToolResultBlockParam, ToolUseBlockParam
from botocore.config import Config

from pagerless_agent.model import Message, ModelRequest, ModelResponse, ToolCall, Usage

type _ContentBlock = TextBlockParam | ToolResultBlockParam | ToolUseBlockParam

# Fixed ID, without aliases or date suffixes, verified on 2026-11-16:
# https://platform.claude.com/docs/en/models/haiku-5-5/migration-guide
ANTHROPIC_MODEL = "claude-haiku-5-5"

# The US geo cross-Region inference profile, verified ACTIVE in us-east-1 on
# 2026-11-17. Nova 2 Lite has no in-Region endpoint there, so the profile is the
# only way to address it from the account:
# https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-amazon-nova-2-lite.html
BEDROCK_MODEL = "us.amazon.nova-2-lite-v1:0"

_OUTPUT_TOOL = "pagerless_structured_output"
_OUTPUT_DESCRIPTION = "Return the requested Diagnosis or Proposal as structured data."


class ProviderError(Exception):
    """The model did not return the complete response the graph requested."""


class _MessagesProvider:
    """The shared Messages protocol, with ownership of the SDK client's lifecycle."""

    _client: AsyncAnthropic
    _model: str

    def __init__(self, *, max_output_tokens: int) -> None:
        if max_output_tokens <= 0:
            raise ValueError("max_output_tokens must be positive")
        self._max_output_tokens = max_output_tokens

    async def complete(self, request: ModelRequest) -> ModelResponse:
        """Send one Messages request, caching the system prompt and tool definitions."""
        tools: list[ToolParam] = [
            {"name": spec.name, "description": spec.description, "input_schema": spec.input_schema}
            for spec in request.tools
        ]
        if request.output_schema is not None:
            tools = [
                {
                    "name": _OUTPUT_TOOL,
                    "description": _OUTPUT_DESCRIPTION,
                    "input_schema": {"type": "object", **request.output_schema},
                }
            ]
        if tools:
            tools[-1]["cache_control"] = {"type": "ephemeral"}
        response = await self._client.messages.create(
            model=self._model,
            max_tokens=(
                self._max_output_tokens
                if request.max_output_tokens is None
                else min(self._max_output_tokens, request.max_output_tokens)
            ),
            system=[{"type": "text", "text": request.system, "cache_control": {"type": "ephemeral"}}],
            tools=tools or omit,
            tool_choice=(
                {"type": "tool", "name": _OUTPUT_TOOL, "disable_parallel_tool_use": True}
                if request.output_schema is not None
                else omit
            ),
            messages=_messages(request.messages),
            # The graph's message protocol carries text and tool calls, not signed thinking blocks.
            thinking={"type": "disabled"},
        )
        if response.stop_reason not in ("end_turn", "tool_use"):
            raise ProviderError(f"model did not complete the response: {response.stop_reason}")
        if any(block.type == "tool_use" and not isinstance(block.input, dict) for block in response.content):
            raise ProviderError("model tool arguments must be a JSON object")
        calls = tuple(
            ToolCall(id=block.id, name=block.name, arguments=block.input)
            for block in response.content
            if block.type == "tool_use"
        )
        structured = None
        if request.output_schema is not None:
            if len(calls) != 1 or calls[0].name != _OUTPUT_TOOL:
                raise ProviderError("expected exactly one structured output from the model")
            structured = calls[0].arguments
            calls = ()
        return ModelResponse(
            text="\n".join(block.text for block in response.content if block.type == "text"),
            tool_calls=calls,
            structured=structured,
            usage=Usage(
                input_tokens=response.usage.input_tokens
                + (response.usage.cache_read_input_tokens or 0)
                + (response.usage.cache_creation_input_tokens or 0),
                output_tokens=response.usage.output_tokens,
            ),
        )

    async def aclose(self) -> None:
        """Release the SDK client's HTTP connections."""
        await self._client.close()

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        await self.aclose()


class AnthropicProvider(_MessagesProvider):
    """The Anthropic API, using the SDK's API-key environment discovery by default."""

    def __init__(
        self,
        *,
        api_key: str | None = None,
        client: AsyncAnthropic | None = None,
        max_output_tokens: int = 4096,
    ) -> None:
        super().__init__(max_output_tokens=max_output_tokens)
        # Every request is accounted for by the graph; hidden SDK retries would bypass its budget.
        self._client = (
            client.with_options(max_retries=0)
            if client is not None
            else AsyncAnthropic(api_key=api_key, max_retries=0, timeout=60)
        )
        self._model = ANTHROPIC_MODEL


class _BedrockRuntime(Protocol):
    """The part of the boto3 `bedrock-runtime` client that this adapter uses."""

    def converse(self, **kwargs: Any) -> dict[str, Any]: ...

    def close(self) -> None: ...


class BedrockProvider:
    """Amazon Bedrock through Converse, using the SDK's AWS credential chain and optional profile."""

    def __init__(
        self,
        *,
        aws_region: str | None = None,
        aws_profile: str | None = None,
        client: _BedrockRuntime | None = None,
        max_output_tokens: int = 4096,
    ) -> None:
        if max_output_tokens <= 0:
            raise ValueError("max_output_tokens must be positive")
        self._max_output_tokens = max_output_tokens
        self._client = client if client is not None else _bedrock_client(aws_region, aws_profile)

    async def complete(self, request: ModelRequest) -> ModelResponse:
        """Send one Converse request, caching the static prefix with a system cache point."""
        body = _converse_request(request, max_output_tokens=self._max_output_tokens)
        response = await asyncio.to_thread(self._client.converse, **body)
        return _converse_response(response, structured=request.output_schema is not None)

    async def aclose(self) -> None:
        """Release the client's HTTP connection pool."""
        self._client.close()

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        await self.aclose()


def _client_config() -> Config:
    """Bound one Converse attempt, so that hidden retries cannot bypass the graph's budget."""
    return Config(retries={"max_attempts": 0, "mode": "standard"}, connect_timeout=10, read_timeout=60)


def _bedrock_client(aws_region: str | None, aws_profile: str | None) -> _BedrockRuntime:
    """Create the Converse client through the SDK's credential chain."""
    session = boto3.Session(profile_name=aws_profile, region_name=aws_region)
    return cast(_BedrockRuntime, session.client("bedrock-runtime", config=_client_config()))


def _converse_request(request: ModelRequest, *, max_output_tokens: int) -> dict[str, Any]:
    """Build the Converse request, with structured output as one forced tool call."""
    tools: list[dict[str, Any]] = [
        {
            "toolSpec": {
                "name": spec.name,
                "description": spec.description,
                "inputSchema": {"json": spec.input_schema},
            }
        }
        for spec in request.tools
    ]
    tool_choice: dict[str, Any] | None = None
    if request.output_schema is not None:
        # Nova 2 Lite does not support Bedrock structured outputs (`outputConfig`),
        # so the graph's schema becomes one forced tool call, as on the Anthropic API.
        tools = [
            {
                "toolSpec": {
                    "name": _OUTPUT_TOOL,
                    "description": _OUTPUT_DESCRIPTION,
                    "inputSchema": {"json": {"type": "object", **request.output_schema}},
                }
            }
        ]
        tool_choice = {"tool": {"name": _OUTPUT_TOOL}}
    body: dict[str, Any] = {
        "modelId": BEDROCK_MODEL,
        "messages": _converse_messages(request.messages),
        # Nova 2 Lite accepts cache checkpoints in `system` and `messages` only,
        # not in `tools` (ADR-0023). A checkpoint after the system prompt caches
        # the tool definitions with it, because Converse evaluates the prefix in
        # the order tools, system, messages.
        "system": [{"text": request.system}, {"cachePoint": {"type": "default"}}],
        "inferenceConfig": {
            "maxTokens": (
                max_output_tokens
                if request.max_output_tokens is None
                else min(max_output_tokens, request.max_output_tokens)
            )
        },
    }
    if tools:
        tool_config: dict[str, Any] = {"tools": tools}
        if tool_choice is not None:
            tool_config["toolChoice"] = tool_choice
        body["toolConfig"] = tool_config
    return body


def _converse_response(response: dict[str, Any], *, structured: bool) -> ModelResponse:
    """Translate a Converse response, whose stop reasons and content blocks differ from Messages."""
    stop_reason = response.get("stopReason")
    if stop_reason not in ("end_turn", "tool_use"):
        raise ProviderError(f"model did not complete the response: {stop_reason}")
    texts: list[str] = []
    calls: list[ToolCall] = []
    for block in response["output"]["message"]["content"]:
        if "text" in block:
            texts.append(block["text"])
        elif "toolUse" in block:
            tool_use = block["toolUse"]
            arguments = tool_use["input"]
            if not isinstance(arguments, dict):
                raise ProviderError("model tool arguments must be a JSON object")
            calls.append(ToolCall(id=tool_use["toolUseId"], name=tool_use["name"], arguments=arguments))
    structured_output = None
    if structured:
        if len(calls) != 1 or calls[0].name != _OUTPUT_TOOL:
            raise ProviderError("expected exactly one structured output from the model")
        structured_output = calls[0].arguments
        calls = []
    usage = response["usage"]
    return ModelResponse(
        text="\n".join(texts),
        tool_calls=tuple(calls),
        structured=structured_output,
        usage=Usage(
            input_tokens=usage["inputTokens"]
            + usage.get("cacheReadInputTokens", 0)
            + usage.get("cacheWriteInputTokens", 0),
            output_tokens=usage["outputTokens"],
        ),
    )


def _messages(history: tuple[Message, ...]) -> list[MessageParam]:
    """Translate tool results to user content and group consecutive results in one turn."""
    result: list[MessageParam] = []
    for message in history:
        blocks: list[_ContentBlock] = []
        if message.role == "tool":
            if not message.tool_call_id:
                raise ValueError("a tool result must name the tool call it answers")
            blocks.append(
                {"type": "tool_result", "tool_use_id": message.tool_call_id, "content": message.content}
            )
            if result and result[-1]["role"] == "user":
                cast(list[_ContentBlock], result[-1]["content"]).extend(blocks)
            else:
                result.append({"role": "user", "content": blocks})
            continue
        if message.content:
            blocks.append({"type": "text", "text": message.content})
        blocks.extend(
            {"type": "tool_use", "id": call.id, "name": call.name, "input": call.arguments}
            for call in message.tool_calls
        )
        result.append(cast(MessageParam, {"role": message.role, "content": blocks}))
    return result


def _converse_messages(history: tuple[Message, ...]) -> list[dict[str, Any]]:
    """Translate tool results to Converse user content and group consecutive results in one turn."""
    result: list[dict[str, Any]] = []
    for message in history:
        blocks: list[dict[str, Any]] = []
        if message.role == "tool":
            if not message.tool_call_id:
                raise ValueError("a tool result must name the tool call it answers")
            blocks.append(
                {
                    "toolResult": {
                        "toolUseId": message.tool_call_id,
                        "content": [{"text": message.content}],
                    }
                }
            )
            if result and result[-1]["role"] == "user":
                cast(list[dict[str, Any]], result[-1]["content"]).extend(blocks)
            else:
                result.append({"role": "user", "content": blocks})
            continue
        if message.content:
            blocks.append({"text": message.content})
        blocks.extend(
            {"toolUse": {"toolUseId": call.id, "name": call.name, "input": call.arguments}}
            for call in message.tool_calls
        )
        result.append({"role": message.role, "content": blocks})
    return result
