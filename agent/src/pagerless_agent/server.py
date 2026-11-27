"""The private HTTP surface: POST /turn, cancellation, readiness and liveness.

Only the Go service calls this API. Every turn endpoint requires the installer-provided
internal bearer credential. The callback URL must have the configured origin, so a turn
cannot make the agent connect anywhere else. Browser concerns, CORS and the public API
belong to the Go service.
"""

from __future__ import annotations

import logging
import secrets
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Any
from urllib.parse import SplitResult, urlsplit

from fastapi import Depends, FastAPI, Request
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
    app.state.settings = settings
    _install_error_handlers(app)
    _install_body_limit(app, settings.max_request_bytes)

    @app.post("/turn", response_model=TurnResult, dependencies=[Depends(_authenticated)])
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

    @app.post("/turns/{turn_id}/stop", response_model=StopResult, dependencies=[Depends(_authenticated)])
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


async def _authenticated(request: Request) -> None:
    """Authenticate before FastAPI parses the body, so a refusal leaks no validation detail."""
    settings: ServiceSettings = request.app.state.settings
    _authorize(request, settings)


def _authorize(request: Request, settings: ServiceSettings) -> None:
    header = request.headers.get("authorization", "")
    scheme, _, credential = header.partition(" ")
    if scheme.lower() != "bearer" or not credential:
        raise ApiError(
            401,
            "unauthorized",
            "a valid internal bearer credential is required",
            headers={"WWW-Authenticate": "Bearer"},
        )
    if not secrets.compare_digest(credential.encode("utf-8"), settings.token.encode("utf-8")):
        raise ApiError(
            401,
            "unauthorized",
            "a valid internal bearer credential is required",
            headers={"WWW-Authenticate": "Bearer"},
        )


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


def _install_body_limit(app: FastAPI, limit: int) -> None:
    @app.middleware("http")
    async def limit_body(request: Request, call_next: Any) -> Any:
        declared = request.headers.get("content-length")
        if declared is not None:
            try:
                if int(declared) > limit:
                    return JSONResponse(
                        status_code=413,
                        content={
                            "error": {
                                "code": "request_too_large",
                                "message": f"the request body is larger than {limit} bytes",
                            }
                        },
                    )
            except ValueError:
                return JSONResponse(
                    status_code=400,
                    content={
                        "error": {"code": "invalid_request", "message": "invalid Content-Length header"}
                    },
                )
        return await call_next(request)
