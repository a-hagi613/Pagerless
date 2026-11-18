"""Tests for the versioned result documents, the sharded merge, and their summary."""

from __future__ import annotations

import subprocess
import tempfile
import unittest
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from pagerless_eval import results, table
from pagerless_eval.answer_key import SCENARIOS
from pagerless_eval.answer_key import scenario as scenario_by_name

from tests.support import STARTED, document, run

# The shard matrix that the full paid workflow dispatches, in its order.
SHARDS: tuple[tuple[str, ...], ...] = (
    ("oom", "bad-config"),
    ("bad-image-tag", "unschedulable"),
    ("cart-flag", "payment-flag"),
    ("product-catalog-flag", "cpu", "injection"),
    ("email-memory-leak",),
)
COMMIT = "f" * 40


def shard(
    names: tuple[str, ...],
    *,
    repeats: int = 2,
    started: str = STARTED,
    result: str = "pass",
    commit: str = COMMIT,
    evidence: str = "live",
    dirty: bool = False,
    max_tokens: int = 400_000,
    max_cost_usd: float = 0.9,
) -> dict[str, Any]:
    """Build one shard document that records every repeat of its Scenarios."""
    runs: list[dict[str, Any]] = []
    for name in names:
        item = scenario_by_name(name)
        for repeat in range(1, repeats + 1):
            runs.append(
                run(
                    scenario=item.name,
                    number=item.number,
                    repeat=repeat,
                    result=result,
                    workload=item.workload,
                )
            )
    spent_tokens = sum(
        item["usage"]["inputTokens"] + item["usage"]["outputTokens"] for item in runs
    )
    return results.build_document(
        commit=commit,
        dirty=dirty,
        provider="anthropic",
        evidence=evidence,
        scenarios=names,
        repeats=repeats,
        started_at=datetime.fromisoformat(started.replace("Z", "+00:00")),
        budget={
            "maxTokens": max_tokens,
            "maxCostUSD": max_cost_usd,
            "spentTokens": spent_tokens,
            "spentCostUSD": round(sum(item["usage"]["costUSD"] for item in runs), 6),
            "exhausted": False,
        },
        runs=runs,
    )


def shards() -> list[dict[str, Any]]:
    return [shard(names) for names in SHARDS]


def single_run_shard(name: str, repeat: int) -> dict[str, Any]:
    """Build the document one single-run shard of a full batch publishes."""
    item = scenario_by_name(name)
    return results.build_document(
        commit=COMMIT,
        dirty=False,
        provider="anthropic",
        evidence="live",
        scenarios=(name,),
        repeats=2,
        started_at=datetime.fromisoformat(STARTED.replace("Z", "+00:00")),
        budget={
            "maxTokens": 150_000,
            "maxCostUSD": 1.0,
            "spentTokens": 1100,
            "spentCostUSD": 0.002,
            "exhausted": False,
        },
        runs=[run(scenario=name, number=item.number, repeat=repeat, workload=item.workload)],
    )


