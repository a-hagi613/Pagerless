"""The only model client: any endpoint that speaks OpenAI Chat Completions with tools.

The endpoint is configured entirely by the installer. Provider dialects differ in small
ways, so a ``400`` that names ``max_tokens``, ``temperature``, ``parallel_tool_calls`` or
``tool_choice`` is retried once without that parameter. A tool-calling failure is never
hidden: it becomes a terminal, sanitized provider error.

The SDK's own retries are disabled so every HTTP request is one the budget accounts for.
"""

from __future__ import annotations

import asyncio
import json
import logging
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import urlsplit

from openai import (
    APIConnectionError,
    APIStatusError,
    APITimeoutError,
    AsyncOpenAI,
    AuthenticationError,
    BadRequestError,
    InternalServerError,
    PermissionDeniedError,
    RateLimitError,
    Timeout,
)

from pagerless_agent.arguments import parse_arguments
from pagerless_agent.budget import Budget, estimate_messages_tokens, estimate_tokens
from pagerless_agent.config import ModelSettings
from pagerless_agent.model import ModelRequest, ModelResponse, ToolCall, Usage

log = logging.getLogger(__name__)

EMPTY_TOOL_RESULT = '{"ok": true, "data": null}'
_MAX_DIALECT_RETRIES = 6
_MAX_TRANSPORT_RETRIES = 1
_BACKOFF_SECONDS = 0.5
_MAX_RETRY_AFTER_SECONDS = 10.0
_MAX_ERROR_CHARS = 300
_MALFORMED_MESSAGE = "the model endpoint answered with a body this agent cannot use"

_PARAMETER_NAMES: tuple[str, ...] = (
    "max_completion_tokens",
    "parallel_tool_calls",
    "reasoning_effort",
    "response_format",
    "json_schema",
    "tool_choice",
    "max_tokens",
    "temperature",
    "stream",
    "tools",
)


class ProviderError(Exception):
    """The model endpoint could not produce an answer the agent can use."""

    reason = "provider_error"

    def __init__(self, message: str, *, reason: str | None = None) -> None:
        super().__init__(message)
        if reason is not None:
            self.reason = reason


class ProviderAuthError(ProviderError):
    """The endpoint refused the credentials. The key and endpoint are never quoted."""

    reason = "auth"


class ProviderRejectedError(ProviderError):
    """The endpoint rejected the request for a reason a retry cannot fix."""

    reason = "rejected"


class ProviderUnavailableError(ProviderError):
    """The endpoint could not be reached after the bounded retries."""

    reason = "unavailable"


class ProviderTimeoutError(ProviderError):
    """The endpoint did not answer within the read timeout."""

    reason = "timeout"


class ProviderMalformedError(ProviderError):
    """The endpoint answered 200 with a body the protocol does not allow."""

    reason = "malformed"


class ProviderTruncatedError(ProviderError):
    """The endpoint truncated the answer before it produced anything usable."""

    reason = "truncated"


class _RejectedError(Exception):
    """Internal: a 400 that names the parameter it rejected, so the dialect can adapt."""

    def __init__(self, message: str, parameter: str | None) -> None:
        super().__init__(message)
        self.parameter = parameter


class _RetryableError(Exception):
    """Internal: a transport, timeout or server failure that a bounded retry may fix."""

    def __init__(self, kind: str, message: str, retry_after: float | None = None) -> None:
        super().__init__(message)
        self.kind = kind
        self.retry_after = retry_after


@dataclass(frozen=True)
class _Raw:
    """The JSON body of one provider response."""

    body: dict[str, Any]


@dataclass
class _Dialect:
    """What this endpoint has been observed to accept, cached for the process."""

    max_tokens_field: str = "max_tokens"
    temperature: bool = True
    parallel_tool_calls: bool = True
    tool_choice: str = "forced"
    applied: set[str] = field(default_factory=set)


