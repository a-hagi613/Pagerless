"""The only provider: any endpoint that speaks OpenAI Chat Completions.

The client is configured with a base URL, an API key, a model, and optional default
headers, and with SDK retries disabled (`max_retries=0`) so every model call is one the
budget accounts for. Requests are normalised for the whole ecosystem: exactly one
`system` message first, provider-private reasoning fields never echoed, empty tool results
sent as `{"ok": true, "data": null}`, and tool arguments repaired before the graph sees
them.

Responses are read through the SDK's raw response, so a provider that omits `usage`,
returns a string `finish_reason`, or answers `200` with an `error` body is handled without
a crash. Structured output is negotiated by the four-rung ladder in `structured.py`; a
`400` naming a rejected parameter descends one rung, and the rung that succeeded is cached
per `(base_url, model, stage)`.
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

from pagerless_agent.budget import Budget, estimate_messages_tokens, estimate_tokens
from pagerless_agent.config import ModelSettings
from pagerless_agent.model import ModelRequest, ModelResponse, ToolCall, Usage
from pagerless_agent.structured import (
    LadderCache,
    Rung,
    extract_json,
    is_structured_parameter,
    named_parameter,
    parse_arguments,
    schema_text,
    strictify,
)

log = logging.getLogger(__name__)

# The one tool a structured-output rung may force, so a Proposal can arrive as a function call.
SUBMIT_OUTPUT = "submit_output"
# An empty tool result is rejected by several providers; this is the value that replaces it.
EMPTY_TOOL_RESULT = '{"ok": true, "data": null}'
# The ladder makes at most one request per rung, plus one retry when a parameter is rejected.
_MAX_LADDER_ATTEMPTS = 6
# Transport retries: 1 s then 4 s, bounded by the remaining wall clock.
_MAX_TRANSPORT_RETRIES = 2
_BACKOFF_SECONDS = (1.0, 4.0)
_MAX_RETRY_AFTER_SECONDS = 30.0
# A provider error message is quoted in the Page, so it is bounded and stripped of the key.
_MAX_ERROR_CHARS = 300

_CONFIGURED_RUNG = {
    "auto": Rung.JSON_SCHEMA,
    "json_schema": Rung.JSON_SCHEMA,
    "json_object": Rung.JSON_OBJECT,
    "tool": Rung.TOOL,
    "text": Rung.TEXT,
}

_SCHEMA_INSTRUCTION = (
    "Respond with exactly one JSON object and nothing else: no prose, no Markdown fence. "
    "It must validate against this JSON Schema:\n"
)


class ProviderError(Exception):
    """The model endpoint could not produce an answer the agent can use."""


class ProviderAuthError(ProviderError):
    """The endpoint refused the credentials (401 or 403). The API key is never quoted."""


class ProviderRejectedError(ProviderError):
    """The endpoint rejected the request for a reason the agent cannot fix by retrying."""


class ProviderTruncatedError(ProviderError):
    """The endpoint truncated a structured answer, twice."""


class _RejectedError(Exception):
    """Internal: a 400 that names the parameter it rejected, so the ladder can descend."""

    def __init__(self, message: str, parameter: str | None) -> None:
        super().__init__(message)
        self.parameter = parameter


@dataclass
class _Dialect:
    """What this endpoint has been observed to accept, cached for the process."""

    max_tokens_field: str = "auto"
    temperature: bool | None = None
    parallel_tool_calls: bool | None = None
    forced_tool_choice: bool = True


@dataclass(frozen=True)
class _Raw:
    """The JSON body of one provider response, the status that carried it, and its call count."""

    body: dict[str, Any]
    status: int
    calls: int


@dataclass
class OpenAICompatibleProvider:
    """One OpenAI-compatible endpoint, and the ladder state negotiated with it."""

    settings: ModelSettings
    client: AsyncOpenAI | None = None
    cache: LadderCache = field(default_factory=LadderCache)
    sleep: Callable[[float], Awaitable[None]] = asyncio.sleep
    _dialect: _Dialect = field(default_factory=_Dialect)
    _owns_client: bool = field(default=False, init=False)

    def __post_init__(self) -> None:
        if self.client is None:
            self.client = AsyncOpenAI(
                base_url=self.settings.base_url,
                api_key=self.settings.api_key,
                default_headers=self.settings.headers or None,
                # The graph owns retries, so every call the budget sees is the only call made.
                max_retries=0,
                timeout=Timeout(
                    self.settings.read_timeout_s,
                    connect=self.settings.connect_timeout_s,
                    read=self.settings.read_timeout_s,
                ),
            )
            self._owns_client = True
        if self._dialect.max_tokens_field == "auto":
            self._dialect.max_tokens_field = (
                self.settings.max_tokens_field if self.settings.max_tokens_field != "auto" else "max_tokens"
            )
        if self._dialect.temperature is None:
            self._dialect.temperature = self.settings.supports_temperature

    @property
    def configured_rung(self) -> Rung:
        """Return the rung a run starts at when nothing has been negotiated yet."""
        return _CONFIGURED_RUNG.get(self.settings.structured_output, Rung.JSON_SCHEMA)

    def estimate_prompt_tokens(self, request: ModelRequest) -> int:
        """Return a pessimistic estimate of one request's prompt, before it is sent."""
        total = estimate_tokens(request.system)
        total += estimate_messages_tokens(request.messages)
        if request.tools:
            total += estimate_tokens(
                json.dumps([tool.model_dump(mode="json") for tool in request.tools], sort_keys=True),
                json=True,
            )
        if request.output_schema is not None:
            total += estimate_tokens(schema_text(request.output_schema), json=True)
        return total

    def estimate_input_tokens(self, request: ModelRequest) -> int:
        """Return a pessimistic estimate of one request's input tokens, before it is sent."""
        return self.estimate_prompt_tokens(request) + (
            request.max_output_tokens or self.settings.max_output_tokens
        )

    async def complete(self, request: ModelRequest, *, budget: Budget | None = None) -> ModelResponse:
        """Answer one request, negotiating the ladder and counting every call."""
        if self.client is None:  # pragma: no cover - __post_init__ always builds one
            raise ProviderError("the provider has no client")
        self._spend_model_call(budget)
        rung = self._initial_rung(request)
        key = (self.settings.base_url, self.settings.model, request.stage.value)
        model_calls = 1
        dialect_retries = 0
        max_output_tokens = request.max_output_tokens or self.settings.max_output_tokens
        for _ in range(_MAX_LADDER_ATTEMPTS):
            body = self._body(request, rung, max_output_tokens)
            try:
                raw = await self._post(body, budget)
            except _RejectedError as rejected:
                parameter = rejected.parameter
                if (
                    rung is not None
                    and parameter is not None
                    and is_structured_parameter(parameter)
                    and rung.next is not None
                ):
                    log.info("descending the structured-output ladder to %s: %s", rung.next, rejected)
                    rung = rung.next
                    self.cache.descend(key, rung)
                    continue
                if parameter == "tools" and rung is Rung.TOOL and rung.next is not None:
                    # An endpoint with no function calling at all: fall back to JSON in text.
                    log.info("the endpoint rejected tools; descending to text extraction")
                    rung = rung.next
                    self.cache.descend(key, rung)
                    continue
                if parameter == "tool_choice" and rung is Rung.TOOL and self._dialect.forced_tool_choice:
                    # Some endpoints implement function calling but not a forced choice.
                    log.info("retrying rung 3 without a forced tool_choice")
                    self._dialect.forced_tool_choice = False
                    continue
                if parameter in {"max_tokens", "max_completion_tokens"} and dialect_retries < 1:
                    self._dialect.max_tokens_field = (
                        "max_completion_tokens" if parameter == "max_tokens" else "max_tokens"
                    )
                    dialect_retries += 1
                    continue
                if parameter == "temperature" and dialect_retries < 1:
                    self._dialect.temperature = False
                    dialect_retries += 1
                    continue
                if parameter == "parallel_tool_calls" and dialect_retries < 1:
                    self._dialect.parallel_tool_calls = False
                    dialect_retries += 1
                    continue
                raise ProviderRejectedError(str(rejected)) from rejected
            except ProviderAuthError:
                raise
            except ProviderRejectedError:
                raise
            response = self._normalise(raw, request, rung, model_calls + raw.calls - 1)
            if request.output_schema is not None and response.structured is None:
                if rung is not None and rung.next is not None:
                    log.info("rung %s produced no usable JSON; descending", rung)
                    rung = rung.next
                    continue
                raise ProviderRejectedError(
                    "the endpoint returned no usable structured output at the last rung"
                )
            if response.finish_reason == "length" and request.output_schema is not None:
                if max_output_tokens >= self.settings.max_output_tokens * 2:
                    raise ProviderTruncatedError("the endpoint truncated the structured answer twice")
                log.warning("the structured answer was truncated; retrying once with a larger cap")
                max_output_tokens *= 2
                self._spend_model_call(budget)
                model_calls += 1
                continue
            if rung is not None:
                self.cache.record(key, rung)
            if model_calls > 1:
                response = response.model_copy(update={"model_calls": model_calls})
            return response
        raise ProviderRejectedError("the endpoint rejected every structured-output rung")

    def _initial_rung(self, request: ModelRequest) -> Rung | None:
        if request.output_schema is None:
            return None
        key = (self.settings.base_url, self.settings.model, request.stage.value)
        return self.cache.start(key, configured=self.configured_rung)

    def _body(self, request: ModelRequest, rung: Rung | None, max_output_tokens: int) -> dict[str, Any]:
        messages = self._wire_messages(request, rung)
        body: dict[str, Any] = {
            "model": self.settings.model,
            "messages": messages,
            "stream": False,
        }
        body[self._dialect.max_tokens_field] = max_output_tokens
        if self._dialect.temperature is not False:
            body["temperature"] = 0.0
        tools = self._wire_tools(request, rung)
        if tools:
            body["tools"] = tools
            if self._dialect.parallel_tool_calls is not False:
                body["parallel_tool_calls"] = False
            if rung is Rung.TOOL and self._dialect.forced_tool_choice:
                body["tool_choice"] = {"type": "function", "function": {"name": SUBMIT_OUTPUT}}
            else:
                body["tool_choice"] = "auto"
        if rung is Rung.JSON_SCHEMA and request.output_schema is not None:
            body["response_format"] = {
                "type": "json_schema",
                "json_schema": {
                    "name": f"{request.stage.value}_output",
                    "strict": True,
                    "schema": strictify(request.output_schema),
                },
            }
        elif rung is Rung.JSON_OBJECT:
            body["response_format"] = {"type": "json_object"}
        return body

    def _wire_tools(self, request: ModelRequest, rung: Rung | None) -> list[dict[str, Any]]:
        if rung is Rung.TOOL and request.output_schema is not None:
            return [
                {
                    "type": "function",
                    "function": {
                        "name": SUBMIT_OUTPUT,
                        "description": "Submit the structured answer for this stage.",
                        "parameters": strictify(request.output_schema),
                    },
                }
            ]
        if rung is Rung.TEXT:
            return []
        return [
            {
                "type": "function",
                "function": {
                    "name": tool.name,
                    "description": tool.description,
                    "parameters": tool.input_schema,
                },
            }
            for tool in request.tools
        ]

    def _wire_messages(self, request: ModelRequest, rung: Rung | None) -> list[dict[str, Any]]:
        # Exactly one system message, first. `developer` is OpenAI-only and would break
        # Ollama, vLLM, DeepSeek, and several gateways, so it is never used.
        messages: list[dict[str, Any]] = [{"role": "system", "content": request.system}]
        for message in request.messages:
            if message.role == "system":
                # A stray system message is folded into the single leading one.
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
                        # Several providers reject empty tool content outright.
                        "content": message.content or EMPTY_TOOL_RESULT,
                    }
                )
                continue
            messages.append({"role": message.role, "content": message.content})
        if rung in {Rung.JSON_OBJECT, Rung.TEXT} and request.output_schema is not None:
            messages.append(
                {
                    "role": "user",
                    "content": _SCHEMA_INSTRUCTION + schema_text(request.output_schema),
                }
            )
        elif rung is Rung.TOOL and not self._dialect.forced_tool_choice:
            messages.append(
                {
                    "role": "user",
                    "content": (
                        f"Call the {SUBMIT_OUTPUT} function with the structured answer. "
                        "Call it exactly once and do not answer in prose."
                    ),
                }
            )
        return messages

    async def _post(self, body: dict[str, Any], budget: Budget | None) -> _Raw:
        if self.client is None:  # pragma: no cover - guarded by complete()
            raise ProviderError("the provider has no client")
        retries = 0
        while True:
            try:
                raw = await self.client.chat.completions.with_raw_response.create(**body)
            except BadRequestError as error:
                message = _clean(str(error.message), self.settings.api_key)
                raise _RejectedError(message, named_parameter(message)) from error
            except (AuthenticationError, PermissionDeniedError) as error:
                raise ProviderAuthError(
                    f"the model endpoint refused the credentials at {_host(self.settings.base_url)}"
                ) from error
            except RateLimitError as error:
                if retries >= _MAX_TRANSPORT_RETRIES:
                    reason = _clean(str(error.message), self.settings.api_key)
                    raise ProviderRejectedError(f"the model endpoint is rate limiting: {reason}") from error
                retries += 1
                await self._wait(_retry_after(error, retries), budget)
                self._spend_model_call(budget)
                continue
            except (APITimeoutError, APIConnectionError, InternalServerError) as error:
                if retries >= _MAX_TRANSPORT_RETRIES:
                    raise ProviderRejectedError(
                        f"the model endpoint is unavailable: {type(error).__name__}"
                    ) from error
                retries += 1
                await self._wait(_BACKOFF_SECONDS[min(retries, len(_BACKOFF_SECONDS)) - 1], budget)
                self._spend_model_call(budget)
                continue
            except APIStatusError as error:
                raise ProviderRejectedError(
                    f"the model endpoint answered {error.status_code}: "
                    f"{_clean(str(error.message), self.settings.api_key)}"
                ) from error
            try:
                payload = raw.http_response.json()
            except ValueError as error:
                raise ProviderRejectedError(
                    "the model endpoint answered with a body that is not JSON"
                ) from error
            if not isinstance(payload, dict):
                raise ProviderRejectedError("the model endpoint answered with a non-object body")
            status = raw.http_response.status_code
            return _Raw(body=payload, status=status, calls=retries + 1)

    async def _wait(self, seconds: float, budget: Budget | None) -> None:
        delay = seconds
        if budget is not None:
            budget.check_time()
            delay = min(delay, max(0.0, budget.remaining_seconds))
        if delay > 0:
            await self.sleep(delay)

    def _spend_model_call(self, budget: Budget | None) -> None:
        if budget is not None:
            budget.spend_model_call()

    def _normalise(
        self, raw: _Raw, request: ModelRequest, rung: Rung | None, model_calls: int
    ) -> ModelResponse:
        body = raw.body
        if "choices" not in body and isinstance(body.get("error"), dict):
            error = body["error"]
            message = error.get("message") if isinstance(error, dict) else str(error)
            raise ProviderRejectedError(_clean(str(message), self.settings.api_key))
        choices = body.get("choices")
        if not isinstance(choices, list) or not choices:
            raise ProviderRejectedError("the model endpoint returned no choices")
        if len(choices) > 1:
            log.warning("the model endpoint returned %d choices; using the first", len(choices))
        choice = choices[0] if isinstance(choices[0], dict) else {}
        message = choice.get("message")
        message = message if isinstance(message, dict) else {}
        content = message.get("content")
        text = content if isinstance(content, str) else ""
        tool_calls = self._tool_calls(message)
        finish_reason = choice.get("finish_reason")
        usage = self._usage(body.get("usage"), request, text, tool_calls)
        structured: dict[str, Any] | None = None
        if request.output_schema is not None:
            structured = self._structured(text, tool_calls, rung)
        return ModelResponse(
            text=text,
            tool_calls=tool_calls,
            structured=structured,
            usage=usage,
            rung=rung,
            finish_reason=finish_reason if isinstance(finish_reason, str) else None,
            model_calls=model_calls,
        )

    def _tool_calls(self, message: Mapping[str, Any]) -> tuple[ToolCall, ...]:
        raw_calls = message.get("tool_calls")
        calls: list[ToolCall] = []
        if isinstance(raw_calls, list):
            for index, item in enumerate(raw_calls):
                if not isinstance(item, dict):
                    continue
                function = item.get("function")
                function = function if isinstance(function, dict) else {}
                name = function.get("name")
                if not isinstance(name, str) or not name:
                    continue
                calls.append(_tool_call(item.get("id"), index, name, function.get("arguments")))
        legacy = message.get("function_call")
        if not calls and isinstance(legacy, dict):
            name = legacy.get("name")
            if isinstance(name, str) and name:
                calls.append(_tool_call(None, 0, name, legacy.get("arguments")))
        return tuple(calls)

    def _structured(
        self, text: str, tool_calls: tuple[ToolCall, ...], rung: Rung | None
    ) -> dict[str, Any] | None:
        if rung is Rung.TOOL:
            for call in tool_calls:
                if call.name == SUBMIT_OUTPUT and call.argument_error is None:
                    return call.arguments
            return None
        parsed = extract_json(text)
        return parsed if isinstance(parsed, dict) else None

    def _usage(self, raw: Any, request: ModelRequest, text: str, tool_calls: tuple[ToolCall, ...]) -> Usage:
        prompt = raw.get("prompt_tokens") if isinstance(raw, dict) else None
        completion = raw.get("completion_tokens") if isinstance(raw, dict) else None
        if isinstance(prompt, int) and isinstance(completion, int):
            return Usage(input_tokens=max(0, prompt), output_tokens=max(0, completion))
        # A provider that omits `usage` must still be held to the budget. Estimate
        # pessimistically and mark the figure, so the Page never reports it as fact.
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


