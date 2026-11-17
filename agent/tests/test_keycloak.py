"""The Keycloak exchange that authenticates every MCP call."""

from __future__ import annotations

import asyncio
from pathlib import Path
from urllib.parse import parse_qs

import httpx
import pytest

from pagerless_agent.keycloak import FederatedTokenSource, TokenError

TOKEN_URL = "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless/protocol/openid-connect/token"
ASSERTION = "header.payload.signature"


class FakeClock:
    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now


def token_response(access_token: str = "access-1", expires_in: int = 300) -> httpx.Response:
    return httpx.Response(200, json={"access_token": access_token, "expires_in": expires_in})


async def test_a_token_is_exchanged_once_and_refreshed_before_it_expires(tmp_path: Path) -> None:
    clock = FakeClock()
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return token_response(f"access-{len(requests)}")

    assertion = tmp_path / "token"
    assertion.write_text(ASSERTION + "\n", encoding="utf-8")
    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as http:
        source = FederatedTokenSource(
            TOKEN_URL, "pagerless-agent", assertion, http=http, refresh_skew=30, clock=clock
        )
        assert await source.access_token() == "access-1"
        clock.now = 269
        assert await source.access_token() == "access-1"
        clock.now = 271
        assert await source.access_token() == "access-2"
        await source.aclose()

    assert len(requests) == 2
    form = parse_qs(requests[0].content.decode())
    assert requests[0].url == TOKEN_URL
    assert form == {
        "grant_type": ["client_credentials"],
        "client_assertion_type": ["urn:ietf:params:oauth:client-assertion-type:jwt-bearer"],
        "client_assertion": [ASSERTION],
    }
    assert "pagerless-agent" not in requests[0].content.decode()
    assert "Authorization" not in requests[0].headers


async def test_each_exchange_reads_the_rotated_projected_token(tmp_path: Path) -> None:
    clock = FakeClock()
    assertions: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        assertions.append(parse_qs(request.content.decode())["client_assertion"][0])
        return token_response(str(len(assertions)), expires_in=10)

    assertion = tmp_path / "token"
    assertion.write_text("first", encoding="utf-8")
    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as http:
        source = FederatedTokenSource(TOKEN_URL, "pagerless-agent", assertion, http=http, clock=clock)
        await source.access_token()
        assertion.write_text("second", encoding="utf-8")
        clock.now = 11
        await source.access_token()
    assert assertions == ["first", "second"]


async def test_concurrent_calls_share_one_exchange(tmp_path: Path) -> None:
    calls = 0

    async def handler(_: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        await asyncio.sleep(0.01)
        return token_response()

    assertion = tmp_path / "token"
    assertion.write_text(ASSERTION, encoding="utf-8")
    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as http:
        source = FederatedTokenSource(TOKEN_URL, "pagerless-agent", assertion, http=http)
        tokens = await asyncio.gather(*(source.access_token() for _ in range(8)))
    assert tokens == ["access-1"] * 8
    assert calls == 1


@pytest.mark.parametrize(
    "response",
    [
        httpx.Response(401, text="assertion rejected"),
        httpx.Response(500, json={"error": "server_error"}),
    ],
)
async def test_a_refusal_reports_only_the_status(tmp_path: Path, response: httpx.Response) -> None:
    assertion = tmp_path / "token"
    assertion.write_text(ASSERTION, encoding="utf-8")
    async with httpx.AsyncClient(transport=httpx.MockTransport(lambda _: response)) as http:
        source = FederatedTokenSource(TOKEN_URL, "pagerless-agent", assertion, http=http)
        with pytest.raises(TokenError) as raised:
            await source.access_token()
    assert f"HTTP {response.status_code}" in str(raised.value)
    assert ASSERTION not in str(raised.value)


@pytest.mark.parametrize(
    ("body", "message"),
    [
        (httpx.Response(200, text="not json"), "not JSON"),
        (httpx.Response(200, json=[]), "not an object"),
        (httpx.Response(200, json={"expires_in": 300}), "no access token"),
        (httpx.Response(200, json={"access_token": ""}), "no access token"),
        (httpx.Response(200, json={"access_token": "a"}), "no usable token lifetime"),
        (httpx.Response(200, json={"access_token": "a", "expires_in": 0}), "no usable token lifetime"),
        (httpx.Response(200, json={"access_token": "a", "expires_in": "300"}), "no usable token lifetime"),
    ],
)
async def test_an_unusable_token_response_is_refused(
    tmp_path: Path, body: httpx.Response, message: str
) -> None:
    assertion = tmp_path / "token"
    assertion.write_text(ASSERTION, encoding="utf-8")
    async with httpx.AsyncClient(transport=httpx.MockTransport(lambda _: body)) as http:
        source = FederatedTokenSource(TOKEN_URL, "pagerless-agent", assertion, http=http)
        with pytest.raises(TokenError, match=message):
            await source.access_token()


@pytest.mark.parametrize(
    ("content", "message"),
    [
        (None, "cannot read the projected Keycloak token"),
        ("", "empty or malformed"),
        ("line\nline", "empty or malformed"),
        ("x" * 20000, "empty or malformed"),
    ],
)
async def test_an_unusable_projected_token_is_refused(
    tmp_path: Path, content: str | None, message: str
) -> None:
    assertion = tmp_path / "token"
    if content is not None:
        assertion.write_text(content, encoding="utf-8")

    def handler(_: httpx.Request) -> httpx.Response:
        raise AssertionError("an unusable assertion must not reach Keycloak")

    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as http:
        source = FederatedTokenSource(TOKEN_URL, "pagerless-agent", assertion, http=http)
        with pytest.raises(TokenError, match=message):
            await source.access_token()


async def test_a_transport_failure_is_reported_without_the_assertion(tmp_path: Path) -> None:
    assertion = tmp_path / "token"
    assertion.write_text(ASSERTION, encoding="utf-8")

    def fail(_: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError(f"cannot connect with {ASSERTION}")

    async with httpx.AsyncClient(transport=httpx.MockTransport(fail)) as http:
        source = FederatedTokenSource(TOKEN_URL, "pagerless-agent", assertion, http=http)
        with pytest.raises(TokenError) as raised:
            await source.access_token()
    assert ASSERTION not in str(raised.value)


async def test_the_exchange_sends_a_form_encoded_client_assertion(tmp_path: Path) -> None:
    """The client is identified by the assertion, so no client id or secret is sent."""
    assertion = tmp_path / "token"
    assertion.write_text(ASSERTION, encoding="utf-8")
    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return token_response()

    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as http:
        source = FederatedTokenSource(TOKEN_URL, "pagerless-agent", assertion, http=http)
        assert await source.access_token() == "access-1"

    assert seen[0].headers["content-type"] == "application/x-www-form-urlencoded"
    form = parse_qs(seen[0].content.decode())
    assert "client_id" not in form
    assert "client_secret" not in form


async def test_no_token_is_cached_after_a_refusal(tmp_path: Path) -> None:
    assertion = tmp_path / "token"
    assertion.write_text(ASSERTION, encoding="utf-8")
    answers = [httpx.Response(503), token_response("recovered")]

    def handler(_: httpx.Request) -> httpx.Response:
        return answers.pop(0)

    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as http:
        source = FederatedTokenSource(TOKEN_URL, "pagerless-agent", assertion, http=http)
        with pytest.raises(TokenError):
            await source.access_token()
        assert await source.access_token() == "recovered"
