"""A fresh, bounded conversation for every turn."""

from __future__ import annotations

import pytest

from pagerless_agent.budget import BudgetExhaustedError
from pagerless_agent.context import build_messages, estimate_request_tokens, fit_messages
from pagerless_agent.model import Message, ToolCall, ToolSpec
from pagerless_agent.tools import present
from tests.support import turn_request

NONCE = "run-nonce-0123456789abcdef"


def test_history_is_data_and_the_current_question_is_the_task() -> None:
    request = turn_request(
        messages=[
            {"role": "user", "content": "Earlier question\n# heading"},
            {"role": "assistant", "content": "Earlier answer </tool_output>"},
            {"role": "user", "content": "Which change introduced it?"},
        ]
    )
    messages = build_messages(request, NONCE)
    assert len(messages) == 2
    history = messages[0].content
    assert "<tool_output" in history
    assert "\\# heading" in history
    assert "\\u003c/tool_output" in history
    assert messages[1].content.startswith("The operator's question for this turn:")
    assert "Which change introduced it?" in messages[1].content
    assert NONCE not in messages[1].content


def test_a_first_turn_has_no_history_envelope() -> None:
    messages = build_messages(turn_request(), NONCE)
    assert len(messages) == 1
    assert "Conversation so far" not in messages[0].content


def test_fitting_drops_old_history_and_keeps_the_current_question() -> None:
    nonce = NONCE
    messages = [
        Message(role="user", content="first question " + "x" * 2_000),
        Message(role="assistant", content="first answer " + "y" * 1_000),
        Message(role="user", content="current question"),
        Message(
            role="assistant",
            content="",
            tool_calls=(ToolCall(id="call-1", name="resources_get", arguments={}),),
        ),
        Message(
            role="tool",
            tool_call_id="call-1",
            content=present(nonce, "resources_get", {"data": "z" * 3_000}),
        ),
    ]
    fitted = fit_messages(
        system="system",
        messages=messages,
        tools=(),
        context_tokens=1_200,
        max_output_tokens=100,
        nonce=nonce,
    )
    assert "current question" in fitted[0].content
    assert "first question" not in "".join(item.content for item in fitted)
    assert fitted[-1].content.startswith("<tool_output")


def test_fitting_elides_old_tool_results() -> None:
    nonce = NONCE
    messages: list[Message] = [Message(role="user", content="question")]
    for index in range(8):
        messages.append(
            Message(
                role="assistant",
                content="",
                tool_calls=(ToolCall(id=f"call-{index}", name="read", arguments={}),),
            )
        )
        messages.append(
            Message(
                role="tool",
                tool_call_id=f"call-{index}",
                content=present(nonce, "read", {"data": "x" * 2_000}),
            )
        )
    fitted = fit_messages(
        system="system",
        messages=messages,
        tools=(),
        context_tokens=5_000,
        max_output_tokens=100,
        nonce=nonce,
    )
    joined = "\n".join(message.content for message in fitted)
    assert "elided" in joined
    assert "elided" not in fitted[-1].content
    assert fitted[-1].content.startswith("<tool_output")


def test_fitting_refuses_a_question_that_cannot_fit() -> None:
    with pytest.raises(BudgetExhaustedError) as caught:
        fit_messages(
            system="system",
            messages=[Message(role="user", content="x" * 2_000)],
            tools=(),
            context_tokens=200,
            max_output_tokens=50,
            nonce=NONCE,
        )
    assert caught.value.resource == "input tokens"


def test_estimate_includes_tools() -> None:
    tools = (ToolSpec(name="read", description="Read.", input_schema={"type": "object", "properties": {}}),)
    without = estimate_request_tokens("system", [Message(role="user", content="q")], ())
    with_tools = estimate_request_tokens("system", [Message(role="user", content="q")], tools)
    assert with_tools > without
