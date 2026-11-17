"""Claude providers for the Anthropic API and Amazon Bedrock.

Both APIs use the Messages protocol. The graph owns schema Validation and
investigation budgets; these adapters preserve tool calls and count all input
tokens, including cache reads and writes, against that budget.
"""

from __future__ import annotations

from types import TracebackType
from typing import Self, cast

from anthropic import AsyncAnthropic, AsyncAnthropicBedrock, omit
from anthropic.types import MessageParam, TextBlockParam, ToolParam, ToolResultBlockParam, ToolUseBlockParam

from pagerless_agent.model import Message, ModelRequest, ModelResponse, ToolCall, Usage

type _ContentBlock = TextBlockParam | ToolResultBlockParam | ToolUseBlockParam

# Fixed IDs, without aliases or date suffixes, verified on 2026-11-16:
# https://platform.claude.com/docs/en/models/haiku-5-5/migration-guide
ANTHROPIC_MODEL = "claude-haiku-5-5"
BEDROCK_MODEL = "anthropic.claude-haiku-5-5"
_OUTPUT_TOOL = "pagerless_structured_output"


class ProviderError(Exception):
    """The model did not return the complete response the graph requested."""


class _MessagesProvider:
    """The shared Messages protocol, with ownership of the SDK client's lifecycle."""

    _client: AsyncAnthropic | AsyncAnthropicBedrock
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
                    "description": "Return the requested Diagnosis or Proposal as structured data.",
                    "input_schema": {"type": "object", **request.output_schema},
                }
            ]
        if tools:
            tools[-1]["cache_control"] = {"type": "ephemeral"}
        response = await self._client.messages.create(
            model=self._model,
            max_tokens=self._max_output_tokens,
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


class BedrockProvider(_MessagesProvider):
    """Amazon Bedrock, using the SDK's AWS credential chain and optional named profile."""

    def __init__(
        self,
        *,
        aws_region: str | None = None,
        aws_profile: str | None = None,
        client: AsyncAnthropicBedrock | None = None,
        max_output_tokens: int = 4096,
    ) -> None:
        super().__init__(max_output_tokens=max_output_tokens)
        self._client = (
            client.with_options(max_retries=0)
            if client is not None
            else AsyncAnthropicBedrock(
                aws_region=aws_region, aws_profile=aws_profile, max_retries=0, timeout=60
            )
        )
        self._model = BEDROCK_MODEL


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
