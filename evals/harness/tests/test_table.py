"""Tests for the README results table generator."""

from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from pagerless_eval import table

from tests.support import document, run


class RenderTest(unittest.TestCase):
    def test_no_live_run_states_that_nothing_has_been_published(self) -> None:
        block = table.render([])
        self.assertIn("No live evaluation has been published yet", block)
        self.assertNotIn("|", block)

    def test_a_replay_document_is_not_published(self) -> None:
        block = table.render([document(provider="replay", evidence="replay")])
        self.assertIn("No live evaluation has been published yet", block)

    def test_a_dirty_live_batch_is_not_published(self) -> None:
        block = table.render([document(dirty=True)])
        self.assertIn("No live evaluation has been published yet", block)

    def test_a_live_document_states_the_model_and_run_count(self) -> None:
        block = table.render([document([run(), run(repeat=2)])])
        self.assertIn("**Model:** Claude Haiku 5.5", block)
        self.assertIn("**Runs:** 2", block)
        self.assertIn("1. oom", block)
        self.assertIn("10m 0s", block)

    def test_the_total_row_counts_every_result(self) -> None:
        block = table.render([document([run(result="pass"), run(repeat=2, result="fail")])])
        self.assertIn("| **Total** | | **1** | **0** | **1** |", block)

    def test_the_duration_and_cost_helpers_are_readable(self) -> None:
        self.assertEqual(table._duration(612), "10m 12s")
        self.assertEqual(table._duration(42), "42s")
        self.assertEqual(table._cost(0.002), "$0.0020")


class BlockTest(unittest.TestCase):
    def test_replace_block_keeps_the_rest_of_the_readme(self) -> None:
        readme = f"before\n{table.BEGIN}\nold\n{table.END}\nafter\n"
        replaced = table.replace_block(readme, "new")
        self.assertEqual(replaced, f"before\n{table.BEGIN}\nnew\n{table.END}\nafter\n")

    def test_missing_markers_are_refused(self) -> None:
        with self.assertRaises(ValueError):
            table.replace_block("no markers here", "new")

    def test_the_cli_updates_then_checks_the_readme(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            readme = root / "README.md"
            readme.write_text(f"{table.BEGIN}\nold\n{table.END}\n", encoding="utf-8")
            results = root / "results"
            results.mkdir()
            self.assertEqual(
                table.main(["--update-readme", "--readme", str(readme), "--results-dir", str(results)]),
                0,
            )
            self.assertIn("No live evaluation has been published yet", readme.read_text(encoding="utf-8"))
            self.assertEqual(
                table.main(["--check", "--readme", str(readme), "--results-dir", str(results)]),
                0,
            )
            readme.write_text(f"{table.BEGIN}\nstale\n{table.END}\n", encoding="utf-8")
            self.assertEqual(
                table.main(["--check", "--readme", str(readme), "--results-dir", str(results)]),
                1,
            )


if __name__ == "__main__":
    unittest.main()
