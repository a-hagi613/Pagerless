"""The interface between the agent graph and a model provider.

A provider answers one `ModelRequest` with one `ModelResponse`. Live providers
(Bedrock and the Anthropic API) and Replay implement the same `ModelProvider`
protocol, so the graph cannot tell them apart.
"""

from __future__ import annotations

from enum import StrEnum
from typing import Any, Literal, Protocol

from pydantic import BaseModel, ConfigDict, Field

from pagerless_agent.bundle import ContextBundle


class _Frozen(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class Stage(StrEnum):
    """The stage that makes a model call: one of the investigation's graph or the Narrative."""

    INVESTIGATE = "investigate"
    DIAGNOSE = "diagnose"
    PROPOSE = "propose"
    NARRATIVE = "narrative"


class ToolSpec(_Frozen):
    """A tool the model may call."""

    name: str
    description: str
    input_schema: dict[str, Any]


class ToolCall(_Frozen):
    """A request from the model to call one tool."""

    id: str
    name: str
    arguments: dict[str, Any]


class Message(_Frozen):
    """One message of the conversation.

    A `tool` message carries the output of the call named by `tool_call_id`.
    """

    role: Literal["user", "assistant", "tool"]
    content: str
    tool_calls: tuple[ToolCall, ...] = ()
    tool_call_id: str | None = None


class Usage(_Frozen):
    """The tokens one call used. `input_tokens` counts every input token, cached or not."""

    input_tokens: int = Field(ge=0)
    output_tokens: int = Field(ge=0)


class ModelRequest(_Frozen):
    """One call to the model.

    `output_schema` is set when the stage needs structured output, and the
    response must then carry `structured`. `bundle` is the Context Bundle of the
    investigation. Live providers ignore it, because the messages already
    contain it. Replay resolves its recorded placeholders against it. The
    Narrative has no Context Bundle, so its recorded text carries no
    placeholders. `max_output_tokens` caps this call when the stage needs a
    smaller limit than the provider's.
    """

    stage: Stage
    system: str
    messages: tuple[Message, ...]
    tools: tuple[ToolSpec, ...]
    output_schema: dict[str, Any] | None = None
    bundle: ContextBundle | None = None
    max_output_tokens: int | None = Field(default=None, ge=1)


class ModelResponse(_Frozen):
    """The model's answer to one request."""

    text: str = ""
    tool_calls: tuple[ToolCall, ...] = ()
    structured: dict[str, Any] | None = None
    usage: Usage


class ModelProvider(Protocol):
    """Anything that can answer a model request."""

    async def complete(self, request: ModelRequest) -> ModelResponse:
        """Answer the request."""
        ...
