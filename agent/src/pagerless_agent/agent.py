"""The agent graph.

The graph is fixed (spec #1, ADR-0001):

    gather -> investigate <-> run_tools -> diagnose -> propose -> submit
                                              revise <-+-----------+

1. `gather` collects the Context Bundle with deterministic code.
2. `investigate` and `run_tools` form a bounded tool loop.
3. `diagnose` writes the Diagnosis with cited Findings.
4. `propose` writes the Proposal as structured output, validated against the per-Action schema.
5. `submit` posts the report. If the Proposal is refused, by the schema or by the server,
   `revise` asks for one revised Proposal. A second refusal ends the run.

A budget that runs out, a server that refuses the agent, or a failure ends the run with
no report. The Findings that the agent already posted as step events stay with the Triage,
and pagerless-server then ends the Triage as Failed.
"""

from __future__ import annotations

import asyncio
import json
import logging
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Literal, TypedDict

from langgraph.graph import END, START, StateGraph
from pydantic import ValidationError

from pagerless_agent import prompts
from pagerless_agent.budget import Budget, BudgetExhaustedError, Limits
from pagerless_agent.bundle import ContextBundle
from pagerless_agent.contract import (
    MAX_FINDINGS,
    MAX_STEP_SUMMARY_LENGTH,
    MAX_TOOL_NAME_LENGTH,
    PROPOSED_ACTION,
    DiagnosisOutput,
    Finding,
    Report,
    ReportError,
    StepEvent,
    StepKind,
    Usage,
    normalize_proposal,
    proposal_tool_schema,
    validation_errors,
)
from pagerless_agent.model import (
    Message,
    ModelProvider,
    ModelRequest,
    ModelResponse,
    Stage,
    ToolCall,
    ToolSpec,
)
from pagerless_agent.pricing import HAIKU_5_5_PRICES, Prices, cost_usd
from pagerless_agent.server import ServerClient, ServerError, ServerRefusedError
from pagerless_agent.settings import Target
from pagerless_agent.tools import ContextSource, ToolRegistry, UnknownToolError, new_nonce, present

log = logging.getLogger(__name__)

# The agent submits a Proposal and, if it is refused, one revision.
MAX_PROPOSAL_ATTEMPTS = 2
# Bounds the supersteps of one run, so that a model that never stops calling tools cannot loop for ever.
_RECURSION_LIMIT = 120
# The Context Bundle is rendered within this many characters.
_MAX_BUNDLE_CHARS = 20_000

OutcomeKind = Literal["reported", "budget_exhausted", "proposal_refused", "rejected", "failed"]


@dataclass(frozen=True)
class Outcome:
    """How a run ended."""

    kind: OutcomeKind
    detail: str
    phase: str | None = None

    @property
    def exit_code(self) -> int:
        """The process exit code. A Job that exits non-zero without a report ends the Triage as Failed."""
        return {"reported": 0, "failed": 1, "budget_exhausted": 2, "proposal_refused": 3, "rejected": 4}[
            self.kind
        ]


class _State(TypedDict, total=False):
    bundle: ContextBundle
    messages: list[Message]
    findings: list[Finding]
    pending: list[ToolCall]
    diagnosis: str
    attempt: int
    proposal: Any
    errors: list[ReportError]
    outcome: Outcome


def _truncate(text: str, limit: int) -> str:
    return text if len(text) <= limit else text[: limit - 1] + "…"


