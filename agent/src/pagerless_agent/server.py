"""The agent's client for the pagerless-server agent API (`internal/agentapi`)."""

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

from pagerless_agent.contract import NarrativeRecord, NarrativeReport, Report, ReportError, StepEvent, encode

log = logging.getLogger(__name__)

# Attempts for a request that failed in transit or because the server was unavailable.
_ATTEMPTS = 3
_RETRYABLE = frozenset({502, 503, 504})
_BACKOFF_SECONDS = 0.5
_TIMEOUT_SECONDS = 15.0


class ServerRefusedError(Exception):
    """The server will not take this agent's requests, so the investigation must stop.

    The cause is the token (401, 403) or the Triage, which is no longer
    Investigating (409), for example after its Job's deadline passed.
    """

    def __init__(self, status: int, message: str) -> None:
        super().__init__(f"pagerless-server answered {status}: {message}")
        self.status = status


class ServerError(Exception):
    """The server could not be reached, or answered in a way that the agent cannot act on."""


@dataclass(frozen=True)
class ReportAnswer:
    """The server's synchronous answer to a report."""

    accepted: bool
    phase: str | None = None
    errors: list[ReportError] = field(default_factory=list)


class ServerClient:
    """Posts step events and the report, authenticating with the projected service account token."""

    def __init__(
        self,
        base_url: str,
        triage: str,
        token_file: Path,
        *,
        http: httpx.AsyncClient | None = None,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self._base = base_url.rstrip("/")
        self._triage = quote(triage, safe="")
        self._token_file = token_file
        self._http = http or httpx.AsyncClient(timeout=_TIMEOUT_SECONDS)
        self._sleep = sleep

    async def post_event(self, event: StepEvent) -> None:
        """Post one step event. A step is informational, so a failure to deliver it is logged and not raised.

        A server that refuses the agent still raises ServerRefusedError, because the investigation is over.
        """
        try:
            response = await self._post("events", encode(event))
        except ServerError as error:
            log.warning("could not deliver a step event: %s", error)
            return
        if response.status_code == 202:
            return
        self._raise_if_refused(response)
        log.warning("the server did not accept a step event: %s", _message(response))

    async def post_report(self, report: Report) -> ReportAnswer:
        """Post the report and return the server's answer: accepted, or the Validation errors."""
        response = await self._post("report", encode(report))
        if response.status_code == 200:
            body = _json(response)
            return ReportAnswer(accepted=bool(body.get("accepted")), phase=body.get("phase"))
        if response.status_code == 422:
            return ReportAnswer(accepted=False, errors=_errors(response))
        self._raise_if_refused(response)
        raise ServerError(f"the server answered {response.status_code} to the report: {_message(response)}")

    def _raise_if_refused(self, response: httpx.Response) -> None:
        if response.status_code in (401, 403, 409):
            raise ServerRefusedError(response.status_code, _message(response))

    async def narrative_record(self) -> NarrativeRecord:
        """Read the Triage record that the Narrative describes."""
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
        """Post the Narrative, which the server stores in the Triage's status."""
        response = await self._request("POST", "narrative", encode(NarrativeReport(narrative=narrative)))
        if response.status_code == 202:
            return
        self._raise_if_refused(response)
        raise ServerError(
            f"the server answered {response.status_code} to the Narrative: {_message(response)}"
        )

    async def _post(self, endpoint: str, body: dict[str, Any]) -> httpx.Response:
        return await self._request("POST", endpoint, body)

    async def _request(
        self, method: str, endpoint: str, body: dict[str, Any] | None = None
    ) -> httpx.Response:
        url = f"{self._base}/v1/triages/{self._triage}/{endpoint}"
        last: str = ""
        for attempt in range(1, _ATTEMPTS + 1):
            # The kubelet refreshes the projected token before it expires, so read it for every request.
            headers = {"Authorization": f"Bearer {self._read_token()}"}
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
            raise ServerError(f"cannot read the service account token: {error}") from error
        if not token:
            raise ServerError(f"the service account token file {self._token_file} is empty")
        return token

    async def aclose(self) -> None:
        """Close the HTTP client."""
        await self._http.aclose()


def _json(response: httpx.Response) -> dict[str, Any]:
    try:
        body = response.json()
    except ValueError:
        return {}
    return body if isinstance(body, dict) else {}


def _message(response: httpx.Response) -> str:
    return str(_json(response).get("message") or response.text[:200])


def _errors(response: httpx.Response) -> list[ReportError]:
    raw = _json(response).get("errors")
    if not isinstance(raw, list) or not raw:
        raise ServerError(f"the server refused the report without saying why: {_message(response)}")
    try:
        return [ReportError.model_validate(item) for item in raw]
    except ValidationError as error:
        raise ServerError(f"the server's Validation errors are malformed: {error}") from error
