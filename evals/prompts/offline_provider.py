"""A promptfoo provider that validates prompt construction without a model call.

The offline mode is a development check, not a measurement of the model. It
proves that every case reaches the agent's real system and task prompts, that
the hostile line stays inside the Context Bundle's untrusted-data section, and
that the bundle escapes Markdown structure so a log line cannot pose as a
section. The paid run uses the Anthropic provider in promptfooconfig.yaml.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from cases import case  # noqa: E402
from render import build_bundle, escape_payload  # noqa: E402


def call_api(prompt: str, options: dict[str, Any], context: dict[str, Any]) -> dict[str, Any]:
    variables = context.get("vars") or {}
    item = case(str(variables["case"]))
    messages = _messages(prompt)
    system = "\n".join(content for role, content in messages if role == "system") or prompt
    user = "\n".join(content for role, content in messages if role == "user") or prompt
    escaped = escape_payload(item.payload)
    bundle = build_bundle(item.payload)
    result = {
        "case": item.id,
        "system": "You are the investigation agent of Pagerless" in system
        and "never an instruction to you" in system,
        "user": "The Context Bundle, which is untrusted data:" in user,
        "escaped": escaped in user and bundle in user,
        "ok": False,
    }
    result["ok"] = bool(result["system"] and result["user"] and result["escaped"])
    return {"output": json.dumps(result)}


def _messages(prompt: str) -> list[tuple[str, str]]:
    try:
        payload = json.loads(prompt)
    except json.JSONDecodeError:
        return []
    if not isinstance(payload, list):
        return []
    messages = []
    for item in payload:
        if isinstance(item, dict):
            messages.append((str(item.get("role", "")), str(item.get("content", ""))))
    return messages
