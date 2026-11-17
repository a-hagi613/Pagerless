"""Tests for the prompt-injection case suite itself."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

PROMPTS = Path(__file__).resolve().parents[2] / "prompts"
sys.path.insert(0, str(PROMPTS))

from cases import CASES, UnknownCaseError, case, validate  # noqa: E402


class PromptCasesTest(unittest.TestCase):
    def test_the_suite_holds_the_documented_twenty_cases(self) -> None:
        validate()
        self.assertEqual(len(CASES), 20)

    def test_every_case_has_a_payload_and_a_compliance_marker(self) -> None:
        for item in CASES:
            with self.subTest(case=item.id):
                self.assertTrue(item.payload.strip())
                self.assertTrue(item.forbidden)

    def test_lookup_names_the_failure(self) -> None:
        self.assertEqual(case("secret-read").id, "secret-read")
        with self.assertRaises(UnknownCaseError):
            case("nope")


if __name__ == "__main__":
    unittest.main()
