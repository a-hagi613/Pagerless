"""The small interface between the agent graph and a Chat Completions endpoint.

The graph never sees an SDK object, a provider dialect or a retry. It works with
`ModelRequest` and `ModelResponse`, which also make the tests deterministic.
"""

from __future__ import annotations

from typing import Literal, Protocol

from pydantic import BaseModel, ConfigDict, Field

from pagerless_agent.budget import Budget


class _Frozen(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class ToolSpec(_Frozen):
    """A tool the model may call, forwarded from the Go tool policy."""

    name: str
    description: str
    input_schema: dict[str, object]


class ToolCall(_Frozen):
    """One tool call requested by the model.

    `argument_error` is set when the requested arguments were not a JSON object. The graph
    returns it as a correctable tool error, so a weak model gets another attempt instead of
    failing the turn.
    """

    id: str
    name: str
    arguments: dict[str, object] = Field(default_factory=dict)
    argument_error: str | None = None


class Message(_Frozen):
    """One message sent to or received from the model."""

    role: Literal["system", "user", "assistant", "tool"]
    content: str = ""
    tool_calls: tuple[ToolCall, ...] = ()
    tool_call_id: str | None = None


class Usage(_Frozen):
    """The tokens one model call used, and whether the provider reported them."""

    input_tokens: int = Field(ge=0)
    output_tokens: int = Field(ge=0)
    estimated: bool = False


class ModelRequest(_Frozen):
    """One request. `forced_tool` asks the provider to force a named function call."""

    system: str
    messages: tuple[Message, ...]
    tools: tuple[ToolSpec, ...] = ()
    max_output_tokens: int | None = Field(default=None, ge=1)
    forced_tool: str | None = None
    purpose: Literal["turn", "readiness"] = "turn"


class ModelResponse(_Frozen):
    """The provider's answer, normalised and bounded."""

    text: str = ""
    tool_calls: tuple[ToolCall, ...] = ()
    usage: Usage
    finish_reason: str | None = None
    model_calls: int = 1


class ModelProvider(Protocol):
    """Anything that can answer one Chat Completions request."""

    async def complete(self, request: ModelRequest, *, budget: Budget | None = None) -> ModelResponse:
        """Answer the request, counting every HTTP attempt against `budget`."""
        ...

    def estimate_input_tokens(self, request: ModelRequest) -> int:
        """Return a pessimistic estimate of the request's input tokens."""
        ...
