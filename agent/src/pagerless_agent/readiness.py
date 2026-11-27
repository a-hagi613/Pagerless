"""The model half of readiness: a real, harmless tool-call probe with bounded caching.

Kubernetes probes run often, so a probe result is cached. A failure backs off, which keeps
a broken endpoint from being billed on every probe. Reasons are static categories: they
never contain a credential, a URL or provider text.
"""

from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import Callable
from dataclasses import dataclass

from pagerless_agent.budget import Budget, BudgetExhaustedError, Limits
from pagerless_agent.model import Message, ModelProvider, ModelRequest, ToolSpec
from pagerless_agent.openai_compat import (
    ProviderAuthError,
    ProviderError,
    ProviderMalformedError,
    ProviderRejectedError,
    ProviderTimeoutError,
    ProviderTruncatedError,
    ProviderUnavailableError,
)

log = logging.getLogger(__name__)

PROBE_TOOL = "readiness_probe"
FAILURE_BACKOFF_BASE_S = 5.0
FAILURE_BACKOFF_MAX_S = 60.0


@dataclass(frozen=True)
class ProbeResult:
    """One cached readiness answer."""

    ok: bool
    reason: str


class ModelProbe:
    """Checks that the configured endpoint can make a tool call, at most once per TTL."""

    def __init__(
        self,
        provider: ModelProvider,
        *,
        ttl_s: float,
        timeout_s: float,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._provider = provider
        self._ttl_s = ttl_s
        self._timeout_s = timeout_s
        self._clock = clock
        self._lock = asyncio.Lock()
        self._last: ProbeResult | None = None
        self._next_at = 0.0
        self._failures = 0

    async def check(self) -> ProbeResult:
        """Return the cached result, or run one bounded probe."""
        async with self._lock:
            now = self._clock()
            if self._last is not None and now < self._next_at:
                return self._last
            result = await self._probe()
            self._last = result
            if result.ok:
                self._failures = 0
                self._next_at = now + self._ttl_s
            else:
                self._failures += 1
                delay = min(FAILURE_BACKOFF_BASE_S * 2 ** (self._failures - 1), FAILURE_BACKOFF_MAX_S)
                self._next_at = now + delay
            log.info("model readiness probe: %s (%s)", result.ok, result.reason)
            return result

    async def _probe(self) -> ProbeResult:
        tool = ToolSpec(
            name=PROBE_TOOL,
            description="Report that the model endpoint can make a harmless tool call.",
            input_schema={"type": "object", "additionalProperties": False, "properties": {}},
        )
        request = ModelRequest(
            system=(
                "You are a readiness probe. Call the readiness_probe tool with no arguments "
                "and do not answer in prose."
            ),
            messages=(Message(role="user", content="Call readiness_probe now."),),
            tools=(tool,),
            max_output_tokens=32,
            forced_tool=PROBE_TOOL,
            purpose="readiness",
        )
        budget = Budget(
            limits=Limits(tool_calls=1, model_calls=3, input_tokens=100_000, seconds=self._timeout_s)
        )
        try:
            async with asyncio.timeout(self._timeout_s):
                response = await self._provider.complete(request, budget=budget)
        except TimeoutError:
            return ProbeResult(False, "the model tool-call probe timed out")
        except ProviderAuthError:
            return ProbeResult(False, "the model endpoint rejected the credentials")
        except ProviderRejectedError as error:
            if error.reason == "tools_unsupported":
                return ProbeResult(False, "the model endpoint does not support tool calls")
            if error.reason == "rate_limit":
                return ProbeResult(False, "the model endpoint is rate limiting the probe")
            return ProbeResult(False, "the model endpoint rejected the tool-call probe")
        except ProviderTimeoutError:
            return ProbeResult(False, "the model tool-call probe timed out")
        except ProviderUnavailableError:
            return ProbeResult(False, "the model endpoint is unreachable")
        except ProviderMalformedError:
            return ProbeResult(False, "the model endpoint returned a malformed response")
        except ProviderTruncatedError:
            return ProbeResult(False, "the model endpoint truncated the probe")
        except BudgetExhaustedError:
            return ProbeResult(False, "the probe exceeded its call budget")
        except ProviderError:
            return ProbeResult(False, "the model probe failed")
        except Exception as error:  # pragma: no cover - defensive boundary around a probe
            log.warning("model readiness probe failed (%s)", type(error).__name__)
            return ProbeResult(False, "the model probe failed")
        if any(call.name == PROBE_TOOL for call in response.tool_calls):
            return ProbeResult(True, "model tool-call probe succeeded")
        return ProbeResult(False, "the model did not call the readiness tool")
