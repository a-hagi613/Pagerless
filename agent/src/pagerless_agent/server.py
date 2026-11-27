"""The private HTTP surface: POST /turn, cancellation, readiness and liveness.

Only the Go service calls this API. Every turn endpoint requires the installer-provided
internal bearer credential. The callback URL must have the configured origin, so a turn
cannot make the agent connect anywhere else. Browser concerns, CORS and the public API
belong to the Go service.
"""

from __future__ import annotations

import json
import logging
import secrets
from collections.abc import AsyncIterator, Awaitable, Callable, MutableMapping
from contextlib import asynccontextmanager
from typing import Any
from urllib.parse import SplitResult, urlsplit

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from starlette.exceptions import HTTPException as StarletteHTTPException

from pagerless_agent import __version__
from pagerless_agent.callback import ToolExecutor, ToolGateway
from pagerless_agent.config import BudgetSettings, ServiceSettings
from pagerless_agent.contract import (
    ReadyCheck,
    ReadyResult,
    StopResult,
    TurnLimits,
    TurnRequest,
    TurnResult,
    encode,
)
from pagerless_agent.model import ModelProvider
from pagerless_agent.openai_compat import OpenAICompatibleProvider
from pagerless_agent.readiness import ModelProbe
from pagerless_agent.turns import TurnCapacityError, TurnConflictError, TurnManager

log = logging.getLogger(__name__)

MAX_ERROR_BODY = 500


class ApiError(Exception):
    """An HTTP error with the contract's error object."""

    def __init__(
        self, status: int, code: str, message: str, *, headers: dict[str, str] | None = None
    ) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message
        self.headers = headers


class _BodyTooLargeError(Exception):
    """A body crossed the ingress ceiling while the route was reading it."""

    def __init__(self, limit: int) -> None:
        super().__init__(f"the request body is larger than {limit} bytes")
        self.limit = limit


def create_app(
    settings: ServiceSettings,
    *,
    provider: ModelProvider | None = None,
    gateway: ToolExecutor | None = None,
    probe: ModelProbe | None = None,
    manager: TurnManager | None = None,
) -> FastAPI:
    """Build the application. Only components created here are closed on shutdown."""
    owned_provider = provider is None
    selected_provider = provider or OpenAICompatibleProvider(settings.model)
    owned_gateway = gateway is None
    selected_gateway = gateway or ToolGateway(
        settings.callback_origin,
        settings.token,
        timeout_s=settings.tool_timeout_s,
    )
    selected_probe = probe or ModelProbe(
        selected_provider,
        ttl_s=settings.readiness_ttl_s,
        timeout_s=settings.readiness_timeout_s,
    )
    selected_manager = manager or TurnManager(settings, selected_provider, selected_gateway)

    @asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        log.info("private investigation agent is ready")
        try:
            yield
        finally:
            await selected_manager.shutdown()
            close = getattr(selected_provider, "aclose", None)
            if owned_provider and close is not None:
                await close()
            if owned_gateway:
                aclose = getattr(selected_gateway, "aclose", None)
                if aclose is not None:
                    await aclose()

    app = FastAPI(
        title="Pagerless private investigation agent",
        version=__version__,
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
        openapi_url="/openapi.json",
    )
    app.add_middleware(
        _IngressGuard,
        token=settings.token,
        max_request_bytes=settings.max_request_bytes,
    )
    _install_error_handlers(app)

    @app.post("/turn", response_model=TurnResult)
    async def turn(body: TurnRequest) -> JSONResponse:
        _require_callback_origin(body.callback_url, settings.callback_origin)
        _require_limits(body.limits, settings.budgets)
        try:
            result = await selected_manager.submit(body)
        except TurnConflictError as conflict:
            raise ApiError(409, conflict.code, str(conflict)) from conflict
        except TurnCapacityError as capacity:
            raise ApiError(429, capacity.code, str(capacity)) from capacity
        return JSONResponse(content=encode(result))

    @app.post("/turns/{turn_id}/stop", response_model=StopResult)
    async def stop(turn_id: str) -> JSONResponse:
        result = await selected_manager.stop(turn_id)
        return JSONResponse(content=encode(result))

    @app.get("/readyz", response_model=ReadyResult)
    async def readyz() -> JSONResponse:
        outcome = await selected_probe.check()
        result = ReadyResult(
            status="ready" if outcome.ok else "degraded",
            checks=[ReadyCheck(name="model", ok=outcome.ok, reason=outcome.reason)],
        )
        return JSONResponse(
            status_code=200 if outcome.ok else 503,
            content=encode(result),
        )

    @app.get("/healthz")
    async def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/livez")
    async def livez() -> dict[str, str]:
        return {"status": "ok"}

    return app


