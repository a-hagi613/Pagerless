"""Exchange the Job's projected service account token for a Keycloak access token.

Keycloak authenticates the agent through client authentication with a federated
JWT (``client-auth-federated``). The assertion is the projected service account
token whose audience is the realm issuer, so the exchange needs no client secret
and no stored credential. Every MCP call presents the resulting access token to
agentgateway, which validates it against the realm JWKS.
"""

from __future__ import annotations

import asyncio
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any, Protocol

import httpx

CLIENT_ASSERTION_TYPE = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
# The projected token is a JWT of a few kilobytes, not an unbounded file.
_MAX_ASSERTION_BYTES = 16384


class TokenError(Exception):
    """The projected token cannot be read, or Keycloak refused or malformed the exchange."""


class TokenProvider(Protocol):
    """A source of bearer tokens for the MCP gateway."""

    async def access_token(self) -> str:
        """Return an access token that is valid now."""
        ...


class FederatedTokenSource:
    """Obtain and cache a Keycloak access token for the agent's federated client."""

    def __init__(
        self,
        token_url: str,
        client_id: str,
        assertion_file: Path,
        *,
        http: httpx.AsyncClient | None = None,
        refresh_skew: float = 30.0,
        timeout: float = 30.0,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._token_url = token_url
        self._client_id = client_id
        self._assertion_file = assertion_file
        self._http = http if http is not None else httpx.AsyncClient(timeout=httpx.Timeout(timeout))
        self._owns_http = http is None
        self._refresh_skew = refresh_skew
        self._clock = clock
        self._lock = asyncio.Lock()
        self._token: str | None = None
        self._expires_at = 0.0

    async def access_token(self) -> str:
        """Return a cached token, or exchange the projected token for a new one."""
        async with self._lock:
            if self._token is not None and self._clock() < self._expires_at - self._refresh_skew:
                return self._token
            token, lifetime = await self._exchange()
            self._token = token
            self._expires_at = self._clock() + lifetime
            return token

    async def aclose(self) -> None:
        """Close the HTTP client, if this source owns it."""
        if self._owns_http:
            await self._http.aclose()

    async def _exchange(self) -> tuple[str, float]:
        assertion = self._assertion()
        try:
            response = await self._http.post(
                self._token_url,
                data={
                    "grant_type": "client_credentials",
                    "client_assertion_type": CLIENT_ASSERTION_TYPE,
                    "client_assertion": assertion,
                },
            )
        except httpx.HTTPError as error:
            # The exception text can carry the request, including the assertion.
            raise TokenError(f"cannot reach the Keycloak token endpoint ({type(error).__name__})") from None
        if response.status_code != httpx.codes.OK:
            raise TokenError(f"Keycloak refused the client assertion with HTTP {response.status_code}")
        return _token_payload(response)

    def _assertion(self) -> str:
        try:
            assertion = self._assertion_file.read_text(encoding="utf-8").strip()
        except OSError as error:
            raise TokenError(f"cannot read the projected Keycloak token ({type(error).__name__})") from None
        if not assertion or len(assertion) > _MAX_ASSERTION_BYTES or "\n" in assertion:
            raise TokenError("the projected Keycloak token is empty or malformed")
        return assertion


def _token_payload(response: httpx.Response) -> tuple[str, float]:
    try:
        payload: Any = response.json()
    except ValueError:
        raise TokenError("Keycloak returned a token response that is not JSON") from None
    if not isinstance(payload, dict):
        raise TokenError("Keycloak returned a token response that is not an object")
    token = payload.get("access_token")
    lifetime = payload.get("expires_in")
    if not isinstance(token, str) or not token:
        raise TokenError("Keycloak returned no access token")
    if isinstance(lifetime, bool) or not isinstance(lifetime, int) or lifetime <= 0:
        raise TokenError("Keycloak returned no usable token lifetime")
    return token, float(lifetime)
