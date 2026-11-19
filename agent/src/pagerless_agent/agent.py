"""The agent graph.

The graph is fixed:

    investigate <-> run_tools -> diagnose -> propose -> submit
                                     ^          |         |
                                     |          +-> repair
                                     +---------- revise <+

1. `investigate` and `run_tools` form a bounded tool loop over the server's catalogue.
2. `diagnose` writes the Diagnosis, whose Findings must cite recorded tool calls.
3. `propose` writes one Proposal: a `FilePatch` of one to five files, or `DiagnosisOnly`.
4. `submit` posts the report. The server's `422` earns exactly one revision, and a local
   validation failure earns exactly one repair turn at the stage that failed.

A budget that runs out, a server that refuses the agent, or an endpoint that rejects the
request ends the run with the documented outcome. Findings already posted as step events
stay with the Page, and `pagerless-server` closes the Page as Failed.
"""

from __future__ import annotations

import asyncio
import json
import logging
import time
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass, field
from typing import Any, Literal, TypedDict

from langgraph.graph import END, START, StateGraph
from pydantic import BaseModel, ValidationError

from pagerless_agent import prompts
from pagerless_agent.budget import (
    CONTEXT_PRESSURE,
    Budget,
    BudgetExhaustedError,
    Limits,
    shed_messages,
)
from pagerless_agent.config import Settings
from pagerless_agent.contract import (
    MAX_FINDINGS,
    MAX_STEP_SUMMARY_LENGTH,
    MAX_TOOL_NAME_LENGTH,
    PROPOSAL,
    Capabilities,
    Diagnosis,
    DiagnosisOnly,
    Finding,
    Investigation,
    Page,
    Proposal,
    Report,
    ReportError,
    StepEvent,
    StepKind,
    Usage,
    citation_problems,
    diagnosis_schema,
    normalize_proposal,
    proposal_schema,
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
from pagerless_agent.openai_compat import (
    ProviderAuthError,
    ProviderError,
)
from pagerless_agent.toolclient import ServerRefusedError, ToolClient
from pagerless_agent.tools import (
    SuspicionTracker,
    UnknownToolError,
    encode_payload,
    new_nonce,
    present,
)

log = logging.getLogger(__name__)

# The agent submits a Proposal and, if the server refuses it, one revision.
MAX_PROPOSAL_ATTEMPTS = 2
# Bounds the supersteps of one run, so a model that never stops calling tools cannot loop.
_RECURSION_LIMIT = 200
# The Context Bundle is rendered inside this many characters.
_MAX_BUNDLE_CHARS = 20_000

OutcomeKind = Literal[
    "reported",
    "budget_exhausted",
    "proposal_refused",
    "rejected",
    "provider_auth_failed",
    "provider_rejected",
    "failed",
]

_EXIT_CODES: dict[str, int] = {
    "reported": 0,
    "failed": 1,
    "budget_exhausted": 2,
    "proposal_refused": 3,
    "rejected": 4,
    "provider_auth_failed": 5,
    "provider_rejected": 6,
}


@dataclass(frozen=True)
class Outcome:
    """How one investigation ended."""

    kind: OutcomeKind
    detail: str
    phase: str | None = None

    @property
    def exit_code(self) -> int:
        """The process exit code, which tells the server why a Job produced no report."""
        return _EXIT_CODES[self.kind]


class _State(TypedDict, total=False):
    messages: list[Message]
    findings: list[Finding]
    pending: list[ToolCall]
    results: dict[str, Any]
    diagnosis: str
    proposal: Proposal
    attempt: int
    errors: list[ReportError]
    outcome: Outcome


class _StageFailedError(Exception):
    """A stage's structured output could not be validated even after its one repair."""

    def __init__(self, what: str, problems: list[str]) -> None:
        super().__init__(f"the {what} is invalid: {'; '.join(problems)}")
        self.what = what
        self.problems = problems


@dataclass
class _UsageTotals:
    """What the investigation read and wrote, summed over every model call."""

    input_tokens: int = 0
    output_tokens: int = 0
    provider_reported: bool = False
    estimated: bool = False
    rungs: dict[str, str] = field(default_factory=dict)

    def spend(self, response: ModelResponse, stage: Stage) -> None:
        self.input_tokens += response.usage.input_tokens
        self.output_tokens += response.usage.output_tokens
        if response.usage.estimated:
            self.estimated = True
        else:
            self.provider_reported = True
        if response.rung is not None:
            self.rungs[stage.value] = response.rung.value

    @property
    def source(self) -> Literal["provider", "estimated", "mixed"]:
        if self.estimated and self.provider_reported:
            return "mixed"
        return "estimated" if self.estimated else "provider"


class Agent:
    """One investigation: the graph and everything it depends on."""

    def __init__(
        self,
        *,
        settings: Settings,
        provider: ModelProvider,
        client: ToolClient,
        clock: Callable[[], float] = time.monotonic,
        nonce: str | None = None,
    ) -> None:
        self._settings = settings
        self._provider = provider
        self._client = client
        self._clock = clock
        self._nonce = nonce or new_nonce()
        self._budget = Budget(limits=Limits(), clock=clock)
        self._usage = _UsageTotals()
        self._suspicion = SuspicionTracker(nonce=self._nonce)
        self._investigation: Investigation | None = None
        self._system = ""
        self._catalogue: frozenset[str] = frozenset()
        self._record_finding = ToolSpec(
            name=prompts.RECORD_FINDING,
            description=prompts.RECORD_FINDING_DESCRIPTION,
            input_schema=Finding.model_json_schema(by_alias=True),
        )

    # Lifecycle

    async def run(self) -> Outcome:
        """Run the investigation and report how it ended. It never raises for a failed run."""
        try:
            async with asyncio.timeout(self._settings.seconds):
                investigation = await self._client.load_investigation()
        except TimeoutError:
            return Outcome("budget_exhausted", "the time budget is spent before the manifest arrived")
        except ServerRefusedError as error:
            return Outcome("rejected", str(error))
        except Exception as error:
            log.exception("could not read the investigation manifest")
            return Outcome("failed", f"The manifest could not be read: {type(error).__name__}")
        try:
            self._prepare(investigation)
        except ValueError as error:
            return Outcome("failed", str(error))
        try:
            final = await self.graph().ainvoke(
                {
                    "messages": [Message(role="user", content=self._task(investigation))],
                    "findings": [],
                    "pending": [],
                    "results": {},
                    "attempt": 0,
                    "errors": [],
                },
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
        except ProviderAuthError as error:
            await self._progress(f"The model endpoint refused the credentials: {error}")
            return Outcome("provider_auth_failed", str(error))
        except ProviderError as error:
            await self._progress(f"The model endpoint rejected the request: {error}")
            return Outcome("provider_rejected", str(error))
        except _StageFailedError as error:
            await self._progress(str(error))
            return Outcome("failed", str(error))
        except Exception as error:
            log.exception("the investigation failed")
            detail = f"The agent failed: {type(error).__name__}: {error}"
            await self._progress(detail)
            return Outcome("failed", detail)

    def _prepare(self, investigation: Investigation) -> None:
        self._investigation = investigation
        self._budget = Budget(
            limits=Limits(
                tool_calls=investigation.limits.tool_calls,
                model_calls=investigation.limits.model_calls,
                input_tokens=investigation.limits.input_tokens,
                seconds=investigation.limits.seconds,
            ),
            clock=self._clock,
        )
        self._system = prompts.system_prompt(self._budget.limits, investigation)
        catalogue = {spec.name for spec in investigation.tools}
        if prompts.RECORD_FINDING in catalogue:
            raise ValueError(f"the server's catalogue may not name {prompts.RECORD_FINDING}")
        self._catalogue = frozenset(catalogue)

    def _task(self, investigation: Investigation) -> str:
        bundle = present(
            self._nonce,
            "context bundle",
            investigation.context,
            limit=_MAX_BUNDLE_CHARS,
        )
        return prompts.task_prompt(investigation, bundle)

    # The graph

    def graph(self) -> Any:
        """Build and compile the graph."""
        graph = StateGraph(_State)
        graph.add_node("investigate", self._investigate)
        graph.add_node("run_tools", self._run_tools)
        graph.add_node("diagnose", self._diagnose)
        graph.add_node("propose", self._propose)
        graph.add_node("submit", self._submit)
        graph.add_node("revise", self._revise)
        graph.add_node("give_up", self._give_up)
        graph.add_edge(START, "investigate")
        graph.add_conditional_edges(
            "investigate", lambda state: "run_tools" if state.get("pending") else "diagnose"
        )
        graph.add_edge("run_tools", "investigate")
        graph.add_edge("diagnose", "propose")
        graph.add_conditional_edges("propose", self._after_propose)
        graph.add_conditional_edges("submit", self._after_submit)
        graph.add_edge("revise", "propose")
        graph.add_edge("give_up", END)
        return graph.compile()

    # Nodes

    async def _investigate(self, state: _State) -> _State:
        if not state["messages"] or state["messages"][-1].role != "tool":
            await self._event("Progress", "Investigating.")
        response = await self._ask(
            Stage.INVESTIGATE,
            state["messages"],
            tools=(*self._specs(), self._record_finding),
        )
        assistant = Message(role="assistant", content=response.text, tool_calls=response.tool_calls)
        return {"messages": [*state["messages"], assistant], "pending": list(response.tool_calls)}

    async def _run_tools(self, state: _State) -> _State:
        messages = list(state["messages"])
        findings = list(state["findings"])
        results = dict(state["results"])
        for call in state["pending"]:
            payload, is_error = await self._perform(call, findings, results)
            envelope = present(self._nonce, call.name, payload, is_error=is_error)
            messages.append(Message(role="tool", content=envelope, tool_call_id=call.id))
        return {"messages": messages, "findings": findings, "results": results, "pending": []}

    async def _diagnose(self, state: _State) -> _State:
        await self._event("Progress", "Writing the Diagnosis.")
        diagnosis, messages = await self._structured_stage(
            Stage.DIAGNOSE,
            state["messages"],
            prompts.DIAGNOSE_PROMPT,
            Diagnosis,
            "Diagnosis",
            diagnosis_schema(),
            _diagnosis_semantics(state["results"]),
        )
        findings = list(state["findings"])
        for finding in diagnosis.findings:
            await self._add_finding(finding, findings)
        return {
            "messages": messages,
            "findings": findings,
            "diagnosis": diagnosis.diagnosis,
        }

    async def _propose(self, state: _State) -> _State:
        attempt = state["attempt"] + 1
        messages = list(state["messages"])
        if attempt == 1:
            messages.append(Message(role="user", content=prompts.PROPOSE_PROMPT))
        await self._event("Progress", "Writing the Proposal." if attempt == 1 else "Revising the Proposal.")
        try:
            proposal, messages = await self._structured_stage(
                Stage.PROPOSE,
                messages,
                None,
                None,
                "Proposal",
                proposal_schema(),
                _proposal_semantics,
                validator=_validate_proposal,
            )
        except _StageFailedError as error:
            return {"messages": messages, "attempt": attempt, "errors": _stage_errors(error)}
        return {
            "messages": messages,
            "attempt": attempt,
            "proposal": proposal,
            "errors": [],
        }

    def _after_propose(self, state: _State) -> str:
        return "submit" if not state.get("errors") else "give_up"

    async def _submit(self, state: _State) -> _State:
        report = Report(
            page=self._page,
            diagnosis=state["diagnosis"],
            findings=state["findings"],
            proposal=state["proposal"],
            usage=Usage(
                input_tokens=self._usage.input_tokens,
                output_tokens=self._usage.output_tokens,
                model_calls=self._budget.model_calls,
                estimated=self._usage.estimated,
            ),
            capabilities=Capabilities(
                structured_output=dict(self._usage.rungs),
                usage_source=self._usage.source,
            ),
        )
        answer = await self._client.post_report(report)
        if answer.accepted:
            return {"outcome": Outcome("reported", "The report was accepted.", phase=answer.phase)}
        # The server also records the refusal on the Page; the Job log keeps it
        # beside the model calls that produced it for failure diagnostics.
        reasons = "; ".join(f"{error.field}: {error.message}" for error in answer.errors)
        log.warning("the server refused Proposal attempt %d: %s", state["attempt"], reasons)
        return {"errors": answer.errors}

    def _after_submit(self, state: _State) -> str:
        if "outcome" in state or not state.get("errors"):
            return END
        return "revise" if state["attempt"] < MAX_PROPOSAL_ATTEMPTS else "give_up"

    async def _revise(self, state: _State) -> _State:
        message = Message(role="user", content=prompts.revise_prompt(state["errors"]))
        return {"messages": [*state["messages"], message]}

    async def _give_up(self, state: _State) -> _State:
        reasons = "; ".join(f"{error.field}: {error.message}" for error in state["errors"])
        detail = f"The Proposal was refused {MAX_PROPOSAL_ATTEMPTS} times ({reasons})."
        await self._progress(detail)
        return {"outcome": Outcome("proposal_refused", detail)}

    # Tools

    def _specs(self) -> tuple[ToolSpec, ...]:
        return tuple(self._investigation.tools) if self._investigation else ()

    async def _perform(
        self, call: ToolCall, findings: list[Finding], results: dict[str, Any]
    ) -> tuple[dict[str, Any], bool]:
        """Run one model tool call and return the JSON payload and whether it is an error."""
        if call.argument_error is not None:
            self._budget.spend_tool_call()
            await self._tool_event(call, "invalid arguments")
            return _tool_error("invalid_arguments", call.argument_error), True
        if call.name == prompts.RECORD_FINDING:
            payload = await self._record(call, findings)
            return payload, payload.get("ok") is False
        if self._suspicion.observe_tool_attempt(call.name, self._catalogue):
            await self._suspicious_output()
        self._budget.spend_tool_call()
        await self._tool_event(call, json.dumps(call.arguments, sort_keys=True))
        try:
            result = await self._within_budget(lambda: self._client.call(call.name, call.id, call.arguments))
        except UnknownToolError:
            return (
                _tool_error(
                    "unknown_tool",
                    f"There is no tool named {call.name!r}. Use a tool from the catalogue.",
                ),
                True,
            )
        except BudgetExhaustedError:
            raise
        except Exception as error:
            log.warning("tool %s failed", call.name, exc_info=True)
            return _tool_error("tool_failed", f"The tool failed: {type(error).__name__}"), True
        call_id = result.call_id
        if call_id is not None:
            results[call_id] = result.payload
        if self._suspicion.observe_payload(encode_payload(result.payload)):
            await self._suspicious_output()
        return result.payload, result.is_error

    async def _record(self, call: ToolCall, findings: list[Finding]) -> dict[str, Any]:
        try:
            finding = Finding.model_validate(call.arguments)
        except ValidationError as error:
            problems = "; ".join(
                f"{'.'.join(str(part) for part in item['loc'])}: {item['msg']}" for item in error.errors()
            )
            return _tool_error("invalid_finding", f"The Finding was not recorded: {problems}")
        if finding in findings:
            return {"ok": True, "data": {"recorded": len(findings), "duplicate": True}}
        if len(findings) >= MAX_FINDINGS:
            return _tool_error(
                "too_many_findings", f"The Finding was not recorded: at most {MAX_FINDINGS} are kept."
            )
        await self._add_finding(finding, findings)
        return {"ok": True, "data": {"recorded": len(findings)}}

    async def _add_finding(self, finding: Finding, findings: list[Finding]) -> None:
        """Keep a Finding and post it, so it survives even if no report follows."""
        if finding in findings or len(findings) >= MAX_FINDINGS:
            return
        findings.append(finding)
        summary = _truncate(f"{finding.kind}: {finding.summary}", MAX_STEP_SUMMARY_LENGTH)
        await self._client.post_event(
            StepEvent(kind="Finding", summary=summary, finding=finding, tool_call_id=finding.tool_call_id)
        )

    async def _tool_event(self, call: ToolCall, arguments: str) -> None:
        summary = _truncate(f"{call.name} {arguments}", MAX_STEP_SUMMARY_LENGTH)
        await self._client.post_event(
            StepEvent(
                kind="ToolCall",
                tool=_truncate(call.name, MAX_TOOL_NAME_LENGTH),
                summary=summary,
                tool_call_id=call.id,
            )
        )

    async def _suspicious_output(self) -> None:
        for event in self._suspicion.drain():
            log.warning("suspicious output: %s", event)
            await self._client.post_event(
                StepEvent(kind="SuspiciousOutput", summary=_truncate(event, MAX_STEP_SUMMARY_LENGTH))
            )

    # Model calls

    async def _ask(
        self,
        stage: Stage,
        messages: Sequence[Message],
        *,
        tools: tuple[ToolSpec, ...] = (),
        schema: dict[str, Any] | None = None,
        max_output_tokens: int | None = None,
    ) -> ModelResponse:
        fitted = self._fit(list(messages))
        request = ModelRequest(
            stage=stage,
            system=self._system,
            messages=tuple(fitted),
            tools=tools,
            output_schema=schema,
            max_output_tokens=max_output_tokens,
        )
        self._budget.check_model_call()
        self._budget.check_input_estimate(self._provider.estimate_input_tokens(request))
        response = await self._within_budget(lambda: self._provider.complete(request, budget=self._budget))
        self._budget.spend_input_tokens(response.usage.input_tokens)
        self._usage.spend(response, stage)
        return response

    def _fit(self, messages: list[Message]) -> list[Message]:
        """Shed the oldest tool results until the request fits the model's context window."""
        context_tokens = self._settings.model.context_tokens
        limit = int(context_tokens * CONTEXT_PRESSURE)
        if self._estimate(messages) <= limit:
            return messages
        shed = shed_messages(messages)
        if self._estimate(shed) > limit:
            raise BudgetExhaustedError(
                "input tokens",
                f"the conversation cannot be reduced below {context_tokens} tokens",
            )
        log.info("shed the oldest tool results to fit a %d-token context window", context_tokens)
        return shed

    def _estimate(self, messages: list[Message]) -> int:
        request = ModelRequest(
            stage=Stage.INVESTIGATE,
            system=self._system,
            messages=tuple(messages),
            tools=self._specs(),
        )
        return self._provider.estimate_input_tokens(request)

    async def _structured_stage[T: BaseModel](
        self,
        stage: Stage,
        messages: list[Message],
        prompt: str | None,
        model: type[T] | None,
        what: str,
        schema: dict[str, Any],
        semantic: Callable[[T, dict[str, Any]], list[str]] | None = None,
        *,
        validator: Callable[[object], T] | None = None,
    ) -> tuple[T, list[Message]]:
        """Ask for a structured stage, validate it locally, and allow exactly one repair."""
        conversation = list(messages)
        if prompt is not None:
            conversation.append(Message(role="user", content=prompt))
        response = await self._ask(stage, conversation, schema=schema)
        problems, value = _validate(response.structured, model, validator)
        if not problems and value is not None and semantic is not None:
            problems = semantic(value, {})
        if problems:
            log.info("the %s failed local validation; asking for one repair", what)
            conversation.append(Message(role="assistant", content=json.dumps(response.structured or {})))
            conversation.append(Message(role="user", content=prompts.repair_prompt(what, problems)))
            response = await self._ask(stage, conversation, schema=schema)
            problems, value = _validate(response.structured, model, validator)
            if not problems and value is not None and semantic is not None:
                problems = semantic(value, {})
        conversation.append(Message(role="assistant", content=json.dumps(response.structured or {})))
        if problems or value is None:
            raise _StageFailedError(what, problems or ["no structured output was produced"])
        return value, conversation

    # Bookkeeping

    @property
    def _page(self) -> Page:
        if self._investigation is None:  # pragma: no cover - run() prepares first
            raise RuntimeError("the investigation manifest has not been loaded")
        return self._investigation.page

    async def _event(self, kind: StepKind, summary: str, *, tool: str | None = None) -> None:
        event = StepEvent(
            kind=kind,
            tool=_truncate(tool, MAX_TOOL_NAME_LENGTH) if tool else None,
            summary=_truncate(summary, MAX_STEP_SUMMARY_LENGTH),
        )
        await self._client.post_event(event)

    async def _progress(self, summary: str) -> None:
        """Post a final Progress event where nothing may be raised."""
        try:
            await self._event("Progress", summary)
        except ServerRefusedError, ProviderError:
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
                    "time", f"the {self._budget.limits.seconds:g} second time budget is spent"
                ) from None
            raise


def _validate[T: BaseModel](
    payload: dict[str, Any] | None,
    model: type[T] | None,
    validator: Callable[[object], T] | None,
) -> tuple[list[str], T | None]:
    """Validate a structured payload with the strictest model available."""
    if payload is None:
        return ["no structured output was produced"], None
    try:
        if validator is not None:
            return [], validator(payload)
        if model is None:
            return ["no validator was configured"], None
        return [], model.model_validate(payload)
    except ValidationError as error:
        prefix = "output"
        return [f"{item.field}: {item.message}" for item in validation_errors(error, prefix)], None


def _validate_proposal(payload: object) -> Proposal:
    validated: Proposal = PROPOSAL.validate_python(normalize_proposal(payload))
    return validated


def _diagnosis_semantics(results: dict[str, Any]) -> Callable[[Diagnosis, dict[str, Any]], list[str]]:
    def check(diagnosis: Diagnosis, _: dict[str, Any]) -> list[str]:
        return citation_problems(diagnosis.findings, results)

    return check


def _proposal_semantics(proposal: Proposal, _: dict[str, Any]) -> list[str]:
    """Apply the checks no JSON Schema expresses to a validated Proposal."""
    if isinstance(proposal, DiagnosisOnly):
        if not proposal.recommendation.strip():
            return ["recommendation: must say what a human should do"]
        return []
    problems: list[str] = []
    if not 1 <= len(proposal.files) <= 5:
        problems.append("files: a FilePatch must change one to five files")
    if len({file.path for file in proposal.files}) != len(proposal.files):
        problems.append("files: two edits name the same path")
    return problems


def _stage_errors(error: _StageFailedError) -> list[ReportError]:
    return [ReportError(field=problem.split(": ", 1)[0], message=problem) for problem in error.problems]


def _tool_error(code: str, message: str) -> dict[str, Any]:
    return {"ok": False, "error": {"code": code, "message": message}}


def _truncate(text: str, limit: int) -> str:
    return text if len(text) <= limit else text[: limit - 1] + "…"