Scope = MutableMapping[str, Any]
Receive = Callable[[], Awaitable[dict[str, Any]]]
Send = Callable[[dict[str, Any]], Awaitable[None]]
Message = dict[str, Any]


class _IngressGuard:
    """Authenticate protected routes and count real body bytes before FastAPI reads them.

    This is raw ASGI and deliberately sits outside ``ExceptionMiddleware``: a refusal must
    not depend on route parsing or body validation, and it must not buffer the body. A
    declared ``Content-Length`` is rejected early; actual chunks are counted and discarded
    once the limit is crossed, including when no length is declared. The downstream response
    is suppressed so the client receives one contract-shaped 413, not a parser error.
    """

    def __init__(self, app: Callable[..., Awaitable[None]], *, token: str, max_request_bytes: int) -> None:
        self._app = app
        self._token = token
        self._limit = max_request_bytes

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self._app(scope, receive, send)
            return
        if _is_protected(str(scope.get("path", ""))) and not _credential_matches(
            _header(scope, b"authorization"), self._token
        ):
            await _send_error(
                send,
                401,
                "unauthorized",
                "a valid internal bearer credential is required",
                extra_headers={"WWW-Authenticate": "Bearer"},
            )
            return
        declared = _header(scope, b"content-length")
        if declared is not None:
            try:
                length = int(declared)
            except ValueError:
                await _send_error(send, 400, "invalid_request", "invalid Content-Length header")
                return
            if length < 0:
                await _send_error(send, 400, "invalid_request", "invalid Content-Length header")
                return
            if length > self._limit:
                await _send_error(
                    send,
                    413,
                    "request_too_large",
                    f"the request body is larger than {self._limit} bytes",
                )
                return

        total = 0
        rejected = False
        response_started = False

        async def limited_receive() -> Message:
            nonlocal total, rejected
            if rejected:
                return {"type": "http.request", "body": b"", "more_body": False}
            message = await receive()
            if message["type"] == "http.request":
                body = message.get("body", b"")
                total += len(body)
                if total > self._limit:
                    # Abort the read instead of signalling a normal end-of-body. A parser
                    # that already holds a valid prefix must not reach its route handler.
                    rejected = True
                    raise _BodyTooLargeError(self._limit)
            return message

        async def guarded_send(message: Message) -> None:
            nonlocal response_started
            if rejected:
                return
            if message["type"] == "http.response.start":
                response_started = True
            await send(message)

        try:
            await self._app(scope, limited_receive, guarded_send)
        except _BodyTooLargeError:
            pass
        except Exception:
            if not rejected:
                raise
        if rejected and not response_started:
            await _send_error(
                send,
                413,
                "request_too_large",
                f"the request body is larger than {self._limit} bytes",
            )


def _is_protected(path: str) -> bool:
    normalized = path.rstrip("/") or "/"
    if normalized == "/turn":
        return True
    if not normalized.startswith("/turns/") or not normalized.endswith("/stop"):
        return False
    turn_id = normalized[len("/turns/") : -len("/stop")]
    return bool(turn_id) and "/" not in turn_id


def _header(scope: Scope, name: bytes) -> str | None:
    for key, value in scope.get("headers", []):
        if key.lower() == name:
            return str(value.decode("latin-1"))
    return None


def _credential_matches(header: str | None, token: str) -> bool:
    scheme, _, credential = (header or "").partition(" ")
    if scheme.lower() != "bearer" or not credential:
        return False
    return secrets.compare_digest(credential.encode("utf-8"), token.encode("utf-8"))


