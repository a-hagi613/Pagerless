"""The agent's only channel to the cluster, the connectors, and GitHub.

The agent talks to exactly two base URLs: this client talks to `PAGERLESS_SERVER_URL`, and
the model client talks to `PAGERLESS_MODEL_BASE_URL`. There is no Kubernetes client, no
connector credential, and no GitHub token here. Every read is a scoped call to the
server's tool broker, authenticated by the Page's projected token, attributed with the
Page UID, and idempotent under a key derived from the Page UID and the model's call id.

One `GET /v1/pages/{page}/investigation` returns the Context Bundle, the tool catalogue,
the scope, and the limits. Tool calls then go to
`POST /v1/pages/{page}/tools/{tool}`, and the final report to
`POST /v1/pages/{page}/report`.
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import quote

import httpx
from pydantic import ValidationError

from pagerless_agent.budget import BudgetExhaustedError
from pagerless_agent.contract import (
    Investigation,
    NarrativeRecord,
    NarrativeReport,
    Report,
    ReportError,
    StepEvent,
    encode,
)
from pagerless_agent.model import ToolSpec
from pagerless_agent.tools import ToolResult, UnknownToolError

log = logging.getLogger(__name__)

# The two headers every request carries: the Page UID for a defence-in-depth check, and
# (on tool calls) an idempotency key so a transport retry never doubles a connector read.
PAGE_UID_HEADER = "X-Pagerless-Page-Uid"
PAGE_UID_HEADER_COMPAT = "X-Page-UID"
IDEMPOTENCY_HEADER = "Idempotency-Key"

_ATTEMPTS = 3
_RETRYABLE = frozenset({502, 503, 504})
_BACKOFF_SECONDS = 0.5
_TIMEOUT_SECONDS = 20.0
# Tool error statuses the model corrects from, returned as a normal tool error.
_TOOL_ERROR_STATUSES = frozenset({400, 404, 413, 502, 504})
_REFUSED_STATUSES = frozenset({401, 403, 409})
_MAX_ERROR_CHARS = 400


class ServerRefusedError(Exception):
    """The server will not accept this agent's requests, so the investigation must stop.

    The cause is the token (401), or a Page that is no longer Investigating (409).
    """

    def __init__(self, status: int, message: str) -> None:
        super().__init__(f"pagerless-server answered {status}: {message}")
        self.status = status


class ServerError(Exception):
    """The server could not be reached, or answered in a way the agent cannot act on."""


@dataclass(frozen=True)
class ReportAnswer:
    """The server's synchronous answer to a report."""

    accepted: bool
    phase: str | None = None
    errors: list[ReportError] = field(default_factory=list)


