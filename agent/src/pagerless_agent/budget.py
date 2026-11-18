"""The budgets of one investigation (ADR-0010): tool calls, wall-clock time, and input tokens."""

from __future__ import annotations

import time
from collections.abc import Callable
from dataclasses import dataclass, field

MAX_TOOL_CALLS = 15
MAX_SECONDS = 5 * 60
MAX_INPUT_TOKENS = 300_000


@dataclass(frozen=True)
class Limits:
    """How much an investigation may spend."""

    tool_calls: int = MAX_TOOL_CALLS
    seconds: float = MAX_SECONDS
    input_tokens: int = MAX_INPUT_TOKENS


class BudgetExhaustedError(Exception):
    """A budget ran out. `resource` names it: tool calls, time, or input tokens."""

    def __init__(self, resource: str, detail: str) -> None:
        super().__init__(detail)
        self.resource = resource
        self.detail = detail


@dataclass
class Budget:
    """Tracks what an investigation has spent against its limits.

    Input tokens are summed over every model call, counting cached tokens, so
    the figure measures what the investigation read and not what it was billed.
    """

    limits: Limits = field(default_factory=Limits)
    clock: Callable[[], float] = time.monotonic
    tool_calls: int = 0
    input_tokens: int = 0
    _started: float = field(init=False)

    def __post_init__(self) -> None:
        self._started = self.clock()

    @property
    def remaining_seconds(self) -> float:
        """The time left, which is never negative."""
        return max(0.0, self.limits.seconds - (self.clock() - self._started))

    def check_time(self) -> None:
        """Raise BudgetExhaustedError when the time is up."""
        if self.remaining_seconds <= 0:
            raise BudgetExhaustedError("time", f"the {self.limits.seconds:g} second time budget is spent")

    def check_model_call(self) -> None:
        """Raise BudgetExhaustedError when another model call is not allowed."""
        self.check_time()
        if self.input_tokens >= self.limits.input_tokens:
            raise BudgetExhaustedError(
                "input tokens",
                f"{self.input_tokens} input tokens read; the budget is {self.limits.input_tokens}",
            )

    def spend_tool_call(self) -> None:
        """Count one tool call, or raise BudgetExhaustedError when the budget has no call left."""
        self.check_time()
        if self.tool_calls >= self.limits.tool_calls:
            raise BudgetExhaustedError("tool calls", f"all {self.limits.tool_calls} tool calls are spent")
        self.tool_calls += 1

    def spend_input_tokens(self, tokens: int) -> None:
        """Count the input tokens of one model call."""
        self.input_tokens += tokens
