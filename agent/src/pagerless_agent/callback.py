"""The only outbound evidence channel: authenticated tool callbacks to the Go service.

The agent holds no Kubernetes or GitHub credential and creates no client for either. It
calls one URL, built from the callback origin the installation configured, and it refuses
redirects and proxies. A transport retry repeats the same callId and payload, so the Go
service can return its recorded result instead of reading twice.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Protocol
from urllib.parse import quote

import httpx
from pydantic import ValidationError

from pagerless_agent.contract import CallbackResponse
from pagerless_agent.tools import sanitize_text

MAX_CALLBACK_BYTES = 262_144
_ATTEMPTS = 2
_BACKOFF_SECONDS = 0.25
MAX_ERROR_CHARS = 500


class CallbackError(Exception):
    """The Go tool callback could not produce a usable answer."""

    code = "tool_unavailable"

    def __init__(self, message: str) -> None:
        super().__init__(sanitize_text(message, MAX_ERROR_CHARS))


class ToolRejectedError(CallbackError):
    """The callback refused this agent's credential, so the turn cannot continue."""

    code = "tool_rejected"


class ToolUnavailableError(CallbackError):
    """The callback could not be reached, timed out, or answered with a server error."""

    code = "tool_unavailable"


class ToolProtocolError(CallbackError):
    """The callback answered in a way the contract does not allow."""

    code = "tool_malformed"


@dataclass(frozen=True)
class ToolOutcome:
    """One tool result as the Go policy recorded it."""

    status: str
    result: Any
    redacted: bool = False
    truncated: bool = False

    @property
    def is_error(self) -> bool:
        """Return whether the result is an error the model may correct from."""
        return self.status in {"error", "denied", "timed_out"}

    @property
    def is_evidence(self) -> bool:
        """Return whether the result can be cited as evidence."""
        return self.status in {"ok", "truncated"}


class ToolExecutor(Protocol):
    """The one outbound call the graph makes."""

    async def call(
        self,
        *,
        turn_id: str,
        call_id: str,
        tool: str,
        arguments: dict[str, Any],
        timeout_s: float | None = None,
    ) -> ToolOutcome:
        """Call one tool through the Go policy."""
        ...


class ToolGateway:
    """Calls Go's ``POST /internal/turns/{turnId}/tools`` for one turn at a time."""

    def __init__(
        self,
        base_url: str,
        token: str,
        *,
        timeout_s: float,
        http: httpx.AsyncClient | None = None,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self._base = base_url.rstrip("/")
        self._token = token
        self._timeout_s = timeout_s
        self._sleep = sleep
        self._owns_client = http is None
        self._http = http or httpx.AsyncClient(
            follow_redirects=False,
            trust_env=False,
            timeout=httpx.Timeout(timeout_s, connect=min(10.0, timeout_s)),
        )

    async def call(
        self,
        *,
        turn_id: str,
        call_id: str,
        tool: str,
        arguments: dict[str, Any],
        timeout_s: float | None = None,
    ) -> ToolOutcome:
        """Call one tool. Transport retries reuse the same callId and payload."""
        timeout = self._timeout_s if timeout_s is None else max(0.1, min(timeout_s, self._timeout_s))
        url = f"{self._base}/internal/turns/{quote(turn_id, safe='')}/tools"
        body = {"callId": call_id, "tool": tool, "arguments": arguments}
        headers = {
            "Authorization": f"Bearer {self._token}",
            "Accept": "application/json",
        }
        last = "the callback did not answer"
        for attempt in range(1, _ATTEMPTS + 1):
            try:
                status_code, payload = await self._post(url, headers, body, timeout)
            except (httpx.TimeoutException, httpx.TransportError) as error:
                last = f"the tool callback was unreachable ({type(error).__name__})"
                if attempt < _ATTEMPTS:
                    await self._sleep(_BACKOFF_SECONDS)
                    continue
                raise ToolUnavailableError(last) from error
            if status_code in {401, 403}:
                raise ToolRejectedError(f"the tool callback refused the internal credential ({status_code})")
            if 300 <= status_code < 400:
                raise ToolProtocolError(
                    f"the tool callback redirected ({status_code}); redirects are refused"
                )
            if status_code >= 500:
                last = f"the tool callback answered {status_code}"
                if attempt < _ATTEMPTS:
                    await self._sleep(_BACKOFF_SECONDS)
                    continue
                raise ToolUnavailableError(last)
            if status_code != 200:
                raise ToolProtocolError(f"the tool callback answered {status_code}")
            return _outcome(payload)
        raise ToolUnavailableError(last)  # pragma: no cover - the loop always returns or raises

    async def _post(
        self,
        url: str,
        headers: dict[str, str],
        body: dict[str, Any],
        timeout: float,
    ) -> tuple[int, bytes]:
        async with self._http.stream(
            "POST",
            url,
            json=body,
            headers=headers,
            timeout=httpx.Timeout(timeout, connect=min(10.0, timeout)),
        ) as response:
            payload = await _read_bounded(response)
            status = response.status_code
        return status, payload

    async def aclose(self) -> None:
        """Close the client when this gateway created it."""
        if self._owns_client:
            await self._http.aclose()


async def _read_bounded(response: httpx.Response) -> bytes:
    declared = response.headers.get("content-length")
    if declared is not None:
        try:
            if int(declared) > MAX_CALLBACK_BYTES:
                raise ToolProtocolError(
                    f"the tool result declared {declared} bytes, above the {MAX_CALLBACK_BYTES} byte limit"
                )
        except ValueError:
            raise ToolProtocolError("the tool result declared an invalid Content-Length") from None
    chunks: list[bytes] = []
    total = 0
    async for chunk in response.aiter_bytes():
        total += len(chunk)
        if total > MAX_CALLBACK_BYTES:
            raise ToolProtocolError(
                f"the tool result exceeded the {MAX_CALLBACK_BYTES} byte limit and was discarded"
            )
        chunks.append(chunk)
    return b"".join(chunks)


def _outcome(payload: bytes) -> ToolOutcome:
    try:
        decoded = json.loads(payload)
    except ValueError as error:
        raise ToolProtocolError("the tool callback answered with a body that is not JSON") from error
    if not isinstance(decoded, dict):
        raise ToolProtocolError("the tool callback answered with a non-object body")
    try:
        response = CallbackResponse.model_validate(decoded)
    except ValidationError as error:
        raise ToolProtocolError(
            f"the tool callback response is malformed: {error.error_count()} errors"
        ) from error
    if response.status == "budget_exhausted":
        return ToolOutcome(status=response.status, result=response.result)
    if response.status in {"ok", "truncated"} and response.result is None:
        raise ToolProtocolError("the tool callback returned no result for a successful read")
    return ToolOutcome(
        status=response.status,
        result=response.result,
        redacted=response.redacted,
        truncated=response.truncated or response.status == "truncated",
    )
