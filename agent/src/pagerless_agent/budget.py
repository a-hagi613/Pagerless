"""Per-turn budgets and the estimates that make an omitted provider usage harmless."""

from __future__ import annotations

import math
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from pagerless_agent.model import Message

# Characters per token assumed for prose and JSON. Both round up on purpose, because a
# failed estimate must not be able to hide spend.
PROSE_CHARS_PER_TOKEN = 3.6
JSON_CHARS_PER_TOKEN = 3.0


def estimate_tokens(text: str, *, json: bool = False) -> int:
    """Return a pessimistic token estimate for a string."""
    if not text:
        return 0
    return math.ceil(len(text) / (JSON_CHARS_PER_TOKEN if json else PROSE_CHARS_PER_TOKEN))


def estimate_messages_tokens(messages: tuple[Message, ...]) -> int:
    """Estimate the input tokens of a conversation, counting tool arguments as JSON."""
    total = 0
    for message in messages:
        total += estimate_tokens(message.content)
        for call in message.tool_calls:
            total += estimate_tokens(call.name) + estimate_tokens(str(call.arguments), json=True)
    return total


@dataclass(frozen=True)
class Limits:
    """The ceilings one turn may not exceed."""

    tool_calls: int = 15
    model_calls: int = 12
    input_tokens: int = 300_000
    seconds: float = 300.0


class BudgetExhaustedError(Exception):
    """A ceiling was reached. `resource` names it: tool calls, model calls, input tokens or time."""

    def __init__(self, resource: str, detail: str) -> None:
        super().__init__(detail)
        self.resource = resource
        self.detail = detail


@dataclass
class Budget:
    """Tracks what one turn has spent against its four ceilings."""

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
        """Raise when the wall clock is spent."""
        if self.remaining_seconds <= 0:
            raise BudgetExhaustedError("time", f"the {self.limits.seconds:g} second time budget is spent")

    def check_tool_call(self) -> None:
        """Raise when another tool call is not allowed."""
        self.check_time()
        if self.tool_calls >= self.limits.tool_calls:
            raise BudgetExhaustedError("tool calls", f"all {self.limits.tool_calls} tool calls are spent")

    def spend_tool_call(self) -> None:
        """Count one tool call, or raise when the budget has none left."""
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
        """Count one model HTTP attempt, or raise when the budget has none left."""
        self.check_model_call()
        self.model_calls += 1

    def check_input_estimate(self, tokens: int) -> None:
        """Raise when a pre-flight estimate would exceed the input-token budget."""
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
