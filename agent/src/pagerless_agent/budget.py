"""The four budgets of one Page investigation, and the context shedding that keeps them.

A Page investigation is bounded by four independent budgets (spec §3.6.1):

* 15 tool calls,
* 12 model calls,
* 300 000 input tokens,
* 300 seconds of wall clock.

Every call is counted before it is made and every budget raises `BudgetExhaustedError`
rather than letting a run overspend. Input tokens are estimated pessimistically before a
call, because a provider that omits `usage` must still be unable to exceed the budget
(§3.6.3). When the estimate approaches the model's context window, the oldest tool
results are shed first (§3.6.4); the system prompt, the task message, and the last
assistant turn are never shed.
"""

from __future__ import annotations

import math
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from pagerless_agent.model import Message

MAX_TOOL_CALLS = 15
MAX_MODEL_CALLS = 12
MAX_INPUT_TOKENS = 300_000
MAX_SECONDS = 300.0

# The share of the model's context window that a request may occupy before shedding.
CONTEXT_PRESSURE = 0.9
# The number of most recent tool results that shedding never elides.
KEEP_TOOL_RESULTS = 6
# The per-result cap applied to tool results once elision alone is not enough.
SHED_TOOL_RESULT_CHARS = 2_000
# Characters per token assumed for prose and for JSON. Both are pessimistic on purpose:
# a 15-20 percent overshoot is the safe direction for a safety rail.
PROSE_CHARS_PER_TOKEN = 3.6
JSON_CHARS_PER_TOKEN = 3.0

ELIDED_REASON = "context"


@dataclass(frozen=True)
class Limits:
    """How much one investigation may spend."""

    tool_calls: int = MAX_TOOL_CALLS
    model_calls: int = MAX_MODEL_CALLS
    input_tokens: int = MAX_INPUT_TOKENS
    seconds: float = MAX_SECONDS


class BudgetExhaustedError(Exception):
    """A budget ran out. `resource` names it: tool calls, model calls, input tokens, or time."""

    def __init__(self, resource: str, detail: str) -> None:
        super().__init__(detail)
        self.resource = resource
        self.detail = detail


def estimate_tokens(text: str, *, json: bool = False) -> int:
    """Return a pessimistic token estimate for a string.

    Provider tokenisers differ, and a provider that omits `usage` gives the agent nothing
    to measure. The estimate therefore rounds up, so an under-count cannot hide spend.
    """
    if not text:
        return 0
    return math.ceil(len(text) / (JSON_CHARS_PER_TOKEN if json else PROSE_CHARS_PER_TOKEN))


def estimate_messages_tokens(messages: tuple[Message, ...]) -> int:
    """Estimate the input tokens of a conversation, counting tool-call arguments as JSON."""
    total = 0
    for message in messages:
        total += estimate_tokens(message.content)
        for call in message.tool_calls:
            total += estimate_tokens(call.name) + estimate_tokens(str(call.arguments), json=True)
    return total


def shed_messages(
    messages: list[Message],
    *,
    keep_tool_results: int = KEEP_TOOL_RESULTS,
    truncate_to: int = SHED_TOOL_RESULT_CHARS,
) -> list[Message]:
    """Return the conversation with its oldest tool results elided and the rest truncated.

    Shedding operates on tool results only, and in this order:

    1. every tool result older than the last `keep_tool_results` becomes an elision marker;
    2. every remaining tool result is truncated to `truncate_to` characters.

    User and assistant turns are returned untouched: they carry the interface, the Target,
    and the current plan, and dropping them would change the task rather than its cost.
    """
    tool_indexes = [index for index, message in enumerate(messages) if message.role == "tool"]
    elide = set(tool_indexes[:-keep_tool_results]) if keep_tool_results else set(tool_indexes)
    shed: list[Message] = []
    for index, message in enumerate(messages):
        if message.role != "tool":
            shed.append(message)
            continue
        tool_name = message.tool_call_id or "tool"
        if index in elide:
            shed.append(
                message.model_copy(
                    update={
                        "content": f'{{"tool": "{tool_name}", "elided": true, "reason": "{ELIDED_REASON}"}}'
                    }
                )
            )
            continue
        if len(message.content) > truncate_to:
            kept = message.content[:truncate_to]
            marker = f'{{"truncated": true, "fullContentChars": {len(message.content)}}}'
            shed.append(message.model_copy(update={"content": f"{kept}\n{marker}"}))
            continue
        shed.append(message)
    return shed


@dataclass
class Budget:
    """Tracks what one investigation has spent against its four limits."""

    limits: Limits = field(default_factory=Limits)
    clock: Callable[[], float] = time.monotonic
    tool_calls: int = 0
    model_calls: int = 0
    input_tokens: int = 0
    _started: float = field(init=False)

    def __post_init__(self) -> None:
        self._started = self.clock()

    @property
    def remaining_seconds(self) -> float:
        """The time left, which is never negative."""
        return max(0.0, self.limits.seconds - (self.clock() - self._started))

    @property
    def remaining_input_tokens(self) -> int:
        """The input tokens left, which is never negative."""
        return max(0, self.limits.input_tokens - self.input_tokens)

    def check_time(self) -> None:
        """Raise `BudgetExhaustedError` when the wall clock is spent."""
        if self.remaining_seconds <= 0:
            raise BudgetExhaustedError("time", f"the {self.limits.seconds:g} second time budget is spent")

    def check_tool_call(self) -> None:
        """Raise when another tool call is not allowed."""
        self.check_time()
        if self.tool_calls >= self.limits.tool_calls:
            raise BudgetExhaustedError("tool calls", f"all {self.limits.tool_calls} tool calls are spent")

    def spend_tool_call(self) -> None:
        """Count one tool call, or raise when the budget has no call left."""
        self.check_tool_call()
        self.tool_calls += 1

    def check_model_call(self) -> None:
        """Raise when another model call is not allowed."""
        self.check_time()
        if self.model_calls >= self.limits.model_calls:
            raise BudgetExhaustedError("model calls", f"all {self.limits.model_calls} model calls are spent")
        if self.input_tokens >= self.limits.input_tokens:
            raise BudgetExhaustedError(
                "input tokens",
                f"{self.input_tokens} input tokens read; the budget is {self.limits.input_tokens}",
            )

    def spend_model_call(self) -> None:
        """Count one model call, or raise when the budget has no call left."""
        self.check_model_call()
        self.model_calls += 1

    def check_input_estimate(self, tokens: int) -> None:
        """Raise when a pre-flight input estimate would exceed the input-token budget."""
        self.check_time()
        if self.input_tokens + tokens > self.limits.input_tokens:
            raise BudgetExhaustedError(
                "input tokens",
                f"the next call would read about {tokens} input tokens and only "
                f"{self.remaining_input_tokens} remain",
            )

    def spend_input_tokens(self, tokens: int) -> None:
        """Count the input tokens of one model call."""
        self.input_tokens += max(0, tokens)
