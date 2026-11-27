"""Budget accounting and pessimistic token estimates."""

from __future__ import annotations

import pytest

from pagerless_agent.budget import (
    Budget,
    BudgetExhaustedError,
    Limits,
    estimate_messages_tokens,
    estimate_tokens,
)
from pagerless_agent.model import Message, ToolCall


class Clock:
    """A manually advanced monotonic clock."""

    def __init__(self) -> None:
        self.now = 100.0

    def __call__(self) -> float:
        return self.now


def test_estimates_round_up_and_count_tool_arguments() -> None:
    assert estimate_tokens("") == 0
    assert estimate_tokens("abc") == 1
    assert estimate_tokens("x" * 100) == 28
    assert estimate_tokens("x" * 30, json=True) == 10
    message = Message(
        role="assistant",
        content="x" * 100,
        tool_calls=(ToolCall(id="c1", name="read", arguments={"path": "a" * 30}),),
    )
    assert estimate_messages_tokens((message,)) > estimate_tokens(message.content)


def test_tool_budget_counts_and_stops() -> None:
    budget = Budget(limits=Limits(tool_calls=1))
    budget.spend_tool_call()
    with pytest.raises(BudgetExhaustedError) as caught:
        budget.spend_tool_call()
    assert caught.value.resource == "tool calls"


def test_model_budget_counts_and_stops() -> None:
    budget = Budget(limits=Limits(model_calls=1, input_tokens=100))
    budget.spend_model_call()
    with pytest.raises(BudgetExhaustedError):
        budget.spend_model_call()
    assert budget.model_calls == 1


def test_input_token_budget_is_checked_before_and_after() -> None:
    budget = Budget(limits=Limits(input_tokens=100))
    budget.check_input_estimate(60)
    budget.spend_input_tokens(60)
    with pytest.raises(BudgetExhaustedError) as caught:
        budget.check_input_estimate(50)
    assert caught.value.resource == "input tokens"
    budget.spend_input_tokens(50)
    assert budget.remaining_input_tokens == 0
    with pytest.raises(BudgetExhaustedError):
        budget.check_model_call()


def test_wall_clock_budget_expires() -> None:
    clock = Clock()
    budget = Budget(limits=Limits(seconds=10), clock=clock)
    assert budget.remaining_seconds == 10
    clock.now = 105.0
    assert budget.remaining_seconds == 5
    clock.now = 111.0
    assert budget.remaining_seconds == 0
    with pytest.raises(BudgetExhaustedError) as caught:
        budget.spend_tool_call()
    assert caught.value.resource == "time"