async def _send_error(
    send: Send,
    status: int,
    code: str,
    message: str,
    *,
    extra_headers: dict[str, str] | None = None,
) -> None:
    body = json.dumps(
        {"error": {"code": code, "message": message[:MAX_ERROR_BODY]}},
        separators=(",", ":"),
    ).encode("utf-8")
    headers = [
        (b"content-type", b"application/json"),
        (b"content-length", str(len(body)).encode("ascii")),
    ]
    for name, value in (extra_headers or {}).items():
        headers.append((name.lower().encode("ascii"), value.encode("latin-1")))
    await send({"type": "http.response.start", "status": status, "headers": headers})
    await send({"type": "http.response.body", "body": body})


def _require_callback_origin(callback_url: str, configured: str) -> None:
    parsed = _parsed_callback(callback_url)
    expected = _parsed_callback(configured)
    if _normalized_origin(parsed) != _normalized_origin(expected):
        raise ApiError(400, "invalid_request", "callbackUrl must equal the configured callback origin")
    if parsed.path not in {"", "/"}:
        raise ApiError(400, "invalid_request", "callbackUrl must not carry a path")


def _parsed_callback(value: str) -> SplitResult:
    if any(character in value for character in "\r\n"):
        raise ApiError(400, "invalid_request", "callbackUrl must be one HTTP(S) origin")
    parsed = urlsplit(value)
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.hostname
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
    ):
        raise ApiError(
            400,
            "invalid_request",
            "callbackUrl must be an HTTP(S) origin without credentials, a query or a fragment",
        )
    try:
        _ = parsed.port
    except ValueError as error:
        raise ApiError(400, "invalid_request", "callbackUrl has an invalid port") from error
    return parsed


def _normalized_origin(parsed: SplitResult) -> tuple[str, str, int | None]:
    return (
        parsed.scheme.lower(),
        (parsed.hostname or "").lower(),
        parsed.port if parsed.port is not None else {"http": 80, "https": 443}.get(parsed.scheme.lower()),
    )


def _require_limits(limits: TurnLimits | None, ceilings: BudgetSettings) -> None:
    if limits is None:
        return
    checks = (
        ("toolCalls", limits.tool_calls, ceilings.tool_calls),
        ("modelCalls", limits.model_calls, ceilings.model_calls),
        ("inputTokens", limits.input_tokens, ceilings.input_tokens),
        ("seconds", limits.seconds, ceilings.seconds),
    )
    for name, value, ceiling in checks:
        if value is not None and value > ceiling:
            raise ApiError(400, "invalid_request", f"limits.{name} may only lower the configured ceiling")


def _install_error_handlers(app: FastAPI) -> None:
    @app.exception_handler(ApiError)
    async def api_error(_: Request, error: ApiError) -> JSONResponse:
        return JSONResponse(
            status_code=error.status,
            content={"error": {"code": error.code, "message": error.message[:MAX_ERROR_BODY]}},
            headers=error.headers,
        )

    @app.exception_handler(_BodyTooLargeError)
    async def body_too_large(_: Request, error: _BodyTooLargeError) -> JSONResponse:
        return JSONResponse(
            status_code=413,
            content={
                "error": {
                    "code": "request_too_large",
                    "message": f"the request body is larger than {error.limit} bytes",
                }
            },
        )

    @app.exception_handler(RequestValidationError)
    async def validation_error(_: Request, error: RequestValidationError) -> JSONResponse:
        parts: list[str] = []
        for item in error.errors()[:3]:
            location = ".".join(str(part) for part in item["loc"]) or "body"
            parts.append(f"{location}: {item['msg']}")
        return JSONResponse(
            status_code=400,
            content={"error": {"code": "invalid_request", "message": "; ".join(parts)[:500]}},
        )

    @app.exception_handler(StarletteHTTPException)
    async def http_error(_: Request, error: StarletteHTTPException) -> JSONResponse:
        code = "not_found" if error.status_code == 404 else "method_not_allowed"
        return JSONResponse(
            status_code=error.status_code,
            content={"error": {"code": code, "message": "the requested endpoint does not exist"}},
        )

    @app.exception_handler(Exception)
    async def unexpected(_: Request, error: Exception) -> JSONResponse:
        log.error("unhandled request failure (%s)", type(error).__name__)
        return JSONResponse(
            status_code=500,
            content={"error": {"code": "internal_error", "message": "the agent service failed"}},
        )