class ToolClient:
    """The scoped tool broker client, and the source of the investigation manifest."""

    def __init__(
        self,
        base_url: str,
        page: str,
        page_uid: str,
        token_file: Path,
        *,
        http: httpx.AsyncClient | None = None,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self._base = base_url.rstrip("/")
        self._page = quote(page, safe="")
        self._uid = page_uid
        self._token_file = token_file
        self._http = http or httpx.AsyncClient(timeout=_TIMEOUT_SECONDS)
        self._sleep = sleep
        self._investigation: Investigation | None = None

    @property
    def page_uid(self) -> str:
        """Return the Page UID this client is scoped to."""
        return self._uid

    @property
    def investigation(self) -> Investigation:
        """Return the manifest, which must have been loaded first."""
        if self._investigation is None:
            raise ServerError("the investigation manifest has not been loaded")
        return self._investigation

    async def load_investigation(self) -> Investigation:
        """Fetch the manifest: Context Bundle, catalogue, scope, and limits in one call."""
        response = await self._request("GET", "investigation")
        if response.status_code != 200:
            self._raise_if_refused(response)
            raise ServerError(
                f"the server answered {response.status_code} to the investigation: {_message(response)}"
            )
        try:
            investigation = Investigation.model_validate(_json(response))
        except ValidationError as error:
            raise ServerError(f"the investigation manifest is malformed: {error}") from error
        if investigation.page.uid != self._uid:
            raise ServerError("the investigation manifest names a different Page UID")
        self._investigation = investigation
        return investigation

    # ToolRegistry

    def specs(self) -> list[ToolSpec]:
        """Return the server's catalogue, forwarded to the model verbatim."""
        return list(self.investigation.tools)

    async def call(self, name: str, call_id: str, arguments: dict[str, Any]) -> ToolResult:
        """Call one scoped tool. It raises UnknownToolError outside the catalogue."""
        catalogue = {spec.name for spec in self.investigation.tools}
        if name not in catalogue:
            raise UnknownToolError(name)
        response = await self._request(
            "POST",
            f"tools/{quote(name, safe='')}",
            body={"arguments": arguments},
            idempotency=f"{self._uid}/{call_id}",
        )
        if response.status_code == 200:
            payload = _json(response)
            if not isinstance(payload, dict):
                raise ServerError("the tool broker answered with a non-object body")
            is_error = payload.get("ok") is False
            return ToolResult(payload=payload, is_error=is_error)
        if response.status_code == 429:
            code = _code(response)
            if code == "budget_exhausted":
                raise BudgetExhaustedError("tool calls", _message(response))
            raise ServerError(f"the tool broker rate limited the call: {_message(response)}")
        if response.status_code == 403 and _code(response) == "out_of_scope":
            return ToolResult(payload=_error_payload(response), is_error=True)
        if response.status_code in _TOOL_ERROR_STATUSES:
            return ToolResult(payload=_error_payload(response), is_error=True)
        self._raise_if_refused(response)
        raise ServerError(f"the server answered {response.status_code} to tool {name}: {_message(response)}")

    # Page lifecycle

    async def post_event(self, event: StepEvent) -> None:
        """Post one step event. A step is informational, so a delivery failure is logged, not raised."""
        try:
            response = await self._request("POST", "events", body=encode(event))
        except ServerError as error:
            log.warning("could not deliver a step event: %s", error)
            return
        if response.status_code in {200, 202, 204}:
            return
        self._raise_if_refused(response)
        log.warning("the server did not accept a step event: %s", _message(response))

    async def post_report(self, report: Report) -> ReportAnswer:
        """Post the report and return the server's answer: accepted, or its validation errors."""
        response = await self._request("POST", "report", body=encode(report))
        if response.status_code == 200:
            body = _json(response)
            return ReportAnswer(accepted=bool(body.get("accepted")), phase=_phase(body))
        if response.status_code == 422:
            return ReportAnswer(accepted=False, errors=_errors(response))
        self._raise_if_refused(response)
        raise ServerError(f"the server answered {response.status_code} to the report: {_message(response)}")

    async def narrative_record(self) -> NarrativeRecord:
        """Read the Page record that the Narrative describes."""
        response = await self._request("GET", "narrative")
        if response.status_code == 200:
            try:
                return NarrativeRecord.model_validate(_json(response))
            except ValidationError as error:
                raise ServerError(f"the server's Narrative record is malformed: {error}") from error
        self._raise_if_refused(response)
        raise ServerError(
            f"the server answered {response.status_code} to the Narrative record: {_message(response)}"
        )

    async def post_narrative(self, narrative: str) -> None:
        """Post the Narrative, which the server stores in the Page's status."""
        response = await self._request("POST", "narrative", body=encode(NarrativeReport(narrative=narrative)))
        if response.status_code in {200, 202, 204}:
            return
        self._raise_if_refused(response)
        raise ServerError(
            f"the server answered {response.status_code} to the Narrative: {_message(response)}"
        )

    # Transport

    async def _request(
        self,
        method: str,
        endpoint: str,
        *,
        body: dict[str, Any] | None = None,
        idempotency: str | None = None,
    ) -> httpx.Response:
        url = f"{self._base}/v1/pages/{self._page}/{endpoint}"
        last = ""
        for attempt in range(1, _ATTEMPTS + 1):
            # The kubelet refreshes the projected token, so it is read for every request.
            headers = {
                "Authorization": f"Bearer {self._read_token()}",
                PAGE_UID_HEADER: self._uid,
                PAGE_UID_HEADER_COMPAT: self._uid,
            }
            if idempotency is not None:
                headers[IDEMPOTENCY_HEADER] = idempotency
            try:
                response = await self._http.request(method, url, json=body, headers=headers)
            except httpx.TransportError as error:
                last = f"{type(error).__name__}: {error}"
            else:
                if response.status_code not in _RETRYABLE:
                    return response
                last = f"status {response.status_code}"
            if attempt < _ATTEMPTS:
                await self._sleep(_BACKOFF_SECONDS * attempt)
        raise ServerError(f"pagerless-server is unavailable after {_ATTEMPTS} attempts ({last})")

    def _read_token(self) -> str:
        try:
            token = self._token_file.read_text(encoding="utf-8").strip()
        except OSError as error:
            raise ServerError(f"cannot read the projected service account token: {error}") from error
        if not token:
            raise ServerError(f"the projected service account token file {self._token_file} is empty")
        return token

    def _raise_if_refused(self, response: httpx.Response) -> None:
        if response.status_code in _REFUSED_STATUSES:
            raise ServerRefusedError(response.status_code, _message(response))

    async def aclose(self) -> None:
        """Close the HTTP client."""
        await self._http.aclose()


def _json(response: httpx.Response) -> Any:
    try:
        return response.json()
    except ValueError:
        return {}


def _code(response: httpx.Response) -> str | None:
    body = _json(response)
    error = body.get("error") if isinstance(body, dict) else None
    code = error.get("code") if isinstance(error, dict) else None
    return code if isinstance(code, str) else None


def _error_payload(response: httpx.Response) -> dict[str, Any]:
    body = _json(response)
    if isinstance(body, dict) and isinstance(body.get("error"), dict):
        return {"ok": False, "error": body["error"]}
    return {"ok": False, "error": {"code": "tool_error", "message": _message(response)}}


def _message(response: httpx.Response) -> str:
    body = _json(response)
    if isinstance(body, dict):
        error = body.get("error")
        if isinstance(error, dict) and isinstance(error.get("message"), str):
            return str(error["message"])[:_MAX_ERROR_CHARS]
        if isinstance(body.get("message"), str):
            return str(body["message"])[:_MAX_ERROR_CHARS]
    return response.text[:_MAX_ERROR_CHARS]


def _phase(body: Any) -> str | None:
    phase = body.get("phase") if isinstance(body, dict) else None
    return phase if isinstance(phase, str) else None


def _errors(response: httpx.Response) -> list[ReportError]:
    body = _json(response)
    raw = body.get("errors") if isinstance(body, dict) else None
    if not isinstance(raw, list) or not raw:
        raise ServerError(f"the server refused the report without saying why: {_message(response)}")
    try:
        return [ReportError.model_validate(item) for item in raw]
    except ValidationError as error:
        raise ServerError(f"the server's validation errors are malformed: {error}") from error
