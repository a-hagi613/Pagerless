"""Build a fresh conversation for one turn and keep it inside the model's context.

Each turn starts from the stored messages. Earlier turns are data, not trusted evidence,
so they travel in an envelope; only the current operator message directs the work. This is
what makes a follow-up safe: the model sees the thread, but citations must still name a
tool result from the current turn.
"""

from __future__ import annotations

import json
from typing import Any

from pagerless_agent import prompts
from pagerless_agent.budget import BudgetExhaustedError, estimate_messages_tokens, estimate_tokens
from pagerless_agent.contract import MAX_MESSAGE_CHARS, TurnRequest
from pagerless_agent.model import Message, ToolSpec
from pagerless_agent.tools import present, sanitize_text

MAX_HISTORY_ENVELOPE_CHARS = 24_000
KEEP_RECENT_TOOL_RESULTS = 6
CONTEXT_PRESSURE = 0.9


def build_messages(request: TurnRequest, nonce: str) -> list[Message]:
    """Return the opening conversation for this turn."""
    history = request.messages[:-1]
    messages: list[Message] = []
    if history:
        rendered: list[dict[str, Any]] = [
            {
                "role": message.role,
                "content": sanitize_text(message.content, MAX_MESSAGE_CHARS),
                "turnId": message.turn_id,
            }
            for message in history
        ]
        envelope = present(
            nonce,
            "conversation history",
            rendered,
            limit=MAX_HISTORY_ENVELOPE_CHARS,
        )
        messages.append(Message(role="user", content=prompts.history_framing(envelope)))
    current = sanitize_text(request.messages[-1].content, MAX_MESSAGE_CHARS)
    messages.append(Message(role="user", content=prompts.operator_turn(current)))
    return messages


def estimate_request_tokens(system: str, messages: list[Message], tools: tuple[ToolSpec, ...]) -> int:
    """Return a pessimistic input estimate for one request."""
    total = estimate_tokens(system)
    total += estimate_messages_tokens(tuple(messages))
    if tools:
        encoded = json.dumps([tool.model_dump(mode="json") for tool in tools], sort_keys=True)
        total += estimate_tokens(encoded, json=True)
    return total


def fit_messages(
    *,
    system: str,
    messages: list[Message],
    tools: tuple[ToolSpec, ...],
    context_tokens: int,
    max_output_tokens: int,
    nonce: str,
) -> list[Message]:
    """Elide old tool results and drop old history until the request fits.

    The current operator question and the most recent tool step are never dropped. If they
    alone exceed the window, the turn fails visibly rather than sending a truncated task.
    """
    budget = max(0, int(context_tokens * CONTEXT_PRESSURE) - max_output_tokens)
    if estimate_request_tokens(system, messages, tools) <= budget:
        return list(messages)

    elided = _elide_tool_results(messages, nonce)
    if estimate_request_tokens(system, elided, tools) <= budget:
        return elided

    units = _units(elided)
    pinned = _pinned_unit(units)
    while pinned > 0 and estimate_request_tokens(system, _flatten(units), tools) > budget:
        units.pop(0)
        pinned -= 1
    flattened = _flatten(units)
    if estimate_request_tokens(system, flattened, tools) > budget:
        raise BudgetExhaustedError(
            "input tokens",
            f"the current question and latest evidence cannot fit a {context_tokens}-token context window",
        )
    return flattened


def _elide_tool_results(messages: list[Message], nonce: str) -> list[Message]:
    indexes = [index for index, message in enumerate(messages) if message.role == "tool"]
    keep = set(indexes[-KEEP_RECENT_TOOL_RESULTS:]) if KEEP_RECENT_TOOL_RESULTS else set()
    elided: list[Message] = []
    for index, message in enumerate(messages):
        if message.role == "tool" and index not in keep:
            marker = present(
                nonce,
                "elided tool result",
                {"elided": True, "reason": "context", "toolCallId": message.tool_call_id},
                limit=600,
            )
            elided.append(message.model_copy(update={"content": marker}))
        else:
            elided.append(message)
    return elided


def _units(messages: list[Message]) -> list[list[Message]]:
    units: list[list[Message]] = []
    for message in messages:
        if message.role == "tool" and units and units[-1][0].role == "assistant":
            units[-1].append(message)
        else:
            units.append([message])
    return units


def _flatten(units: list[list[Message]]) -> list[Message]:
    return [message for unit in units for message in unit]


def _pinned_unit(units: list[list[Message]]) -> int:
    """Return the index of the most recent user-unit, which starts the current question."""
    for index in range(len(units) - 1, -1, -1):
        if units[index][0].role == "user":
            return index
    return max(0, len(units) - 1)
