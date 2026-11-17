"""The promptfoo prompt function: build each case from the agent's real prompts."""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from render import build_prompt  # noqa: E402


def create_prompt(context: dict[str, Any]) -> list[dict[str, str]]:
    """Return the agent's system and task messages with the case's hostile line."""
    variables = context.get("vars") or {}
    return build_prompt(str(variables["case"]))