@dataclass
class OpenAICompatibleProvider:
    """One OpenAI-compatible endpoint and the dialect negotiated with it."""

    settings: ModelSettings
    client: AsyncOpenAI | None = None
    sleep: Callable[[float], Awaitable[None]] = asyncio.sleep
    _dialect: _Dialect = field(default_factory=_Dialect)
    _owns_client: bool = field(default=False, init=False)

    def __post_init__(self) -> None:
        if self.client is None:
            self.client = AsyncOpenAI(
                base_url=self.settings.base_url,
                api_key=self.settings.api_key,
                default_headers=self.settings.headers or None,
                max_retries=0,
                timeout=Timeout(
                    self.settings.read_timeout_s,
                    connect=self.settings.connect_timeout_s,
                    read=self.settings.read_timeout_s,
                ),
            )
            self._owns_client = True

    def estimate_prompt_tokens(self, request: ModelRequest) -> int:
        """Return a pessimistic estimate of one request's prompt, before it is sent."""
        total = estimate_tokens(request.system)
        total += estimate_messages_tokens(request.messages)
        if request.tools:
            total += estimate_tokens(
                json.dumps([tool.model_dump(mode="json") for tool in request.tools], sort_keys=True),
                json=True,
            )
        return total

    def estimate_input_tokens(self, request: ModelRequest) -> int:
        """Return a pessimistic estimate of the input plus the reserved output."""
        return self.estimate_prompt_tokens(request) + (
            request.max_output_tokens or self.settings.max_output_tokens
        )

    async def complete(self, request: ModelRequest, *, budget: Budget | None = None) -> ModelResponse:
        """Answer one request, adapting the dialect and counting every HTTP attempt."""
        if self.client is None:  # pragma: no cover - __post_init__ always builds one
            raise ProviderError("the provider has no client")
        max_output = request.max_output_tokens or self.settings.max_output_tokens
        dialect_retries = 0
        transport_retries = 0
        calls = 0
        body = self._body(request, max_output)
        while True:
            self._spend_model_call(budget)
            calls += 1
            try:
                raw = await self._post(body)
            except _RejectedError as rejected:
                parameter = rejected.parameter
                if parameter == "tools":
                    raise ProviderRejectedError(
                        "the model endpoint does not support tool calls", reason="tools_unsupported"
                    ) from rejected
                if (
                    parameter is not None
                    and dialect_retries < _MAX_DIALECT_RETRIES
                    and self._adapt(parameter)
                ):
                    dialect_retries += 1
                    body = self._body(request, max_output)
                    continue
                raise ProviderRejectedError(str(rejected), reason="rejected") from rejected
            except _RetryableError as retryable:
                if transport_retries < _MAX_TRANSPORT_RETRIES and request.purpose == "turn":
                    transport_retries += 1
                    await self._wait(retryable.retry_after, budget)
                    continue
                raise self._transport_failure(retryable) from retryable
            except ProviderAuthError:
                raise
            response = self._normalise(raw, request, calls)
            if response.finish_reason == "length" and not response.text and not response.tool_calls:
                raise ProviderTruncatedError("the model endpoint truncated the answer before any content")
            return response

    def _body(self, request: ModelRequest, max_output_tokens: int) -> dict[str, Any]:
        messages = self._wire_messages(request)
        body: dict[str, Any] = {
            "model": self.settings.model,
            "messages": messages,
            "stream": False,
        }
        body[self._dialect.max_tokens_field] = max_output_tokens
        if self._dialect.temperature:
            body["temperature"] = 0.0
        tools = self._wire_tools(request)
        if tools:
            body["tools"] = tools
            if self._dialect.parallel_tool_calls:
                body["parallel_tool_calls"] = False
            if request.forced_tool is not None and self._dialect.tool_choice == "forced":
                body["tool_choice"] = {
                    "type": "function",
                    "function": {"name": request.forced_tool},
                }
            elif self._dialect.tool_choice != "omit":
                body["tool_choice"] = "auto"
        return body

    def _wire_tools(self, request: ModelRequest) -> list[dict[str, Any]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": tool.name,
                    "description": tool.description,
                    "parameters": tool.input_schema or {"type": "object", "properties": {}},
                },
            }
            for tool in request.tools
        ]

    def _wire_messages(self, request: ModelRequest) -> list[dict[str, Any]]:
        messages: list[dict[str, Any]] = [{"role": "system", "content": request.system}]
        for message in request.messages:
            if message.role == "system":
                messages[0]["content"] = f"{messages[0]['content']}\n\n{message.content}"
                continue
            if message.role == "assistant":
                wire: dict[str, Any] = {"role": "assistant", "content": message.content}
                if message.tool_calls:
                    wire["tool_calls"] = [
                        {
                            "id": call.id,
                            "type": "function",
                            "function": {
                                "name": call.name,
                                "arguments": json.dumps(call.arguments, sort_keys=True),
                            },
                        }
                        for call in message.tool_calls
                    ]
                messages.append(wire)
                continue
            if message.role == "tool":
                messages.append(
                    {
                        "role": "tool",
                        "tool_call_id": message.tool_call_id or "",
                        "content": message.content or EMPTY_TOOL_RESULT,
                    }
                )
                continue
            messages.append({"role": message.role, "content": message.content})
        return messages

    async def _post(self, body: dict[str, Any]) -> _Raw:
        if self.client is None:  # pragma: no cover - guarded by complete()
            raise ProviderError("the provider has no client")
        try:
            raw = await self.client.chat.completions.with_raw_response.create(**body)
        except BadRequestError as error:
            message = _clean(str(error.message), self.settings.api_key)
            raise _RejectedError(message, _named_parameter(message)) from error
        except (AuthenticationError, PermissionDeniedError) as error:
            raise ProviderAuthError(
                f"the model endpoint refused the credentials at {_host(self.settings.base_url)}"
            ) from error
        except RateLimitError as error:
            raise _RetryableError(
                "rate_limit",
                _clean(str(error.message), self.settings.api_key),
                _retry_after(error),
            ) from error
        except APITimeoutError as error:
            raise _RetryableError("timeout", "the model endpoint timed out") from error
        except (APIConnectionError, InternalServerError) as error:
            raise _RetryableError(
                "unavailable", f"the model endpoint is unavailable ({type(error).__name__})"
            ) from error
        except APIStatusError as error:
            raise ProviderRejectedError(
                f"the model endpoint answered {error.status_code}: "
                f"{_clean(str(error.message), self.settings.api_key)}"
            ) from error
        try:
            payload = raw.http_response.json()
        except ValueError as error:
            raise ProviderMalformedError(_MALFORMED_MESSAGE) from error
        if not isinstance(payload, dict):
            raise ProviderMalformedError(_MALFORMED_MESSAGE)
        return _Raw(body=payload)

    async def _wait(self, seconds: float | None, budget: Budget | None) -> None:
        delay = _BACKOFF_SECONDS if seconds is None else min(max(seconds, 0.0), _MAX_RETRY_AFTER_SECONDS)
        if budget is not None:
            budget.check_time()
            delay = min(delay, max(0.0, budget.remaining_seconds))
        if delay > 0:
            await self.sleep(delay)

    def _spend_model_call(self, budget: Budget | None) -> None:
        if budget is not None:
            budget.spend_model_call()

    def _adapt(self, parameter: str) -> bool:
        """Apply one dialect fallback, returning whether this endpoint has another attempt."""
        dialect = self._dialect
        if parameter in dialect.applied:
            return False
        if parameter in {"max_tokens", "max_completion_tokens"}:
            alternative = "max_completion_tokens" if parameter == "max_tokens" else "max_tokens"
            if dialect.max_tokens_field == alternative:
                return False
            dialect.max_tokens_field = alternative
        elif parameter == "temperature":
            dialect.temperature = False
        elif parameter == "parallel_tool_calls":
            dialect.parallel_tool_calls = False
        elif parameter == "tool_choice":
            if dialect.tool_choice == "forced":
                dialect.tool_choice = "auto"
            elif dialect.tool_choice == "auto":
                dialect.tool_choice = "omit"
            else:
                return False
        else:
            return False
        dialect.applied.add(parameter)
        return True

    def _transport_failure(self, error: _RetryableError) -> ProviderError:
        if error.kind == "timeout":
            return ProviderTimeoutError("the model endpoint timed out")
        if error.kind == "rate_limit":
            return ProviderRejectedError("the model endpoint is rate limiting", reason="rate_limit")
        return ProviderUnavailableError(f"the model endpoint is unavailable ({error.kind})")

    def _normalise(self, raw: _Raw, request: ModelRequest, calls: int) -> ModelResponse:
        body = raw.body
        if "choices" not in body:
            error = body.get("error")
            if isinstance(error, dict) and isinstance(error.get("message"), str):
                raise ProviderRejectedError(_clean(error["message"], self.settings.api_key))
            raise ProviderMalformedError(_MALFORMED_MESSAGE)
        choices = body.get("choices")
        if not isinstance(choices, list) or not choices:
            raise ProviderMalformedError("the model endpoint returned no choices")
        if len(choices) > 1:
            log.warning("the model endpoint returned %d choices; using the first", len(choices))
        choice = choices[0] if isinstance(choices[0], dict) else None
        if choice is None:
            raise ProviderMalformedError(_MALFORMED_MESSAGE)
        message = choice.get("message")
        if not isinstance(message, dict):
            raise ProviderMalformedError("the model endpoint returned no message")
        text = _content_text(message.get("content"))
        tool_calls = _tool_calls(message)
        finish_reason = choice.get("finish_reason")
        usage = self._usage(body.get("usage"), request, text, tool_calls)
        return ModelResponse(
            text=text,
            tool_calls=tool_calls,
            usage=usage,
            finish_reason=finish_reason if isinstance(finish_reason, str) else None,
            model_calls=calls,
        )

    def _usage(self, raw: Any, request: ModelRequest, text: str, tool_calls: tuple[ToolCall, ...]) -> Usage:
        values = raw if isinstance(raw, dict) else {}
        prompt = _first_int(values, ("prompt_tokens", "input_tokens"))
        completion = _first_int(values, ("completion_tokens", "output_tokens"))
        if prompt is not None and completion is not None:
            return Usage(input_tokens=prompt, output_tokens=completion)
        output_text = text + "".join(json.dumps(call.arguments, sort_keys=True) for call in tool_calls)
        return Usage(
            input_tokens=self.estimate_prompt_tokens(request),
            output_tokens=estimate_tokens(output_text, json=True),
            estimated=True,
        )

    async def aclose(self) -> None:
        """Close the SDK client when this provider created it."""
        if self.client is not None and self._owns_client:
            await self.client.close()


