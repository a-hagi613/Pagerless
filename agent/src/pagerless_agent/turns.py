"""Turn lifecycle: capacity, per-page exclusion, replay and cancellation.

The Go service owns durable turn state. This manager keeps only what it needs to run a
bounded number of turns: one active turn per page, a global ceiling, a fingerprint per
turn, and a bounded cache of terminal results. A replay of the identical request returns
the cached result; a different payload for the same turn is a conflict.
"""

from __future__ import annotations

import asyncio
import logging
import time
from collections import OrderedDict
from collections.abc import Callable
from dataclasses import dataclass

from pagerless_agent.agent import Agent
from pagerless_agent.budget import Limits
from pagerless_agent.callback import ToolExecutor
from pagerless_agent.config import ServiceSettings
from pagerless_agent.contract import (
    StopResult,
    TurnError,
    TurnRequest,
    TurnResult,
    TurnUsage,
)
from pagerless_agent.model import ModelProvider

log = logging.getLogger(__name__)

COMPLETED_TTL_S = 600.0
MAX_COMPLETED = 256


class TurnConflictError(Exception):
    """A turn cannot start. `status` is always 409."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code


class TurnCapacityError(Exception):
    """Every configured turn slot is busy; the caller may retry later."""

    code = "capacity"


@dataclass
class _ActiveTurn:
    """One running turn."""

    request: TurnRequest
    fingerprint: str
    started_at: float
    task: asyncio.Task[TurnResult] | None = None


@dataclass(frozen=True)
class _CompletedTurn:
    """One terminal result retained for idempotent replay."""

    fingerprint: str
    result: TurnResult
    stored_at: float


class TurnManager:
    """Runs turns and enforces the contract's concurrency and replay rules."""

    def __init__(
        self,
        settings: ServiceSettings,
        provider: ModelProvider,
        gateway: ToolExecutor,
        *,
        clock: Callable[[], float] = time.monotonic,
        call_id_factory: Callable[[], str] | None = None,
        completed_ttl_s: float = COMPLETED_TTL_S,
        max_completed: int = MAX_COMPLETED,
    ) -> None:
        self._settings = settings
        self._provider = provider
        self._gateway = gateway
        self._clock = clock
        self._call_id_factory = call_id_factory
        self._completed_ttl_s = completed_ttl_s
        self._max_completed = max_completed
        self._lock = asyncio.Lock()
        self._active_by_page: dict[str, _ActiveTurn] = {}
        self._active_by_turn: dict[str, _ActiveTurn] = {}
        self._completed: OrderedDict[str, _CompletedTurn] = OrderedDict()

    @property
    def active_turns(self) -> int:
        """Return the number of turns currently running."""
        return len(self._active_by_turn)

    async def submit(self, request: TurnRequest) -> TurnResult:
        """Start one turn, or replay a terminal result for the identical request."""
        fingerprint = request.fingerprint()
        entry: _ActiveTurn | None = None
        async with self._lock:
            completed = self._cached(request.turn_id, fingerprint)
            if completed is not None:
                return completed
            active = self._active_by_turn.get(request.turn_id)
            if active is not None:
                if active.fingerprint == fingerprint:
                    raise TurnConflictError("turn_running", "this identical turn is still running")
                raise TurnConflictError(
                    "idempotency_conflict", "this turn ID is already in use with a different payload"
                )
            if request.page_id in self._active_by_page:
                raise TurnConflictError("page_busy", "another turn is already running for this page")
            if len(self._active_by_turn) >= self._settings.max_concurrency:
                raise TurnCapacityError("all investigation slots are busy; retry after a turn finishes")
            entry = _ActiveTurn(request=request, fingerprint=fingerprint, started_at=self._clock())
            self._active_by_page[request.page_id] = entry
            self._active_by_turn[request.turn_id] = entry
            entry.task = asyncio.create_task(self._execute(entry))
        assert entry is not None and entry.task is not None
        return await entry.task

    async def stop(self, turn_id: str) -> StopResult:
        """Stop one active turn. Repeated calls are safe and observationally idempotent."""
        async with self._lock:
            active = self._active_by_turn.get(turn_id)
            if active is not None:
                if active.task is not None and not active.task.done():
                    active.task.cancel()
                return StopResult(status="stopped")
            if turn_id in self._completed:
                return StopResult(status="finished")
            return StopResult(status="unknown")

    async def shutdown(self) -> None:
        """Cancel every running turn and wait for it to finish, bounded by the settings."""
        async with self._lock:
            tasks = [entry.task for entry in self._active_by_turn.values() if entry.task is not None]
        for task in tasks:
            task.cancel()
        if tasks:
            await asyncio.wait(tasks, timeout=self._settings.shutdown_timeout_s)

    async def _execute(self, entry: _ActiveTurn) -> TurnResult:
        started = self._clock()
        agent: Agent | None = None
        try:
            agent = Agent(
                request=entry.request,
                provider=self._provider,
                gateway=self._gateway,
                limits=self._limits(entry.request),
                tool_timeout_s=self._settings.tool_timeout_s,
                context_tokens=self._settings.model.context_tokens,
                max_output_tokens=self._settings.model.max_output_tokens,
                call_id_factory=self._call_id_factory,
            )
            result = await agent.run()
        except asyncio.CancelledError:
            result = (
                agent.stopped_result()
                if agent is not None
                else TurnResult(status="stopped", usage=TurnUsage())
            )
        except Exception as error:  # pragma: no cover - Agent.run already bounds its own failures
            log.exception("turn %s failed outside the graph", entry.request.turn_id)
            result = TurnResult(
                status="failed",
                error=TurnError(code="internal_error", message=f"the agent failed ({type(error).__name__})"),
                usage=TurnUsage(),
            )
        self._finish(entry, result)
        log.info(
            "turn %s on page %s finished %s in %.3fs",
            entry.request.turn_id,
            entry.request.page_id,
            result.status,
            self._clock() - started,
        )
        return result

    def _finish(self, entry: _ActiveTurn, result: TurnResult) -> None:
        """Remove the active registrations and retain the terminal result.

        The method contains no await, so it is atomic with respect to other tasks on the
        event loop.
        """
        self._active_by_turn.pop(entry.request.turn_id, None)
        self._active_by_page.pop(entry.request.page_id, None)
        self._completed[entry.request.turn_id] = _CompletedTurn(
            fingerprint=entry.fingerprint,
            result=result.model_copy(deep=True),
            stored_at=self._clock(),
        )
        self._completed.move_to_end(entry.request.turn_id)
        now = self._clock()
        while self._completed:
            oldest_id, oldest = next(iter(self._completed.items()))
            if (
                now - oldest.stored_at <= self._completed_ttl_s
                and len(self._completed) <= self._max_completed
            ):
                break
            self._completed.pop(oldest_id, None)

    def _cached(self, turn_id: str, fingerprint: str) -> TurnResult | None:
        completed = self._completed.get(turn_id)
        if completed is None:
            return None
        if self._clock() - completed.stored_at > self._completed_ttl_s:
            self._completed.pop(turn_id, None)
            return None
        if completed.fingerprint != fingerprint:
            raise TurnConflictError(
                "idempotency_conflict", "this turn ID is already in use with a different payload"
            )
        self._completed.move_to_end(turn_id)
        return completed.result.model_copy(deep=True)

    def _limits(self, request: TurnRequest) -> Limits:
        configured = self._settings.budgets
        requested = request.limits
        if requested is None:
            return Limits(
                tool_calls=configured.tool_calls,
                model_calls=configured.model_calls,
                input_tokens=configured.input_tokens,
                seconds=configured.seconds,
            )
        return Limits(
            tool_calls=min(requested.tool_calls or configured.tool_calls, configured.tool_calls),
            model_calls=min(requested.model_calls or configured.model_calls, configured.model_calls),
            input_tokens=min(requested.input_tokens or configured.input_tokens, configured.input_tokens),
            seconds=min(requested.seconds or configured.seconds, configured.seconds),
        )
