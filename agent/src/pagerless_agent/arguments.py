"""Tolerant repair of model tool-call arguments.

A provider may send arguments as a string (the protocol), as an already-decoded object (a
few gateways), empty, double-encoded, fenced or with a trailing comma. Any value that is
still not a JSON object becomes a model-visible tool error, never a crash.
"""

from __future__ import annotations

import json
from typing import Any


def parse_arguments(raw: object) -> dict[str, Any] | None:
    """Return the arguments as a JSON object, or None when they cannot be repaired."""
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