def _content_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if content is None:
        return ""
    if isinstance(content, list):
        parts: list[str] = []
        for item in content:
            if isinstance(item, dict) and isinstance(item.get("text"), str):
                parts.append(item["text"])
        return "".join(parts)
    return ""


def _tool_calls(message: Mapping[str, Any]) -> tuple[ToolCall, ...]:
    raw_calls = message.get("tool_calls")
    calls: list[ToolCall] = []
    seen: set[str] = set()
    if isinstance(raw_calls, list):
        for index, item in enumerate(raw_calls):
            if not isinstance(item, dict):
                continue
            function = item.get("function")
            if not isinstance(function, dict):
                continue
            name = function.get("name")
            if not isinstance(name, str) or not name:
                continue
            identifier = item.get("id")
            call_id = identifier if isinstance(identifier, str) and identifier else f"call_{index}"
            if call_id in seen:
                call_id = f"{call_id}_{index}"
            seen.add(call_id)
            arguments = parse_arguments(function.get("arguments"))
            if arguments is None:
                calls.append(
                    ToolCall(
                        id=call_id,
                        name=name,
                        argument_error=(
                            "The arguments were not a valid JSON object. "
                            "Call the tool again with a JSON object."
                        ),
                    )
                )
            else:
                calls.append(ToolCall(id=call_id, name=name, arguments=arguments))
    return tuple(calls)