class MergeDocumentsTest(unittest.TestCase):
    def test_five_shards_merge_into_one_twenty_run_document(self) -> None:
        merged = results.merge_documents(shards(), commit=COMMIT)
        results.validate_document(merged)
        self.assertEqual(len(merged["runs"]), 20)
        self.assertEqual(merged["evidence"], "live")
        self.assertFalse(merged["pagerlessDirty"])
        self.assertEqual(
            [(item["scenario"], item["repeat"]) for item in merged["runs"]],
            [(item.name, repeat) for item in SCENARIOS for repeat in (1, 2)],
        )
        self.assertEqual([item.name for item in SCENARIOS], merged["scenarios"])
        self.assertEqual(merged["repeats"], 2)

    def test_the_merge_starts_at_the_earliest_shard(self) -> None:
        documents = shards()
        documents[2]["startedAt"] = "2026-11-17T11:41:00Z"
        merged = results.merge_documents(documents, commit=COMMIT)
        self.assertEqual(merged["startedAt"], "2026-11-17T11:41:00Z")

    def test_the_merge_sums_the_spend_and_the_shard_budgets(self) -> None:
        merged = results.merge_documents(shards(), commit=COMMIT)
        self.assertEqual(merged["budget"]["maxTokens"], 5 * 400_000)
        self.assertAlmostEqual(merged["budget"]["maxCostUSD"], 4.5)
        # Every run in the fixtures records 1000 input + 100 output tokens and $0.002.
        self.assertEqual(merged["budget"]["spentTokens"], 20 * 1100)
        self.assertAlmostEqual(merged["budget"]["spentCostUSD"], 20 * 0.002)
        self.assertFalse(merged["budget"]["exhausted"])

    def test_an_exhausted_shard_marks_the_merged_budget(self) -> None:
        documents = shards()
        documents[4]["budget"]["exhausted"] = True
        merged = results.merge_documents(documents, commit=COMMIT)
        self.assertTrue(merged["budget"]["exhausted"])

    def test_a_spend_above_the_shard_budgets_marks_the_merged_budget(self) -> None:
        documents = shards()
        for item in documents:
            item["budget"]["maxTokens"] = 0
            item["budget"]["maxCostUSD"] = 0.0
        merged = results.merge_documents(documents, commit=COMMIT)
        self.assertFalse(merged["budget"]["exhausted"])
        documents = shards()
        for item in documents:
            item["budget"]["maxTokens"] = 1000
        merged = results.merge_documents(documents, commit=COMMIT)
        self.assertTrue(merged["budget"]["exhausted"])

    def test_a_single_shard_merges_by_itself(self) -> None:
        merged = results.merge_documents([shard(("oom",), repeats=1)], commit=COMMIT)
        self.assertEqual(len(merged["runs"]), 1)
        self.assertEqual(merged["scenarios"], ["oom"])
        self.assertEqual(merged["repeats"], 1)

    def test_twenty_single_run_shards_merge_into_one_document(self) -> None:
        # A full batch gives one runner one run of one Scenario, while every shard
        # document still declares the batch's two repeats.
        documents = [
            single_run_shard(item.name, repeat) for item in SCENARIOS for repeat in (1, 2)
        ]
        merged = results.merge_documents(
            documents,
            commit=COMMIT,
            scenarios=tuple(item.name for item in SCENARIOS),
        )
        results.validate_document(merged)
        self.assertEqual(len(merged["runs"]), 20)
        self.assertEqual(merged["repeats"], 2)
        self.assertEqual(
            [(item["scenario"], item["repeat"]) for item in merged["runs"]],
            [(item.name, repeat) for item in SCENARIOS for repeat in (1, 2)],
        )
        self.assertEqual(merged["budget"]["maxTokens"], 20 * 150_000)
        self.assertEqual(merged["budget"]["spentTokens"], 20 * 1100)

    def test_a_single_run_shard_missing_from_the_batch_is_refused(self) -> None:
        documents = [
            single_run_shard(item.name, repeat) for item in SCENARIOS for repeat in (1, 2)
        ]
        with self.assertRaisesRegex(results.ResultError, "missing"):
            results.merge_documents(
                documents[:-1], commit=COMMIT, scenarios=tuple(item.name for item in SCENARIOS)
            )

    def test_a_single_run_shard_renders_as_the_published_table(self) -> None:
        merged = results.merge_documents(
            [single_run_shard(item.name, repeat) for item in SCENARIOS for repeat in (1, 2)],
            commit=COMMIT,
            scenarios=tuple(item.name for item in SCENARIOS),
        )
        rendered = table.render([merged])
        self.assertIn("**Runs:** 20 across 10 Scenarios", rendered)

    def test_the_merged_document_renders_as_the_published_table(self) -> None:
        merged = results.merge_documents(shards(), commit=COMMIT)
        rendered = table.render([merged])
        self.assertIn("**Runs:** 20 across 10 Scenarios", rendered)
        self.assertIn("**Source:** 1 result document(s)", rendered)
        for item in SCENARIOS:
            with self.subTest(scenario=item.name):
                self.assertIn(f"{item.number}. {item.name}", rendered)

    def test_the_merge_refuses_a_shard_from_another_commit(self) -> None:
        documents = shards()
        documents[1]["pagerlessCommit"] = "e" * 40
        with self.assertRaisesRegex(results.ResultError, "not ffff"):
            results.merge_documents(documents, commit=COMMIT)

    def test_the_merge_refuses_a_replay_shard(self) -> None:
        with self.assertRaisesRegex(results.ResultError, "not live"):
            results.merge_documents([shard(("oom",), repeats=1, evidence="replay")], commit=COMMIT)

    def test_the_merge_refuses_a_dirty_shard(self) -> None:
        documents = shards()
        documents[0]["pagerlessDirty"] = True
        with self.assertRaisesRegex(results.ResultError, "dirty"):
            results.merge_documents(documents, commit=COMMIT)

    def test_the_merge_refuses_a_batch_that_misses_a_shard(self) -> None:
        with self.assertRaisesRegex(results.ResultError, "no shard document covers"):
            results.merge_documents(
                shards()[:4], commit=COMMIT, scenarios=tuple(item.name for item in SCENARIOS)
            )
        incomplete = shards()
        incomplete[0]["runs"] = [item for item in incomplete[0]["runs"] if item["repeat"] == 1]
        with self.assertRaisesRegex(results.ResultError, "missing"):
            results.merge_documents(incomplete, commit=COMMIT)

    def test_the_merge_keeps_a_completed_scored_fail_run(self) -> None:
        documents = shards()
        # A live model miss is a measurement, not a merge error: the Triage
        # completed cleanly in the Diagnosed phase but proposed NoAction where
        # the answer key expects the recorded fix.
        missed = [item for item in documents[3]["runs"] if item["scenario"] == "cpu"]
        self.assertEqual(len(missed), 2)
        for item in missed:
            item["phase"] = "Diagnosed"
            item["action"] = "NoAction"
            item["failure"] = None
            item["result"] = "fail"
        merged = results.merge_documents(documents, commit=COMMIT)
        results.validate_document(merged)
        rendered = table.render([merged])
        self.assertIn("| **Total** | | **18** | **0** | **2** |", rendered)

    def test_the_merge_refuses_an_incomplete_or_errored_run(self) -> None:
        cases = (
            ("failure set", {"failure": "the agent Job exhausted its retries"}),
            ("timed out", {"phase": "TimedOut"}),
            ("failed", {"phase": "Failed"}),
        )
        for label, change in cases:
            with self.subTest(run=label):
                documents = shards()
                documents[4]["runs"][0].update(change)
                with self.assertRaisesRegex(
                    results.ResultError, "incomplete or errored runs"
                ):
                    results.merge_documents(documents, commit=COMMIT)

    def test_the_merge_refuses_two_shards_for_the_same_run(self) -> None:
        documents = shards()
        documents[1] = shard(("oom", "bad-image-tag", "unschedulable"))
        with self.assertRaisesRegex(results.ResultError, "more than one shard"):
            results.merge_documents(documents, commit=COMMIT)

    def test_the_merge_refuses_disagreeing_repeat_counts(self) -> None:
        documents = shards()
        documents[4] = shard(("email-memory-leak",), repeats=1)
        with self.assertRaisesRegex(results.ResultError, "repeat count"):
            results.merge_documents(documents, commit=COMMIT)

    def test_the_merge_refuses_a_different_repeat_count(self) -> None:
        with self.assertRaisesRegex(results.ResultError, "not 1"):
            results.merge_documents(shards(), commit=COMMIT, repeats=1)

    def test_the_merge_refuses_no_documents(self) -> None:
        with self.assertRaisesRegex(results.ResultError, "no shard documents"):
            results.merge_documents([], commit=COMMIT)


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


