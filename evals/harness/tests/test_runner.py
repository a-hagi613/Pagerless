"""Tests for the run-record mapping, the CI evidence gate, and the budget."""

from __future__ import annotations

import subprocess
import tempfile
import unittest
from pathlib import Path

from pagerless_eval import results
from pagerless_eval.answer_key import scenario
from pagerless_eval.runner import BudgetExhaustedError, Config, Harness, HarnessError
from pagerless_eval.score import GatewayEvidence, MemorySettings, RunEvidence

from tests.support import triage


def git_checkout(path: Path) -> None:
    subprocess.run(["git", "-C", str(path), "init", "--quiet"], check=True, capture_output=True)
    subprocess.run(["git", "-C", str(path), "config", "user.email", "eval@pagerless.dev"], check=True)
    subprocess.run(["git", "-C", str(path), "config", "user.name", "Eval"], check=True)
    (path / "file").write_text("one\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(path), "add", "file"], check=True)
    subprocess.run(
        ["git", "-C", str(path), "commit", "--quiet", "-m", "one"], check=True, capture_output=True
    )


def oom_triage(**kwargs: object) -> dict[str, object]:
    parameters = {
        "patchResources": {
            "container": "memory-holder",
            "requests": {"memory": "64Mi"},
            "limits": {"memory": "128Mi"},
        }
    }
    return triage("PatchResources", parameters, **kwargs)  # type: ignore[arg-type]


class HarnessTest(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = tempfile.TemporaryDirectory()
        root = Path(self.directory.name)
        git_checkout(root)
        self.harness = Harness(
            Config(
                repo_root=root,
                scenarios=(scenario("oom"),),
                repeats=1,
                results_dir=root / "results",
                adapter=True,
                require_pass=True,
                max_tokens=100,
                max_cost_usd=0.0,
                context="kind-test",
            )
        )
        self.harness.provider = "replay"
        self.harness.evidence_mode = "replay"

    def tearDown(self) -> None:
        self.harness.state_dir.rmdir()
        self.directory.cleanup()

    def evidence(self) -> RunEvidence:
        return RunEvidence(
            injected_commit="a" * 40,
            known_good=MemorySettings(requests="64Mi", limits="128Mi"),
            injected=MemorySettings(requests="32Mi", limits="32Mi"),
            max_node_memory="1073741824",
            gateway=GatewayEvidence(),
        )

    def test_a_passing_triage_maps_to_a_valid_record(self) -> None:
        record = self.harness._record(
            scenario("oom"), 1, oom_triage(), [{"phase": "Proposed", "observedAt": "x"}], self.evidence(), ""
        )
        results.validate_run(record)
        self.assertEqual(record["result"], "pass")
        self.assertEqual(record["approvedBy"], "eval-approver")
        self.assertEqual(record["timeToRecoveredSeconds"], 600.0)

    def test_a_refused_attempt_is_recorded_with_its_action_and_errors(self) -> None:
        record = self.harness._record(
            scenario("oom"),
            1,
            oom_triage(
                validation_attempts=1,
                refusals=[
                    "attempt 1: PatchResources container=checkout refused: "
                    "proposal.parameters.patchResources.requests[memory]: "
                    "the memory request 64Mi would exceed its limit 32Mi"
                ],
            ),
            [],
            self.evidence(),
            "",
        )
        results.validate_run(record)
        self.assertEqual(record["validationAttempts"], 1)
        self.assertEqual(
            record["refusals"],
            [
                {
                    "attempt": 1,
                    "action": "PatchResources",
                    "parameters": "container=checkout",
                    "errors": "proposal.parameters.patchResources.requests[memory]: "
                    "the memory request 64Mi would exceed its limit 32Mi",
                }
            ],
        )

    def test_a_nonterminal_triage_is_recorded_as_timed_out_and_failed(self) -> None:
        record = self.harness._record(
            scenario("oom"),
            1,
            triage("PatchResources", {"patchResources": {"container": "memory-holder"}}, phase="Executing"),
            [],
            self.evidence(),
            "the Triage did not reach a terminal phase",
        )
        results.validate_run(record)
        self.assertEqual(record["phase"], "TimedOut")
        self.assertEqual(record["result"], "fail")

    def test_the_ci_gate_requires_passing_checks_and_the_configured_window(self) -> None:
        record = self.harness._record(scenario("oom"), 1, oom_triage(), [], self.evidence(), "")
        self.harness._assert_ci_evidence(record)
        short = {**record, "verification": {**record["verification"], "windowSeconds": 120.0}}
        with self.assertRaises(HarnessError):
            self.harness._assert_ci_evidence(short)
        unchecked = {**record, "verification": {**record["verification"], "checks": []}}
        with self.assertRaises(HarnessError):
            self.harness._assert_ci_evidence(unchecked)

    def test_the_ci_gate_uses_a_shorter_configured_window(self) -> None:
        record = self.harness._record(scenario("oom"), 1, oom_triage(), [], self.evidence(), "")
        self.assertEqual(self.harness.config.verification_window, 300)
        self.harness.config.verification_window = 60
        observed = {**record, "verification": {**record["verification"], "windowSeconds": 61.0}}
        self.harness._assert_ci_evidence(observed)
        too_short = {**record, "verification": {**record["verification"], "windowSeconds": 45.0}}
        with self.assertRaises(HarnessError):
            self.harness._assert_ci_evidence(too_short)

    def test_the_ci_gate_requires_the_eval_approver(self) -> None:
        record = self.harness._record(scenario("oom"), 1, oom_triage(), [], self.evidence(), "")
        with self.assertRaises(HarnessError):
            self.harness._assert_ci_evidence({**record, "approvedBy": "someone-else"})

    def test_a_spent_budget_fails_closed(self) -> None:
        self.harness.spent_tokens = 100
        with self.assertRaises(BudgetExhaustedError):
            self.harness._check_budget()
        self.assertTrue(self.harness.budget_exhausted)

    def test_an_overshooting_run_marks_the_budget_exhausted(self) -> None:
        record = self.harness._record(scenario("oom"), 1, oom_triage(), [], self.evidence(), "")
        record["usage"]["inputTokens"] = 500
        self.harness._add_spend(record)
        self.assertTrue(self.harness.budget_exhausted)


if __name__ == "__main__":
    unittest.main()
