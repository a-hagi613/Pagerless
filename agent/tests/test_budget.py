"""The four budgets and the context shedding that keeps them."""

from __future__ import annotations

import pytest

from pagerless_agent.budget import (
    KEEP_TOOL_RESULTS,
    MAX_INPUT_TOKENS,
    MAX_MODEL_CALLS,
    MAX_SECONDS,
    MAX_TOOL_CALLS,
    Budget,
    BudgetExhaustedError,
    Limits,
    estimate_tokens,
    shed_messages,
)
from pagerless_agent.model import Message


class Clock:
    """A clock a test advances by hand."""

    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now


def test_the_defaults_are_the_documented_four_budgets() -> None:
    limits = Limits()

    assert limits.tool_calls == 15
    assert limits.model_calls == 12
    assert limits.input_tokens == 300_000
    assert limits.seconds == 300
    assert (MAX_TOOL_CALLS, MAX_MODEL_CALLS, MAX_INPUT_TOKENS, MAX_SECONDS) == (15, 12, 300_000, 300)


def test_the_fifteenth_tool_call_is_allowed_and_the_sixteenth_is_not() -> None:
    budget = Budget(limits=Limits(tool_calls=2))

    budget.spend_tool_call()
    budget.spend_tool_call()
    with pytest.raises(BudgetExhaustedError, match="tool calls"):
        budget.spend_tool_call()


def test_the_twelfth_model_call_is_allowed_and_the_thirteenth_is_not() -> None:
    budget = Budget(limits=Limits(model_calls=2))

    budget.spend_model_call()
    budget.spend_model_call()
    with pytest.raises(BudgetExhaustedError, match="model calls"):
        budget.spend_model_call()


def test_a_pre_flight_estimate_that_would_overspend_is_refused() -> None:
    budget = Budget(limits=Limits(input_tokens=100))

    budget.spend_input_tokens(80)
    budget.check_input_estimate(20)
    with pytest.raises(BudgetExhaustedError, match="input tokens"):
        budget.check_input_estimate(21)


def test_the_wall_clock_is_enforced_with_a_monotonic_clock() -> None:
    clock = Clock()
    budget = Budget(limits=Limits(seconds=10), clock=clock)

    assert budget.remaining_seconds == 10
    clock.now += 9.5
    assert budget.remaining_seconds == 0.5
    clock.now += 1
    with pytest.raises(BudgetExhaustedError, match="time budget"):
        budget.check_time()
    assert budget.remaining_seconds == 0


def test_estimate_tokens_is_pessimistic_for_prose_and_json() -> None:
    assert estimate_tokens("") == 0
    prose = estimate_tokens("a" * 36)
    structured = estimate_tokens('{"a": 1}' * 30, json=True)

    assert prose == 10
    assert structured >= 60
    assert estimate_tokens("a" * 40, json=True) == 14


def tool_message(index: int, body: str = "x" * 4000) -> Message:
    return Message(role="tool", content=body, tool_call_id=f"call-{index}")


def test_shedding_elides_the_oldest_tool_results_and_truncates_the_rest() -> None:
    messages = [Message(role="user", content="task")]
    messages += [tool_message(index) for index in range(KEEP_TOOL_RESULTS + 2)]
    messages.append(Message(role="assistant", content="plan"))

    shed = shed_messages(messages, keep_tool_results=KEEP_TOOL_RESULTS, truncate_to=100)

    assert shed[0] == messages[0]
    assert shed[-1] == messages[-1]
    elided = [message for message in shed if '"elided": true' in message.content]
    assert len(elided) == 2
    assert all('"reason": "context"' in message.content for message in elided)
    kept = [message for message in shed if message.role == "tool" and '"elided"' not in message.content]
    assert len(kept) == KEEP_TOOL_RESULTS
    assert all('"truncated": true' in message.content for message in kept)
    assert all(len(message.content) < 1000 for message in kept)


def test_shedding_never_touches_user_or_assistant_turns() -> None:
    messages = [
        Message(role="user", content="task"),
        tool_message(0),
        Message(role="assistant", content="plan"),
    ]

    shed = shed_messages(messages, keep_tool_results=0, truncate_to=10)

    assert shed[0] == messages[0]
    assert shed[2] == messages[2]
