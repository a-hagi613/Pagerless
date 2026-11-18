"""Tests for the versioned result documents and their summary."""

from __future__ import annotations

import subprocess
import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path

from pagerless_eval import results

from tests.support import document, run


class ResultDocumentTest(unittest.TestCase):
    def test_a_complete_document_validates(self) -> None:
        results.validate_document(document())

    def test_an_invalid_result_is_refused(self) -> None:
        broken = document()
        broken["runs"][0]["result"] = "maybe"
        with self.assertRaises(results.ResultError):
            results.validate_document(broken)

    def test_a_nonterminal_phase_is_refused(self) -> None:
        broken = document()
        broken["runs"][0]["phase"] = "Executing"
        with self.assertRaises(results.ResultError):
            results.validate_document(broken)

    def test_a_timed_out_run_is_recorded_as_a_failure(self) -> None:
        timed_out = run(result="fail")
        timed_out["phase"] = "TimedOut"
        timed_out["failure"] = "the Triage did not reach a terminal phase"
        results.validate_document(document([timed_out]))

    def test_a_missing_field_is_named(self) -> None:
        broken = document()
        del broken["runs"][0]["score"]
        with self.assertRaisesRegex(results.ResultError, "score"):
            results.validate_document(broken)

    def test_a_run_without_its_attempts_is_refused(self) -> None:
        broken = document()
        del broken["runs"][0]["validationAttempts"]
        with self.assertRaisesRegex(results.ResultError, "validationAttempts"):
            results.validate_document(broken)

    def test_a_refusal_must_record_its_errors(self) -> None:
        broken = document()
        broken["runs"][0]["refusals"] = [{"attempt": 1, "action": "Scale"}]
        with self.assertRaisesRegex(results.ResultError, "refusal"):
            results.validate_document(broken)
        with self.assertRaises(results.ResultError) as context:
            results.validate_document(broken)
        self.assertIn("score", str(context.exception))

    def test_negative_usage_is_refused(self) -> None:
        broken = document()
        broken["runs"][0]["usage"]["costUSD"] = -1
        with self.assertRaises(results.ResultError):
            results.validate_document(broken)

    def test_the_path_carries_the_date_commit_and_evidence(self) -> None:
        path = results.result_path(Path("/tmp"), datetime(2026, 11, 17, tzinfo=UTC), "a" * 40, "live")
        self.assertEqual(path.name, "2026-11-17-aaaaaaaaaaaa-live.json")

    def test_an_invalid_evidence_mode_is_refused(self) -> None:
        with self.assertRaises(results.ResultError):
            results.result_path(Path("/tmp"), datetime.now(UTC), "a" * 40, "made-up")

    def test_write_merges_runs_and_keeps_the_document_valid(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "result.json"
            first = document()
            results.write_document(path, first)
            second = document([run(repeat=2)])
            results.write_document(path, second)
            stored = results.load_document(path)
            self.assertEqual([item["repeat"] for item in stored["runs"]], [1, 2])

    def test_write_replaces_the_same_scenario_and_repeat(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "result.json"
            results.write_document(path, document([run(result="fail")]))
            results.write_document(path, document([run(result="pass")]))
            stored = results.load_document(path)
            self.assertEqual(len(stored["runs"]), 1)
            self.assertEqual(stored["runs"][0]["result"], "pass")

    def test_write_refuses_a_document_from_another_commit(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "result.json"
            results.write_document(path, document())
            other = document()
            other["pagerlessCommit"] = "e" * 40
            with self.assertRaises(results.ResultError):
                results.write_document(path, other)

    def test_totals_exclude_replay_and_dirty_batches(self) -> None:
        live = document([run(), run(repeat=2)])
        replay = document([run()], provider="replay", evidence="replay")
        dirty = document([run()], dirty=True)
        summary = results.totals([live, replay, dirty])
        self.assertEqual((summary.runs, summary.scenarios), (2, 1))
        self.assertAlmostEqual(summary.mean_seconds_to_recovered or 0, 600.0)
        self.assertAlmostEqual(summary.total_cost_usd, 0.004)

    def test_totals_are_empty_without_a_live_run(self) -> None:
        summary = results.totals([document(provider="replay", evidence="replay")])
        self.assertEqual(summary.runs, 0)
        self.assertIsNone(summary.mean_seconds_to_recovered)

    def test_model_labels_are_fixed_for_the_measured_provider(self) -> None:
        self.assertEqual(results.model_label("anthropic"), ("claude-haiku-5-5", "Claude Haiku 5.5"))
        with self.assertRaises(results.ResultError):
            results.model_label("someone-else")


class RevisionTest(unittest.TestCase):
    def test_a_git_checkout_reports_its_commit_and_dirtiness(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            _git(repo, "init", "--quiet")
            _git(repo, "config", "user.email", "eval@pagerless.dev")
            _git(repo, "config", "user.name", "Pagerless Eval")
            tracked = repo / "file.txt"
            tracked.write_text("one\n", encoding="utf-8")
            _git(repo, "add", "file.txt")
            _git(repo, "commit", "--quiet", "-m", "one")
            commit, dirty = results.pagerless_revision(repo)
            self.assertRegex(commit, r"^[0-9a-f]{40}$")
            self.assertFalse(dirty)
            tracked.write_text("two\n", encoding="utf-8")
            _, dirty = results.pagerless_revision(repo)
            self.assertTrue(dirty)

    def test_a_directory_without_git_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(results.ResultError):
            results.pagerless_revision(Path(directory))


def _git(repo: Path, *arguments: str) -> None:
    subprocess.run(["git", "-C", str(repo), *arguments], check=True, capture_output=True)


if __name__ == "__main__":
    unittest.main()
