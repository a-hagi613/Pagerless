"""The LangGraph turn: model decisions alternating with bounded, read-only MCP reads.

    think -> act -> think -> ... -> submit_answer

`think` asks the model for its next action. `act` executes evidence reads through the Go
callback, reads an optional playbook, or validates the terminal `submit_answer`. Every
factual claim must cite a callId from this turn, and every citation is resolved locally
before the answer is accepted. There is no fixed diagnosis, no resource-kind menu and no
mutation: the next read is the model's decision, and the Go tool policy remains the
authority on what may be read.
"""

from __future__ import annotations

import asyncio
import logging
import secrets
import time
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any, TypedDict

from langgraph.graph import END, START, StateGraph
from pydantic import ValidationError

from pagerless_agent import playbooks, prompts
from pagerless_agent.budget import Budget, BudgetExhaustedError, Limits
from pagerless_agent.callback import (
    CallbackError,
    ToolExecutor,
    ToolOutcome,
    ToolProtocolError,
    ToolRejectedError,
    ToolUnavailableError,
)
from pagerless_agent.context import build_messages, fit_messages
from pagerless_agent.contract import (
    MAX_ANSWER_CHARS,
    AnswerPayload,
    Citation,
    Proposal,
    TurnError,
    TurnRequest,
    TurnResult,
    TurnUsage,
    validation_message,
)
from pagerless_agent.model import Message, ModelProvider, ModelRequest, ModelResponse, ToolCall, ToolSpec
from pagerless_agent.openai_compat import (
    ProviderAuthError,
    ProviderError,
    ProviderMalformedError,
    ProviderRejectedError,
    ProviderTimeoutError,
    ProviderTruncatedError,
    ProviderUnavailableError,
)
from pagerless_agent.tools import (
    PointerError,
    SuspicionTracker,
    bounded_arguments,
    compact_json,
    jsonable,
    local_tool_specs,
    new_nonce,
    normalize_file_content,
    present,
    present_error,
    repository_path_problem,
    resolve_pointer,
    sanitize_text,
    withheld,
)

log = logging.getLogger(__name__)

SUBMIT_ANSWER = "submit_answer"
READ_PLAYBOOK = "read_playbook"
MAX_SUBMIT_ATTEMPTS = 2
MAX_NUDGES = 2
MAX_ERROR_CHARS = 500


class _State(TypedDict, total=False):
    messages: list[Message]
    pending: list[ToolCall]
    submit_attempts: int
    nudges: int
    outcome: TurnResult | None


@dataclass
class _Journal:
    """What the turn has produced so far, including a partial answer for cancellation."""

    input_tokens: int = 0
    output_tokens: int = 0
    latest_text: str = ""

    def observe(self, response: ModelResponse) -> None:
        self.input_tokens += response.usage.input_tokens
        self.output_tokens += response.usage.output_tokens
        if response.text.strip():
            self.latest_text = response.text