class Agent:
    """One investigation: the graph and everything it depends on."""

    def __init__(
        self,
        *,
        target: Target,
        provider: ModelProvider,
        tools: ToolRegistry,
        source: ContextSource,
        server: ServerClient,
        limits: Limits | None = None,
        clock: Callable[[], float] = time.monotonic,
        nonce: str | None = None,
        prices: Prices = HAIKU_5_5_PRICES,
    ) -> None:
        self._target = target
        self._provider = provider
        self._tools = tools
        self._source = source
        self._server = server
        self._limits = limits or Limits()
        self._budget = Budget(limits=self._limits, clock=clock)
        self._input_tokens = 0
        self._output_tokens = 0
        self._cost_usd = 0.0
        self._nonce = nonce or new_nonce()
        self._prices = prices
        self._record_finding = ToolSpec(
            name=prompts.RECORD_FINDING,
            description=(
                "Record one Finding: a cited piece of evidence that supports the Diagnosis. "
                "It does not use up a tool call."
            ),
            input_schema=Finding.model_json_schema(),
        )
        names = {spec.name for spec in tools.specs()}
        if prompts.RECORD_FINDING in names:
            raise ValueError(f"a tool may not be named {prompts.RECORD_FINDING}")
        self._specs = (*tools.specs(), self._record_finding)
        self._system = prompts.system_prompt(self._limits)

    # The graph.

    def graph(self) -> Any:
        """Build and compile the graph."""
        g = StateGraph(_State)
        g.add_node("gather", self._gather)
        g.add_node("investigate", self._investigate)
        g.add_node("run_tools", self._run_tools)
        g.add_node("diagnose", self._diagnose)
        g.add_node("propose", self._propose)
        g.add_node("submit", self._submit)
        g.add_node("revise", self._revise)
        g.add_node("give_up", self._give_up)
        g.add_edge(START, "gather")
        g.add_edge("gather", "investigate")
        g.add_conditional_edges("investigate", lambda s: "run_tools" if s.get("pending") else "diagnose")
        g.add_edge("run_tools", "investigate")
        g.add_edge("diagnose", "propose")
        g.add_conditional_edges("propose", self._after_propose)
        g.add_conditional_edges("submit", self._after_submit)
        g.add_edge("revise", "propose")
        g.add_edge("give_up", END)
        return g.compile()

    async def run(self) -> Outcome:
        """Run the graph and report how it ended. It never raises for a failure of the investigation."""
        try:
            final = await self.graph().ainvoke(
                {"messages": [], "findings": [], "pending": [], "attempt": 0, "errors": []},
                config={"recursion_limit": _RECURSION_LIMIT},
            )
            return final["outcome"]  # type: ignore[no-any-return]
        except BudgetExhaustedError as exhausted:
            detail = (
                f"Budget exhausted ({exhausted.resource}): {exhausted.detail}. The Findings so far are kept."
            )
            await self._progress(detail)
            return Outcome("budget_exhausted", detail)
        except ServerRefusedError as refused:
            log.error("pagerless-server refused the agent: %s", refused)
            return Outcome("rejected", str(refused))
        except ServerError as error:
            log.error("pagerless-server is unreachable: %s", error)
            return Outcome("failed", str(error))
        except Exception as error:
            log.exception("the investigation failed")
            detail = f"The agent failed: {type(error).__name__}: {error}"
            await self._progress(detail)
            return Outcome("failed", detail)

    # Nodes.

    async def _gather(self, state: _State) -> _State:
        bundle = await self._within_budget(lambda: self._source.gather(self._target))
        await self._event("Progress", "Gathered the Context Bundle.")
        text = present(self._nonce, "context bundle", bundle.render(), limit=_MAX_BUNDLE_CHARS)
        task = Message(role="user", content=prompts.task_prompt(self._target, text))
        return {"bundle": bundle, "messages": [task]}

    async def _investigate(self, state: _State) -> _State:
        if len(state["messages"]) == 1:
            await self._event("Progress", "Investigating.")
        response = await self._ask(Stage.INVESTIGATE, state)
        assistant = Message(role="assistant", content=response.text, tool_calls=response.tool_calls)
        return {"messages": [*state["messages"], assistant], "pending": list(response.tool_calls)}

    async def _run_tools(self, state: _State) -> _State:
        messages = list(state["messages"])
        findings = list(state["findings"])
        for call in state["pending"]:
            if call.name == prompts.RECORD_FINDING:
                text, is_error = await self._record(call, findings)
            else:
                text, is_error = await self._call_tool(call)
            content = present(self._nonce, call.name, text, is_error=is_error)
            messages.append(Message(role="tool", content=content, tool_call_id=call.id))
        return {"messages": messages, "findings": findings, "pending": []}

    async def _diagnose(self, state: _State) -> _State:
        await self._event("Progress", "Writing the Diagnosis.")
        messages = [*state["messages"], Message(role="user", content=prompts.DIAGNOSE_PROMPT)]
        response = await self._ask(Stage.DIAGNOSE, state, messages, DiagnosisOutput.model_json_schema())
        try:
            output = DiagnosisOutput.model_validate(response.structured)
        except ValidationError as error:
            raise _InvalidOutputError("Diagnosis", error) from error
        findings = list(state["findings"])
        for finding in output.findings:
            await self._add_finding(finding, findings)
        if not findings:
            raise _InvalidOutputError("Diagnosis", "it cites no Finding")
        messages.append(Message(role="assistant", content=json.dumps(response.structured)))
        return {"messages": messages, "findings": findings, "diagnosis": output.diagnosis}

    async def _propose(self, state: _State) -> _State:
        attempt = state["attempt"] + 1
        messages = list(state["messages"])
        if attempt == 1:
            messages.append(Message(role="user", content=prompts.PROPOSE_PROMPT))
        await self._event("Progress", "Writing the Proposal." if attempt == 1 else "Revising the Proposal.")
        response = await self._ask(Stage.PROPOSE, state, messages, proposal_tool_schema())
        messages.append(Message(role="assistant", content=json.dumps(response.structured)))
        update: _State = {"messages": messages, "attempt": attempt}
        try:
            update["proposal"] = PROPOSED_ACTION.validate_python(normalize_proposal(response.structured))
            update["errors"] = []
        except ValidationError as error:
            update["proposal"] = None
            update["errors"] = validation_errors(error, "proposal")
        return update

    def _after_propose(self, state: _State) -> str:
        return self._next_after(state, accepted="submit")

    async def _submit(self, state: _State) -> _State:
        report = Report(
            diagnosis=state["diagnosis"],
            findings=state["findings"],
            proposal=state["proposal"],
            usage=Usage(
                inputTokens=self._input_tokens,
                outputTokens=self._output_tokens,
                costUSD=self._cost_usd,
            ),
        )
        answer = await self._server.post_report(report)
        if answer.accepted:
            return {"outcome": Outcome("reported", "The report was accepted.", phase=answer.phase)}
        return {"errors": answer.errors, "proposal": None}

    def _after_submit(self, state: _State) -> str:
        return END if "outcome" in state else self._next_after(state, accepted=END)

    @staticmethod
    def _next_after(state: _State, *, accepted: str) -> str:
        if not state["errors"]:
            return accepted
        return "revise" if state["attempt"] < MAX_PROPOSAL_ATTEMPTS else "give_up"

    async def _revise(self, state: _State) -> _State:
        message = Message(role="user", content=prompts.revise_prompt(state["errors"]))
        return {"messages": [*state["messages"], message]}

    async def _give_up(self, state: _State) -> _State:
        reasons = "; ".join(f"{e.field}: {e.message}" for e in state["errors"])
        detail = f"The Proposal was refused {MAX_PROPOSAL_ATTEMPTS} times ({reasons})."
        await self._progress(detail)
        return {"outcome": Outcome("proposal_refused", detail)}

    # Collaborators.

    async def _ask(
        self,
        stage: Stage,
        state: _State,
        messages: list[Message] | None = None,
        schema: dict[str, Any] | None = None,
    ) -> ModelResponse:
        self._budget.check_model_call()
        request = ModelRequest(
            stage=stage,
            system=self._system,
            messages=tuple(messages if messages is not None else state["messages"]),
            tools=self._specs,
            output_schema=schema,
            bundle=state["bundle"],
        )
        response = await self._within_budget(lambda: self._provider.complete(request))
        self._budget.spend_input_tokens(response.usage.input_tokens)
        self._input_tokens += response.usage.input_tokens
        self._output_tokens += response.usage.output_tokens
        self._cost_usd += cost_usd(
            response.usage.input_tokens, response.usage.output_tokens, prices=self._prices
        )
        return response

    async def _call_tool(self, call: ToolCall) -> tuple[str, bool]:
        self._budget.spend_tool_call()
        arguments = json.dumps(call.arguments, sort_keys=True)
        await self._event("ToolCall", f"{call.name} {arguments}", tool=call.name)
        try:
            result = await self._within_budget(lambda: self._tools.call(call.name, call.arguments))
        except UnknownToolError:
            return f"There is no tool named {call.name!r}.", True
        except BudgetExhaustedError:
            raise
        except Exception as error:
            log.warning("tool %s failed", call.name, exc_info=True)
            return f"The tool failed: {type(error).__name__}: {error}", True
        return result.text, result.is_error

    async def _record(self, call: ToolCall, findings: list[Finding]) -> tuple[str, bool]:
        try:
            finding = Finding.model_validate(call.arguments)
        except ValidationError as error:
            problems = "; ".join(f"{'.'.join(map(str, e['loc']))}: {e['msg']}" for e in error.errors())
            return f"The Finding was not recorded: {problems}", True
        if finding in findings:
            return "That Finding is already recorded.", False
        if len(findings) >= MAX_FINDINGS:
            return f"The Finding was not recorded: at most {MAX_FINDINGS} Findings are kept.", True
        await self._add_finding(finding, findings)
        return f"Recorded Finding {len(findings)}.", False

    async def _add_finding(self, finding: Finding, findings: list[Finding]) -> None:
        """Keep a Finding and post it, so that the server preserves it even if no report follows."""
        if finding in findings or len(findings) >= MAX_FINDINGS:
            return
        findings.append(finding)
        summary = _truncate(f"{finding.kind}: {finding.summary}", MAX_STEP_SUMMARY_LENGTH)
        await self._server.post_event(StepEvent(kind="Finding", summary=summary, finding=finding))

    async def _event(self, kind: StepKind, summary: str, *, tool: str | None = None) -> None:
        event = StepEvent(
            kind=kind,
            tool=_truncate(tool, MAX_TOOL_NAME_LENGTH) if tool else None,
            summary=_truncate(summary, MAX_STEP_SUMMARY_LENGTH),
        )
        await self._server.post_event(event)

    async def _progress(self, summary: str) -> None:
        """Post a Progress event when the run is ending, where nothing may be raised."""
        try:
            await self._event("Progress", summary)
        except ServerRefusedError, ServerError:
            log.warning("could not post the final step")

    async def _within_budget[T](self, start: Callable[[], Awaitable[T]]) -> T:
        """Await something for no longer than the time budget has left."""
        self._budget.check_time()
        timeout = asyncio.timeout(self._budget.remaining_seconds)
        try:
            async with timeout:
                return await start()
        except TimeoutError:
            if timeout.expired():
                raise BudgetExhaustedError(
                    "time", f"the {self._limits.seconds:g} second time budget is spent"
                ) from None
            raise


class _InvalidOutputError(Exception):
    """The model's structured output does not match its schema."""

    def __init__(self, what: str, reason: object) -> None:
        super().__init__(f"the {what} is invalid: {reason}")
