"""The four-rung structured-output ladder, schema projection, and tolerant argument repair.

No provider is required to support any particular feature, so the agent negotiates:

1. `json_schema` — strict JSON Schema in `response_format`;
2. `json_object` — JSON mode, with the schema in the prompt;
3. `tool` — one forced function call on `submit_output`;
4. `text` — JSON extracted from a plain-text answer.

A provider that rejects a structured parameter with a `400` naming it makes the run
descend one rung without spending another model-call slot. The rung is cached per
`(base_url, model, stage)` so a run negotiates once. Local Pydantic validation is
authoritative at every rung, because the strict projection deliberately drops the
quantity and commit patterns the contract relies on.

The module also repairs what a weak model emits instead of crashing on it: empty
arguments, double-encoded JSON, Markdown fences, and trailing commas.
"""

from __future__ import annotations

import json
from enum import StrEnum
from typing import Any


class Rung(StrEnum):
    """One rung of the structured-output ladder, strongest first."""

    JSON_SCHEMA = "json_schema"
    JSON_OBJECT = "json_object"
    TOOL = "tool"
    TEXT = "text"

    @property
    def next(self) -> Rung | None:
        """Return the rung below this one, or None at the bottom of the ladder."""
        order = RUNG_ORDER
        index = order.index(self)
        return order[index + 1] if index + 1 < len(order) else None


RUNG_ORDER: tuple[Rung, ...] = (Rung.JSON_SCHEMA, Rung.JSON_OBJECT, Rung.TOOL, Rung.TEXT)

# The strict JSON Schema keywords the projection keeps. Everything else is dropped,
# because OpenAI's strict mode rejects it and every other provider ignores it.
_KEPT_KEYWORDS = frozenset(
    {
        "type",
        "properties",
        "required",
        "additionalProperties",
        "items",
        "enum",
        "const",
        "anyOf",
        "$defs",
        "$ref",
        "description",
        "title",
    }
)

# The parameter names a provider mentions when it rejects one of them. The longest
# match wins, so `max_completion_tokens` is not mistaken for `max_tokens`.
_PARAMETER_NAMES: tuple[str, ...] = (
    "max_completion_tokens",
    "parallel_tool_calls",
    "response_format",
    "reasoning_effort",
    "json_schema",
    "tool_choice",
    "max_tokens",
    "temperature",
    "stream",
    "strict",
    "tools",
)

_STRUCTURED_PARAMETERS = frozenset({"response_format", "json_schema", "strict"})

LadderKey = tuple[str, str, str]


class LadderCache:
    """Remembers the rung that a `(base_url, model, stage)` tuple last needed.

    The cache is per process and never persisted: a provider upgrade is picked up by the
    next Job, and a Page only needs to report the rung that produced it. A rung that
    succeeds becomes the starting point for the rest of the run, so a provider that
    rejects `response_format` pays for one negotiation rather than one per call.
    """

    def __init__(self) -> None:
        self._rungs: dict[LadderKey, Rung] = {}

    def start(self, key: LadderKey, *, configured: Rung) -> Rung:
        """Return the rung to try first: the cached one when the run already negotiated."""
        return self._rungs.get(key, configured)

    def record(self, key: LadderKey, rung: Rung) -> None:
        """Remember the rung that succeeded for this endpoint, model, and stage."""
        self._rungs[key] = rung

    def descend(self, key: LadderKey, rung: Rung) -> None:
        """Remember a rung a provider forced the run down to, before it is retried."""
        self._rungs[key] = rung

    def cached(self, key: LadderKey) -> Rung | None:
        """Return the cached rung, or None when this tuple has not been negotiated."""
        return self._rungs.get(key)


def strictify(node: Any) -> Any:
    """Project a JSON Schema onto the subset that strict structured outputs accept.

    Objects gain `additionalProperties: false` and every property becomes `required`;
    a formerly optional property accepts `null` instead, which is how strict mode
    expresses optionality. Numeric and string constraints are dropped, which is exactly
    why local validation remains authoritative.
    """
    if isinstance(node, list):
        return [strictify(item) for item in node]
    if not isinstance(node, dict):
        return node
    projected: dict[str, Any] = {}
    for keyword, value in node.items():
        if keyword not in _KEPT_KEYWORDS:
            continue
        if keyword == "properties" and isinstance(value, dict):
            projected["properties"] = {
                name: strictify(schema) for name, schema in value.items() if isinstance(schema, dict)
            }
        elif keyword == "$defs" and isinstance(value, dict):
            projected["$defs"] = {
                name: strictify(schema) for name, schema in value.items() if isinstance(schema, dict)
            }
        elif keyword == "items":
            projected["items"] = strictify(value)
        elif keyword == "anyOf" and isinstance(value, list):
            projected["anyOf"] = [strictify(item) for item in value]
        else:
            projected[keyword] = value
    is_object = projected.get("type") == "object" or "properties" in projected
    if not is_object:
        return projected
    properties: dict[str, Any] = projected.get("properties", {})
    required = [name for name in projected.get("required", []) if name in properties]
    for name, schema in properties.items():
        if name not in required:
            required.append(name)
            if not _accepts_null(schema):
                properties[name] = {"anyOf": [schema, {"type": "null"}]}
    projected["type"] = "object"
    projected["properties"] = properties
    projected["required"] = required
    # Strict mode has no representation for a map, so an object schema is always closed.
    projected["additionalProperties"] = False
    return projected