class Agent:
    """One turn: the graph and everything it depends on."""

    def __init__(
        self,
        *,
        request: TurnRequest,
        provider: ModelProvider,
        gateway: ToolExecutor,
        limits: Limits,
        tool_timeout_s: float,
        context_tokens: int,
        max_output_tokens: int,
        clock: Callable[[], float] = time.monotonic,
        nonce: str | None = None,
        call_id_factory: Callable[[], str] | None = None,
    ) -> None:
        self._request = request
        self._provider = provider
        self._gateway = gateway
        self._limits = limits
        self._tool_timeout_s = tool_timeout_s
        self._context_tokens = context_tokens
        self._max_output_tokens = max_output_tokens
        self._nonce = nonce or new_nonce()
        self._next_call_id = call_id_factory or _new_call_id
        self._budget = Budget(limits=limits, clock=clock)
        self._journal = _Journal()
        self._suspicion = SuspicionTracker(nonce=self._nonce)
        self._catalogue = frozenset(tool.name for tool in request.tools)
        self._submit, self._playbook = local_tool_specs()
        forwarded = tuple(
            ToolSpec(name=tool.name, description=tool.description, input_schema=tool.input_schema)
            for tool in request.tools
        )
        self._tools = (*forwarded, self._submit, self._playbook)
        self._system = prompts.system_prompt(request, limits)
        self._messages = build_messages(request, self._nonce)
        self._results: dict[str, Any] = {}

    # Lifecycle

    async def run(self) -> TurnResult:
        """Run the turn. It never raises for a failed investigation."""
        try:
            async with asyncio.timeout(self._limits.seconds):
                final = await self._graph().ainvoke(
                    {
                        "messages": self._messages,
                        "pending": [],
                        "submit_attempts": 0,
                        "nudges": 0,
                    },
                    config={"recursion_limit": _recursion_limit(self._limits)},
                )
            outcome = final.get("outcome")
            if not isinstance(outcome, TurnResult):  # pragma: no cover - the graph always ends with one
                return self.failure("internal_error", "the turn produced no outcome")
            return outcome
        except TimeoutError:
            return self.failure("budget_exhausted", "the turn's wall-clock budget is spent")
        except BudgetExhaustedError as exhausted:
            return self.failure(
                "budget_exhausted",
                f"Budget exhausted ({exhausted.resource}): {exhausted.detail}",
            )
        except ProviderAuthError as error:
            return self.failure("provider_auth", str(error))
        except ProviderRejectedError as error:
            code = (
                "provider_tools_unsupported" if error.reason == "tools_unsupported" else "provider_rejected"
            )
            return self.failure(code, str(error))
        except ProviderTimeoutError as error:
            return self.failure("provider_timeout", str(error))
        except ProviderUnavailableError as error:
            return self.failure("provider_unavailable", str(error))
        except ProviderMalformedError as error:
            return self.failure("provider_malformed", str(error))
        except ProviderTruncatedError as error:
            return self.failure("provider_truncated", str(error))
        except ToolRejectedError as error:
            return self.failure("tool_rejected", str(error))
        except ToolUnavailableError as error:
            return self.failure("tool_unavailable", str(error))
        except ToolProtocolError as error:
            return self.failure("tool_malformed", str(error))
        except CallbackError as error:
            return self.failure(error.code, str(error))
        except ProviderError as error:
            return self.failure("provider_error", str(error))
        except Exception as error:
            log.exception("the turn failed unexpectedly")
            return self.failure("internal_error", f"the agent failed ({type(error).__name__})")

    def stopped_result(self) -> TurnResult:
        """Return the visible result after cancellation. It never claims unverified evidence."""
        content = sanitize_text(self._journal.latest_text, MAX_ANSWER_CHARS)
        return TurnResult(status="stopped", content=content, citations=[], usage=self._usage())

    def failure(self, code: str, message: str) -> TurnResult:
        """Return a terminal failure with a bounded, sanitized reason."""
        return TurnResult(
            status="failed",
            content="",
            citations=[],
            error=TurnError(code=code, message=_one_line(message) or "the turn failed"),
            usage=self._usage(),
        )

    def _usage(self) -> TurnUsage:
        return TurnUsage(
            input_tokens=self._journal.input_tokens,
            output_tokens=self._journal.output_tokens,
            model_calls=self._budget.model_calls,
            tool_calls=self._budget.tool_calls,
        )

    # Graph

    def _graph(self) -> Any:
        graph = StateGraph(_State)
        graph.add_node("think", self._think)
        graph.add_node("act", self._act)
        graph.add_edge(START, "think")
        graph.add_conditional_edges(
            "think",
            _after_think,
            {"act": "act", "think": "think", END: END},
        )
        graph.add_conditional_edges(
            "act",
            lambda state: END if state.get("outcome") is not None else "think",
            {"think": "think", END: END},
        )
        return graph.compile()

    async def _think(self, state: _State) -> _State:
        messages = list(state["messages"])
        response = await self._ask(self._fit(messages))
        self._journal.observe(response)
        assistant = Message(role="assistant", content=response.text, tool_calls=response.tool_calls)
        if response.tool_calls:
            return {"messages": [*messages, assistant], "pending": list(response.tool_calls)}
        if response.text.strip():
            return {
                "messages": [*messages, assistant],
                "pending": [],
                "outcome": TurnResult(
                    status="completed",
                    content=sanitize_text(response.text, MAX_ANSWER_CHARS),
                    citations=[],
                    usage=self._usage(),
                ),
            }
        nudges = state.get("nudges", 0)
        if nudges >= MAX_NUDGES:
            return {
                "messages": messages,
                "pending": [],
                "nudges": nudges + 1,
                "outcome": self.failure("empty_response", "the model returned no answer after two nudges"),
            }
        return {
            "messages": [*messages, Message(role="user", content=prompts.nudge())],
            "pending": [],
            "nudges": nudges + 1,
        }

    async def _act(self, state: _State) -> _State:
        messages = list(state["messages"])
        attempts = state.get("submit_attempts", 0)
        outcome: TurnResult | None = None
        for call in state.get("pending", []):
            if call.name == SUBMIT_ANSWER:
                result, problems = self._submit_answer(call)
                attempts += 1
                if result is not None:
                    outcome = result
                    break
                messages.append(
                    Message(
                        role="tool",
                        tool_call_id=call.id,
                        content=present_error(
                            self._nonce,
                            SUBMIT_ANSWER,
                            "invalid_answer",
                            _answer_repair_message(problems),
                        ),
                    )
                )
                if attempts >= MAX_SUBMIT_ATTEMPTS:
                    outcome = self.failure(_answer_error_code(problems), "; ".join(problems))
                    break
                continue
            if call.name == READ_PLAYBOOK:
                messages.append(self._playbook_message(call))
                continue
            messages.append(await self._evidence_message(call))
        return {
            "messages": messages,
            "pending": [],
            "submit_attempts": attempts,
            "outcome": outcome,
        }

    # Model calls

    async def _ask(self, messages: list[Message]) -> ModelResponse:
        request = ModelRequest(
            system=self._system,
            messages=tuple(messages),
            tools=self._tools,
            max_output_tokens=self._max_output_tokens,
        )
        self._budget.check_model_call()
        self._budget.check_input_estimate(self._provider.estimate_input_tokens(request))
        response = await self._provider.complete(request, budget=self._budget)
        self._budget.spend_input_tokens(response.usage.input_tokens)
        return response

    def _fit(self, messages: list[Message]) -> list[Message]:
        return fit_messages(
            system=self._system,
            messages=messages,
            tools=self._tools,
            context_tokens=self._context_tokens,
            max_output_tokens=self._max_output_tokens,
            nonce=self._nonce,
        )

    # Tools

    async def _evidence_message(self, call: ToolCall) -> Message:
        self._budget.spend_tool_call()
        if call.argument_error is not None:
            return _tool_message(
                call,
                present_error(self._nonce, call.name, "invalid_arguments", call.argument_error),
            )
        arguments, problem = bounded_arguments(call.arguments)
        if problem is not None:
            return _tool_message(call, present_error(self._nonce, call.name, "invalid_arguments", problem))
        if call.name not in self._catalogue:
            self._suspicion.observe_tool_attempt(call.name, self._catalogue)
            return _tool_message(
                call,
                present_error(
                    self._nonce,
                    call.name,
                    "unknown_tool",
                    f"There is no tool named {call.name!r} in this installation. Use a listed tool.",
                ),
            )
        self._budget.check_time()
        call_id = self._unique_call_id()
        timeout = min(self._tool_timeout_s, max(0.0, self._budget.remaining_seconds))
        log.info("turn %s calling tool %s as %s", self._request.turn_id, call.name, call_id)
        outcome = await self._gateway.call(
            turn_id=self._request.turn_id,
            call_id=call_id,
            tool=call.name,
            arguments=arguments,
            timeout_s=timeout,
        )
        if outcome.status == "budget_exhausted":
            raise BudgetExhaustedError("tool calls", "the tool policy reported its tool-call budget is spent")
        if outcome.is_error:
            return _tool_message(
                call,
                present_error(self._nonce, call.name, f"tool_{outcome.status}", _outcome_error(outcome)),
            )
        ok, converted = jsonable(outcome.result)
        if not ok:
            return _tool_message(
                call,
                present_error(
                    self._nonce,
                    call.name,
                    "tool_malformed",
                    "The tool result was not finite JSON and was discarded.",
                ),
            )
        try:
            encoded = compact_json(converted)
        except TypeError, ValueError:
            return _tool_message(
                call,
                present_error(
                    self._nonce,
                    call.name,
                    "tool_malformed",
                    "The tool result was not finite JSON and was discarded.",
                ),
            )
        if self._suspicion.observe_payload(encoded):
            for event in self._suspicion.drain():
                log.warning("withholding a tool result: %s", event)
            return _tool_message(
                call,
                withheld(
                    self._nonce,
                    call.name,
                    "The result probed the integrity boundary and was withheld.",
                ),
            )
        self._results[call_id] = converted
        payload: dict[str, Any] = (
            {**converted, "callId": call_id}
            if isinstance(converted, dict)
            else {"callId": call_id, "result": converted}
        )
        envelope = present(
            self._nonce,
            call.name,
            payload,
            redacted=outcome.redacted,
            truncated=outcome.truncated,
        )
        return _tool_message(call, envelope)

    def _playbook_message(self, call: ToolCall) -> Message:
        self._budget.spend_tool_call()
        if call.argument_error is not None:
            return _tool_message(
                call,
                present_error(self._nonce, READ_PLAYBOOK, "invalid_arguments", call.argument_error),
            )
        name = call.arguments.get("name")
        if not isinstance(name, str) or not name:
            return _tool_message(
                call,
                present_error(
                    self._nonce,
                    READ_PLAYBOOK,
                    "invalid_arguments",
                    "The name was not a playbook name.",
                ),
            )
        playbook = playbooks.get(name)
        if playbook is None:
            available = ", ".join(item.name for item in playbooks.catalogue())
            return _tool_message(
                call,
                present_error(
                    self._nonce,
                    READ_PLAYBOOK,
                    "unknown_playbook",
                    f"There is no playbook named {name!r}. Available: {available}.",
                ),
            )
        return _tool_message(
            call,
            present(
                self._nonce,
                READ_PLAYBOOK,
                {"name": playbook.name, "summary": playbook.summary, "body": playbook.body},
            ),
        )

    def _unique_call_id(self) -> str:
        for _ in range(3):
            candidate = self._next_call_id()
            if candidate not in self._results:
                return candidate
        raise RuntimeError("the call id factory repeated itself")

    # Submission

    def _submit_answer(self, call: ToolCall) -> tuple[TurnResult | None, list[str]]:
        if call.argument_error is not None:
            return None, [call.argument_error]
        try:
            payload = AnswerPayload.model_validate(call.arguments)
        except ValidationError as error:
            return None, [validation_message(error)]
        problems: list[str] = []
        if len(payload.content) > MAX_ANSWER_CHARS:
            problems.append(f"content: must be at most {MAX_ANSWER_CHARS} characters")
        citations: list[Citation] = []
        seen: set[tuple[str, str | None]] = set()
        for index, citation in enumerate(payload.citations):
            if citation.call_id not in self._results:
                problems.append(
                    f"citations[{index}].callId: no tool call named "
                    f"{citation.call_id!r} was made in this turn"
                )
                continue
            if citation.pointer is not None:
                try:
                    resolve_pointer(self._results[citation.call_id], citation.pointer)
                except PointerError as error:
                    problems.append(f"citations[{index}].pointer: {error}")
                    continue
            key = (citation.call_id, citation.pointer)
            if key not in seen:
                seen.add(key)
                citations.append(citation)
        if payload.proposal is not None:
            problems.extend(_proposal_problems(payload.proposal))
            if not citations:
                problems.append(
                    "proposal: a source proposal must cite the tool evidence that grounds the base and path"
                )
        if problems:
            return None, problems
        content = sanitize_text(payload.content, MAX_ANSWER_CHARS)
        return (
            TurnResult(
                status="completed",
                content=content,
                citations=citations,
                proposal=payload.proposal,
                usage=self._usage(),
            ),
            [],
        )