def _first_int(values: Mapping[str, Any], keys: tuple[str, ...]) -> int | None:
    for key in keys:
        value = values.get(key)
        if isinstance(value, int) and not isinstance(value, bool):
            return max(0, value)
    return None


def _named_parameter(message: str) -> str | None:
    """Return the request parameter a provider's error names first, if any."""
    lowered = message.lower()
    matches = [(lowered.index(name), -len(name), name) for name in _PARAMETER_NAMES if name in lowered]
    return min(matches)[2] if matches else None


def _retry_after(error: APIStatusError) -> float | None:
    headers = getattr(getattr(error, "response", None), "headers", None)
    if headers is None:
        return None
    raw = headers.get("retry-after")
    try:
        value = float(raw) if raw is not None else None
    except ValueError:
        return None
    return min(max(value, 0.0), _MAX_RETRY_AFTER_SECONDS) if value is not None else None


def _clean(message: str, api_key: str) -> str:
    """Bound a provider message and remove the API key if the provider echoed it."""
    cleaned = message.replace(api_key, "«redacted:api-key»") if api_key else message
    cleaned = " ".join(cleaned.split())
    return cleaned[: _MAX_ERROR_CHARS - 1] + "…" if len(cleaned) > _MAX_ERROR_CHARS else cleaned


def _host(base_url: str) -> str:
    return urlsplit(base_url).netloc or base_url