class MergeCommandTest(unittest.TestCase):
    """The `python -m pagerless_eval.results merge` command the workflow runs."""

    def setUp(self) -> None:
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        self.shards = self.root / "shards"
        # Each artifact extracts into its own directory that mirrors the upload.
        for index, names in enumerate(SHARDS, start=1):
            artifact = self.shards / f"eval-results-{COMMIT}-full-{index}-shard"
            document = shard(names)
            started = datetime.fromisoformat(document["startedAt"].replace("Z", "+00:00"))
            path = results.result_path(
                artifact / "evals" / "results", started, COMMIT, "live"
            )
            results.write_document(path, document)

    def tearDown(self) -> None:
        self.directory.cleanup()

    def test_the_command_writes_the_merged_document_and_the_summary(self) -> None:
        out = self.root / "out"
        summary = self.root / "summary.md"
        exit_code = results.main(
            [
                "merge",
                "--shards",
                str(self.shards),
                "--out-dir",
                str(out),
                "--commit",
                COMMIT,
                "--scenarios",
                "all",
                "--summary",
                str(summary),
            ]
        )
        self.assertEqual(exit_code, 0)
        written = sorted(out.glob("*.json"))
        self.assertEqual([path.name for path in written], ["2026-11-17-ffffffffffff-live.json"])
        stored = results.load_document(written[0])
        self.assertEqual(len(stored["runs"]), 20)
        self.assertIn("**Runs:** 20 across 10 Scenarios", summary.read_text(encoding="utf-8"))

    def test_the_command_refuses_a_directory_without_a_shard(self) -> None:
        empty = self.root / "empty"
        empty.mkdir()
        self.assertEqual(
            results.main(
                ["merge", "--shards", str(empty), "--out-dir", str(self.root / "out"), "--commit", COMMIT]
            ),
            1,
        )


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