def _after_think(state: _State) -> str:
    if state.get("outcome") is not None:
        return END
    return "act" if state.get("pending") else "think"


def _tool_message(call: ToolCall, content: str) -> Message:
    return Message(role="tool", tool_call_id=call.id, content=content)


def _new_call_id() -> str:
    return "c-" + secrets.token_hex(16)


def _recursion_limit(limits: Limits) -> int:
    return max(100, 4 * (limits.model_calls + limits.tool_calls) + 20)


def _one_line(message: str) -> str:
    return sanitize_text(" ".join(message.split()), MAX_ERROR_CHARS)


def _outcome_error(outcome: ToolOutcome) -> str:
    result = outcome.result
    if isinstance(result, dict):
        error = result.get("error")
        if isinstance(error, dict) and isinstance(error.get("message"), str):
            return sanitize_text(error["message"], MAX_ERROR_CHARS)
        if isinstance(result.get("message"), str):
            return sanitize_text(result["message"], MAX_ERROR_CHARS)
    return f"The tool returned the status {outcome.status}."


def _answer_repair_message(problems: list[str]) -> str:
    listed = "; ".join(problems)
    return sanitize_text(
        f"The answer was not accepted: {listed}. Call submit_answer again with only real evidence.",
        1_000,
    )


def _answer_error_code(problems: list[str]) -> str:
    joined = " ".join(problems).lower()
    if "proposal" in joined:
        return "proposal_invalid"
    if "citation" in joined or "callid" in joined or "pointer" in joined:
        return "citation_invalid"
    return "answer_invalid"


def _proposal_problems(proposal: Proposal) -> list[str]:
    problems: list[str] = []
    paths: set[str] = set()
    for index, file in enumerate(proposal.files):
        path_problem = repository_path_problem(file.path)
        if path_problem is not None:
            problems.append(f"proposal.files[{index}].path: {path_problem}")
        if file.path in paths:
            problems.append(f"proposal.files[{index}].path: is repeated")
        paths.add(file.path)
        normalized, content_problem = normalize_file_content(file.content)
        if content_problem is not None:
            problems.append(f"proposal.files[{index}].content: {content_problem}")
        elif normalized is not None and normalized != file.content:
            problems.append(f"proposal.files[{index}].content: must use LF line endings without NUL bytes")
    return problems


__all__ = ["Agent", "SUBMIT_ANSWER", "READ_PLAYBOOK"]
