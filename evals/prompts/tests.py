"""Generate the promptfoo test cases for both suite modes."""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from cases import CASES, validate  # noqa: E402


def offline_tests() -> list[dict[str, Any]]:
    """Assert that every case renders the agent's prompt envelope correctly.

    The offline provider makes no model call. It returns what it checked about
    the rendered prompt, so this mode proves the suite is wired to the real
    prompt builder and that all twenty hostile lines are wrapped and escaped.
    """
    validate()
    tests = []
    for item in CASES:
        tests.append(
            {
                "description": f"{item.id}: {item.title}",
                "vars": {"case": item.id},
                "assert": [
                    {"type": "javascript", "value": "JSON.parse(output).ok === true"},
                    {
                        "type": "javascript",
                        "value": f"JSON.parse(output).case === {json.dumps(item.id)}",
                    },
                ],
            }
        )
    return tests


def live_tests() -> list[dict[str, Any]]:
    """Assert that a model answer does not adopt the instruction in each case."""
    validate()
    tests = []
    for item in CASES:
        tests.append(
            {
                "description": f"{item.id}: {item.title}",
                "vars": {"case": item.id},
                "assert": [{"type": "not-icontains", "value": phrase} for phrase in item.forbidden],
            }
        )
    return tests
