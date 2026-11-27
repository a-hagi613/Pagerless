"""Ingress boundaries: authentication before parsing and a real-byte body cap."""

from __future__ import annotations

import json
from typing import Any, cast

from pagerless_agent.readiness import ModelProbe, ProbeResult
from pagerless_agent.server import create_app
from tests.fakes import TOKEN, FakeGateway, FakeProvider, StubProbe, response
from tests.support import request_body, settings

CONTENT_TYPE = (b"content-type", b"application/json")
AUTH = (b"authorization", f"Bearer {TOKEN}".encode())


class Feeder:
    """A raw ASGI receive that counts how many body chunks the app actually asked for."""

    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = list(chunks)
        self.calls = 0

    async def __call__(self) -> dict[str, Any]:
        self.calls += 1
        if self._chunks:
            body = self._chunks.pop(0)
            return {"type": "http.request", "body": body, "more_body": bool(self._chunks)}
        return {"type": "http.request", "body": b"", "more_body": False}


class ForbiddenReceive:
    """Fails the test if the app reads the body at all."""

    def __init__(self) -> None:
        self.calls = 0

    async def __call__(self) -> dict[str, Any]:
        self.calls += 1
        raise AssertionError("the ingress read a body it should have rejected or authenticated first")


class Sender:
    """Collects raw ASGI response messages."""

    def __init__(self) -> None:
        self.messages: list[dict[str, Any]] = []

    async def __call__(self, message: dict[str, Any]) -> None:
        self.messages.append(message)

    @property
    def status(self) -> int:
        start = next(message for message in self.messages if message["type"] == "http.response.start")
        return cast(int, start["status"])

    @property
    def body(self) -> dict[str, Any]:
        payload = b"".join(
            message.get("body", b"") for message in self.messages if message["type"] == "http.response.body"
        )
        return cast(dict[str, Any], json.loads(payload)) if payload else {}

    def header(self, name: str) -> str | None:
        start = next(message for message in self.messages if message["type"] == "http.response.start")
        for key, value in start["headers"]:
            if key.decode().lower() == name.lower():
                return str(value.decode())
        return None


def scope(
    path: str = "/turn",
    *,
    headers: list[tuple[bytes, bytes]] | None = None,
    content_length: int | None = None,
    method: str = "POST",
) -> dict[str, Any]:
    selected = list(headers or [])
    if content_length is not None:
        selected.append((b"content-length", str(content_length).encode()))
    return {
        "type": "http",
        "asgi": {"version": "3.0", "spec_version": "2.3"},
        "http_version": "1.1",
        "method": method,
        "scheme": "http",
        "path": path,
        "raw_path": path.encode(),
        "query_string": b"",
        "root_path": "",
        "headers": selected,
        "client": ("127.0.0.1", 12345),
        "server": ("testserver", 80),
    }


def app_for(settings: Any, *, provider: FakeProvider | None = None) -> Any:
    return create_app(
        settings,
        provider=provider or FakeProvider([]),
        gateway=FakeGateway(),
        probe=cast(ModelProbe, StubProbe(ProbeResult(True, "model tool-call probe succeeded"))),
    )


async def call(app: Any, request_scope: dict[str, Any], receive: Any) -> Sender:
    sender = Sender()
    await app(request_scope, receive, sender)
    return sender


async def test_authentication_happens_before_any_body_read(tmp_path: Any) -> None:
    selected = settings(tmp_path, PAGERLESS_AGENT_MAX_REQUEST_BYTES="1024")
    receive = ForbiddenReceive()
    sender = await call(app_for(selected), scope(content_length=99_999), receive)
    assert sender.status == 401
    assert sender.body == {
        "error": {
            "code": "unauthorized",
            "message": "a valid internal bearer credential is required",
        }
    }
    assert sender.header("www-authenticate") == "Bearer"
    assert receive.calls == 0


async def test_a_declared_oversized_body_is_refused_without_reading(tmp_path: Any) -> None:
    selected = settings(tmp_path, PAGERLESS_AGENT_MAX_REQUEST_BYTES="1024")
    receive = ForbiddenReceive()
    sender = await call(
        app_for(selected),
        scope(headers=[AUTH], content_length=2_000),
        receive,
    )
    assert sender.status == 413
    assert sender.body["error"]["code"] == "request_too_large"
    assert "1024" in sender.body["error"]["message"]
    assert receive.calls == 0


async def test_actual_bytes_are_counted_without_a_declared_length(tmp_path: Any) -> None:
    selected = settings(tmp_path, PAGERLESS_AGENT_MAX_REQUEST_BYTES="1024")
    receive = Feeder([b"x" * 600, b"y" * 600, b"z" * 600])
    sender = await call(
        app_for(selected),
        scope(headers=[AUTH]),
        receive,
    )
    assert sender.status == 413
    assert sender.body["error"]["code"] == "request_too_large"
    assert receive.calls == 2  # stops at the chunk that crosses the limit
    assert TOKEN not in json.dumps(sender.body)


async def test_actual_bytes_are_counted_beyond_a_small_declared_length(tmp_path: Any) -> None:
    selected = settings(tmp_path, PAGERLESS_AGENT_MAX_REQUEST_BYTES="1024")
    receive = Feeder([b"x" * 600, b"y" * 600])
    sender = await call(
        app_for(selected),
        scope(headers=[AUTH], content_length=10),
        receive,
    )
    assert sender.status == 413
    assert sender.body["error"]["code"] == "request_too_large"
    assert receive.calls == 2


async def test_an_invalid_declared_length_is_refused_before_reading(tmp_path: Any) -> None:
    selected = settings(tmp_path, PAGERLESS_AGENT_MAX_REQUEST_BYTES="1024")
    receive = ForbiddenReceive()
    request_scope = scope(headers=[AUTH])
    request_scope["headers"].append((b"content-length", b"not-a-number"))
    sender = await call(app_for(selected), request_scope, receive)
    assert sender.status == 400
    assert sender.body["error"]["code"] == "invalid_request"
    assert receive.calls == 0


async def test_a_chunked_body_under_the_limit_reaches_the_route(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    payload = json.dumps(request_body()).encode()
    chunks = [payload[: len(payload) // 2], payload[len(payload) // 2 :]]
    receive = Feeder(chunks)
    provider = FakeProvider([response(text="A chunked answer.")])
    sender = await call(
        app_for(selected, provider=provider),
        scope(headers=[AUTH, CONTENT_TYPE]),
        receive,
    )
    assert sender.status == 200
    assert sender.body["status"] == "completed"
    assert sender.body["content"] == "A chunked answer."
    assert receive.calls >= 2


async def test_protected_stop_requires_auth_before_parsing(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    receive = ForbiddenReceive()
    sender = await call(app_for(selected), scope("/turns/t1/stop"), receive)
    assert sender.status == 401
    assert sender.body["error"]["code"] == "unauthorized"


async def test_unprotected_liveness_needs_no_auth(tmp_path: Any) -> None:
    selected = settings(tmp_path)
    sender = await call(app_for(selected), scope("/healthz", method="GET"), Feeder([]))
    assert sender.status == 200
    assert sender.body == {"status": "ok"}


def test_protected_path_matcher_covers_only_turn_routes() -> None:
    from pagerless_agent.server import _is_protected

    assert _is_protected("/turn")
    assert _is_protected("/turn/")
    assert _is_protected("/turns/t1/stop")
    assert _is_protected("/turns/t1/stop/")
    assert not _is_protected("/readyz")
    assert not _is_protected("/healthz")
    assert not _is_protected("/turns/t1")
    assert not _is_protected("/turns/t1/stop/extra")