def _tool_call(call_id: Any, index: int, name: str, raw_arguments: Any) -> ToolCall:
    identifier = call_id if isinstance(call_id, str) and call_id else f"call_{index}"
    arguments = parse_arguments(raw_arguments)
    if arguments is None:
        return ToolCall(
            id=identifier,
            name=name,
            argument_error=(
                "The arguments were not a valid JSON object. Call the tool again with a JSON object."
            ),
        )
    return ToolCall(id=identifier, name=name, arguments=arguments)


def _retry_after(error: APIStatusError, retries: int) -> float:
    headers = getattr(getattr(error, "response", None), "headers", None)
    if headers is not None:
        raw = headers.get("retry-after")
        try:
            value = float(raw) if raw is not None else None
        except ValueError:
            value = None
        if value is not None:
            return min(max(value, 0.0), _MAX_RETRY_AFTER_SECONDS)
    return _BACKOFF_SECONDS[min(retries, len(_BACKOFF_SECONDS)) - 1]


def _clean(message: str, api_key: str) -> str:
    """Bound a provider message and remove the API key if the provider echoed it."""
    cleaned = message.replace(api_key, "«redacted:api-key»") if api_key else message
    return cleaned[: _MAX_ERROR_CHARS - 1] + "…" if len(cleaned) > _MAX_ERROR_CHARS else cleaned


def _host(base_url: str) -> str:
    return urlsplit(base_url).netloc or base_url
