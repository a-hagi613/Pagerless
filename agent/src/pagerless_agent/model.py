"""The interface between the agent graph and a model endpoint.

One provider speaks the OpenAI Chat Completions protocol. It answers a `ModelRequest`
with a `ModelResponse`; the graph never sees an SDK object, a provider dialect, or a
retry. `Rung` records which structured-output rung produced a stage's output, so that a
degraded installation is visible on the Page.
"""

from __future__ import annotations

from enum import StrEnum
from typing import Any, Protocol

from pydantic import BaseModel, ConfigDict, Field

from pagerless_agent.budget import Budget
from pagerless_agent.structured import Rung


class _Frozen(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class Stage(StrEnum):
    """The stage that makes a model call: a graph stage, or the Narrative Job."""

    INVESTIGATE = "investigate"
    DIAGNOSE = "diagnose"
    PROPOSE = "propose"
    NARRATIVE = "narrative"


class ToolSpec(_Frozen):
    """A tool the model may call, forwarded verbatim from the server's catalogue."""

    name: str
    description: str
    input_schema: dict[str, Any]


class ToolCall(_Frozen):
    """A request from the model to call one tool.

    `argument_error` is set when the model's arguments could not be repaired. The graph
    returns it to the model as a correctable tool error, so a weak model gets one chance
    to call the tool again rather than failing the Job (§3.4.3).
    """

    id: str
    name: str
    arguments: dict[str, Any] = Field(default_factory=dict)
    argument_error: str | None = None


class Message(_Frozen):
    """One message of the conversation.

    A `tool` message carries the output of the call named by `tool_call_id`. Provider-private
    reasoning fields have no representation here: they are dropped when a response is
    normalised and can therefore never be echoed back to a provider.
    """

    role: str
    content: str = ""
    tool_calls: tuple[ToolCall, ...] = ()
    tool_call_id: str | None = None


class Usage(_Frozen):
    """The tokens one model call used, and whether the provider reported them.

    `estimated` is true when the provider omitted `usage` and the figure was computed
    locally, which the report then states so the Page never presents an estimate as fact.
    """

    input_tokens: int = Field(ge=0)
    output_tokens: int = Field(ge=0)
    estimated: bool = False


class ModelRequest(_Frozen):
    """One call to the model.

    `output_schema` is set when the stage needs structured output; the response must then
    carry `structured`. `max_output_tokens` caps this call when the stage needs a smaller
    limit than the endpoint's.
    """

    stage: Stage
    system: str
    messages: tuple[Message, ...]
    tools: tuple[ToolSpec, ...] = ()
    output_schema: dict[str, Any] | None = None
    max_output_tokens: int | None = Field(default=None, ge=1)


class ModelResponse(_Frozen):
    """The model's answer to one request.

    `rung` is the structured-output rung that produced `structured`, `None` for a stage
    that asked for no schema. `model_calls` counts the HTTP requests behind this answer,
    including transport retries, so the budget accounts for every one.
    """

    text: str = ""
    tool_calls: tuple[ToolCall, ...] = ()
    structured: dict[str, Any] | None = None
    usage: Usage
    rung: Rung | None = None
    finish_reason: str | None = None
    model_calls: int = 1


class ModelProvider(Protocol):
    """Anything that can answer a model request."""

    async def complete(self, request: ModelRequest, *, budget: Budget | None = None) -> ModelResponse:
        """Answer the request, counting every call against `budget`."""
        ...

    def estimate_input_tokens(self, request: ModelRequest) -> int:
        """Return a pessimistic estimate of the request's input tokens."""
        ...