def _accepts_null(schema: dict[str, Any]) -> bool:
    if schema.get("type") == "null":
        return True
    types = schema.get("type")
    if isinstance(types, list) and "null" in types:
        return True
    variants = schema.get("anyOf")
    return isinstance(variants, list) and any(
        isinstance(variant, dict) and variant.get("type") == "null" for variant in variants
    )


def schema_text(schema: dict[str, Any]) -> str:
    """Return a compact, deterministic rendering of a schema for a prompt."""
    return json.dumps(schema, indent=2, sort_keys=True, ensure_ascii=False)


def parse_arguments(raw: object) -> dict[str, Any] | None:
    """Repair a tool call's arguments into a JSON object, or return None.

    A weak model produces empty arguments, double-encoded JSON, a fenced payload, or a
    trailing comma. Each is repaired here. A value that is still not an object returns
    None, which the caller turns into a model-visible tool error rather than a crash.
    """
    if raw is None:
        return {}
    if isinstance(raw, dict):
        return raw
    if not isinstance(raw, str):
        return None
    text = raw.strip()
    if not text:
        return {}
    text = strip_fence(text)
    for candidate in (text, strip_trailing_commas(text)):
        parsed = _decode(candidate)
        if isinstance(parsed, str):
            # Double-encoded: the provider wrapped the JSON object in a JSON string.
            parsed = _decode(parsed)
            if isinstance(parsed, str):
                parsed = _decode(parsed)
        if isinstance(parsed, dict):
            return parsed
    return None


def _decode(text: str) -> Any:
    try:
        return json.loads(text)
    except ValueError, TypeError:
        return None


def strip_fence(text: str) -> str:
    """Remove one Markdown code fence that wraps a whole payload."""
    stripped = text.strip()
    if not stripped.startswith("```"):
        return stripped
    lines = stripped.splitlines()
    if len(lines) < 2 or not lines[-1].strip().startswith("```"):
        return stripped
    return "\n".join(lines[1:-1]).strip()


def strip_trailing_commas(text: str) -> str:
    """Remove trailing commas before a closing brace or bracket, outside strings."""
    out: list[str] = []
    in_string = False
    escaped = False
    index = 0
    while index < len(text):
        character = text[index]
        if in_string:
            out.append(character)
            if escaped:
                escaped = False
            elif character == "\\":
                escaped = True
            elif character == '"':
                in_string = False
            index += 1
            continue
        if character == '"':
            in_string = True
            out.append(character)
            index += 1
            continue
        if character == ",":
            lookahead = index + 1
            while lookahead < len(text) and text[lookahead].isspace():
                lookahead += 1
            if lookahead < len(text) and text[lookahead] in "}]":
                index += 1
                continue
        out.append(character)
        index += 1
    return "".join(out)


def extract_json(text: str) -> Any:
    """Return the first balanced JSON object or array in a text answer, or None."""
    stripped = strip_fence(text).strip()
    if not stripped:
        return None
    parsed = _decode(stripped)
    if isinstance(parsed, (dict, list)):
        return parsed
    start = _first_open(stripped)
    if start is None:
        return None
    depth = 0
    in_string = False
    escaped = False
    for index in range(start, len(stripped)):
        character = stripped[index]
        if in_string:
            if escaped:
                escaped = False
            elif character == "\\":
                escaped = True
            elif character == '"':
                in_string = False
            continue
        if character == '"':
            in_string = True
        elif character in "{[":
            depth += 1
        elif character in "}]":
            depth -= 1
            if depth == 0:
                return _decode(stripped[start : index + 1])
    return None


def _first_open(text: str) -> int | None:
    for index, character in enumerate(text):
        if character in "{[":
            return index
    return None


class PointerError(ValueError):
    """A JSON Pointer does not resolve inside the document it cites."""


def resolve_pointer(document: Any, pointer: str) -> Any:
    """Resolve an RFC 6901 JSON Pointer, refusing anything that leaves the document."""
    if pointer == "":
        return document
    if not pointer.startswith("/"):
        raise PointerError(f"{pointer!r} is not a JSON Pointer")
    current = document
    for raw_token in pointer.split("/")[1:]:
        token = raw_token.replace("~1", "/").replace("~0", "~")
        try:
            if isinstance(current, list):
                current = current[int(token)]
            elif isinstance(current, dict):
                current = current[token]
            else:
                raise PointerError(f"{pointer!r} names a field of a scalar")
        except (KeyError, IndexError, ValueError) as error:
            raise PointerError(f"{pointer!r} does not exist in the cited result") from error
    return current


def named_parameter(message: str) -> str | None:
    """Return the request parameter a provider's error message names first, if any.

    The earliest mention wins, because a message such as "max_tokens is not supported; use
    max_completion_tokens" names the rejected field before the suggested replacement.
    """
    lowered = message.lower()
    matches = [(lowered.index(name), -len(name), name) for name in _PARAMETER_NAMES if name in lowered]
    return min(matches)[2] if matches else None


def is_structured_parameter(parameter: str) -> bool:
    """Return whether a rejected parameter belongs to the structured-output ladder."""
    return parameter in _STRUCTURED_PARAMETERS
