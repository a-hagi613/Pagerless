"""Tests for the run-record mapping, the CI evidence gate, the budget, and reset."""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import MagicMock, call, patch

from pagerless_eval import results
from pagerless_eval.answer_key import scenario
from pagerless_eval.runner import (
    ALERT_POLL_INTERVAL,
    ALERTMANAGER_ALERTS,
    FLAG_WORKLOADS,
    BudgetExhaustedError,
    Config,
    Harness,
    HarnessError,
    Timeouts,
)
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


def alertmanager(*labels: dict[str, str]) -> subprocess.CompletedProcess[str]:
    """Return a kubectl result that carries the given active alerts."""
    return subprocess.CompletedProcess(
        args=[],
        returncode=0,
        stdout=json.dumps([{"labels": item} for item in labels]),
        stderr="",
    )


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

    def test_adapter_check_runs_only_for_pull_request_actions(self) -> None:
        cases = (
            ("NoAction", None, "Diagnosed", scenario("cpu"), False),
            ("RestartWorkload", {}, "Recovered", scenario("email-memory-leak"), False),
            (
                "PatchResources",
                {
                    "patchResources": {
                        "container": "memory-holder",
                        "requests": {"memory": "64Mi"},
                        "limits": {"memory": "128Mi"},
                    }
                },
                "Recovered",
                scenario("oom"),
                True,
            ),
        )
        for action, parameters, terminal_phase, item, expect_adapter_check in cases:
            with self.subTest(action=action):
                snapshots = [
                    triage(action, parameters, phase="Proposed", decision=None),
                    triage(action, parameters, phase="Approved", decision="eval-approver"),
                    triage(action, parameters, phase=terminal_phase, decision="eval-approver"),
                ]
                self.harness.kube = MagicMock()
                self.harness.kube.get_json.side_effect = snapshots
                evaluator = MagicMock()
                with patch.object(self.harness, "_adapter_check") as adapter_check:
                    final_triage, phases = self.harness._drive_triage("triage-eval", item, evaluator)
                evaluator.create.assert_called_once()
                self.assertEqual(final_triage["status"]["phase"], terminal_phase)
                self.assertEqual(
                    [entry["phase"] for entry in phases],
                    ["Proposed", terminal_phase],
                )
                if expect_adapter_check:
                    adapter_check.assert_called_once_with("triage-eval")
                else:
                    adapter_check.assert_not_called()

    def test_a_run_resets_with_its_scenario(self) -> None:
        item = scenario("oom")
        with (
            patch.object(self.harness, "_set_recording"),
            patch.object(self.harness, "_reset") as reset,
            patch.object(self.harness, "_memory_settings", return_value=MemorySettings()),
            patch.object(self.harness, "_delete_triages"),
            patch.object(self.harness, "_break"),
            patch.object(self.harness, "_sandbox_revision", return_value="a" * 40),
            patch.object(self.harness, "_max_node_memory", return_value="1Gi"),
            patch.object(self.harness, "_wait_for_triage", return_value=oom_triage()),
            patch.object(self.harness, "_drive_triage", return_value=(oom_triage(), [])),
            patch.object(self.harness, "_gateway_evidence", return_value=GatewayEvidence()),
        ):
            self.harness._run_once(item, 1, MagicMock())
        reset.assert_called_once_with(item)

    def test_adapter_reset_clears_stresschaos_and_replaces_flag_workloads(self) -> None:
        self.harness.kube = MagicMock()
        with (
            patch.object(self.harness, "_adapter_client") as adapter_reset,
            patch.object(self.harness, "_wait_sandbox_healthy") as healthy,
            patch.object(self.harness, "_wait_alert_inactive") as wait_alert,
        ):
            self.harness._reset(scenario("cpu"))
        adapter_reset.assert_called_once_with("reset")
        healthy.assert_called_once_with()
        wait_alert.assert_not_called()
        calls = self.harness.kube.run.call_args_list
        self.assertIn(
            call(
                "-n",
                "sandbox",
                "delete",
                "stresschaos",
                "--all",
                "--ignore-not-found",
                "--wait=true",
                check=False,
            ),
            calls,
        )
        for workload in FLAG_WORKLOADS:
            with self.subTest(workload=workload):
                self.assertIn(
                    call(
                        "-n",
                        "sandbox",
                        "delete",
                        "pod",
                        "-l",
                        f"opentelemetry.io/name={workload}",
                        "--ignore-not-found",
                        "--wait=true",
                        timeout=300,
                    ),
                    calls,
                )
                self.assertIn(
                    call(
                        "-n",
                        "sandbox",
                        "rollout",
                        "status",
                        f"deployment/{workload}",
                        "--timeout=180s",
                        timeout=240,
                    ),
                    calls,
                )
        replaced = [
            item.args[5].removeprefix("opentelemetry.io/name=")
            for item in calls
            if item.args[:4] == ("-n", "sandbox", "delete", "pod")
        ]
        self.assertEqual(tuple(replaced), FLAG_WORKLOADS)

    def test_a_live_reset_leaves_replacement_to_hack_reset(self) -> None:
        self.harness.config.adapter = False
        self.harness.kube = MagicMock()
        with (
            patch.object(self.harness, "_make") as make,
            patch.object(self.harness, "_wait_sandbox_healthy"),
            patch.object(self.harness, "_wait_alert_inactive"),
        ):
            self.harness._reset(scenario("cpu"))
        make.assert_called_once_with("reset", timeout=self.harness.config.timeouts.reset)
        self.harness.kube.run.assert_called_once_with(
            "-n",
            "sandbox",
            "rollout",
            "status",
            "deployment/checkout",
            "--timeout=600s",
            timeout=660,
        )

    def test_the_reset_waits_for_the_scenarios_alert_to_clear(self) -> None:
        self.harness.kube = MagicMock()
        item = scenario("oom")
        with (
            patch.object(self.harness, "_adapter_client"),
            patch.object(self.harness, "_wait_sandbox_healthy"),
            patch.object(self.harness, "_replace_flag_workloads"),
            patch.object(self.harness, "_wait_alert_inactive") as wait_alert,
        ):
            self.harness._reset(item)
        wait_alert.assert_called_once_with(item)

    def test_an_inactive_alert_returns_after_one_query(self) -> None:
        self.harness.kube = MagicMock()
        self.harness.kube.run.return_value = alertmanager(
            {"alertname": "ContainerOOMKilled", "deployment": "cart"},
        )
        with patch("pagerless_eval.runner.time.sleep") as sleep:
            self.harness._wait_alert_inactive(scenario("oom"))
        self.harness.kube.run.assert_called_once_with(
            "get", "--raw", ALERTMANAGER_ALERTS, timeout=60, check=False
        )
        sleep.assert_not_called()

    def test_the_alert_wait_polls_until_the_matching_alert_clears(self) -> None:
        self.harness.kube = MagicMock()
        firing = alertmanager({"alertname": "ContainerOOMKilled", "deployment": "checkout"})
        self.harness.kube.run.side_effect = [firing, firing, alertmanager()]
        with patch("pagerless_eval.runner.time.sleep") as sleep:
            self.harness._wait_alert_inactive(scenario("oom"))
        self.assertEqual(self.harness.kube.run.call_count, 3)
        self.assertEqual(sleep.call_count, 2)
        sleep.assert_called_with(ALERT_POLL_INTERVAL)

    def test_the_alert_wait_fails_when_the_alert_persists(self) -> None:
        self.harness.config.timeouts = Timeouts(alert_inactive=0)
        self.harness.kube = MagicMock()
        self.harness.kube.run.return_value = alertmanager(
            {"alertname": "ContainerOOMKilled", "deployment": "checkout"},
        )
        with patch("pagerless_eval.runner.time.sleep"), self.assertRaises(HarnessError) as context:
            self.harness._wait_alert_inactive(scenario("oom"))
        self.assertIn("ContainerOOMKilled", str(context.exception))

    def test_an_unreadable_alertmanager_fails_instead_of_assuming_a_clear_alert(self) -> None:
        self.harness.config.timeouts = Timeouts(alert_inactive=0)
        self.harness.kube = MagicMock()
        self.harness.kube.run.return_value = subprocess.CompletedProcess(
            args=[], returncode=1, stdout="", stderr="the server could not find the requested resource"
        )
        with patch("pagerless_eval.runner.time.sleep"), self.assertRaises(HarnessError) as context:
            self.harness._wait_alert_inactive(scenario("oom"))
        self.assertIn("Alertmanager", str(context.exception))

    def test_a_completed_run_appends_a_progress_line_to_the_step_summary(self) -> None:
        record = self.harness._record(scenario("oom"), 1, oom_triage(), [], self.evidence(), "")
        self.harness.completed = 1
        summary = Path(self.directory.name) / "summary.md"
        with patch.dict(os.environ, {"GITHUB_STEP_SUMMARY": str(summary)}):
            self.harness._print_run(record)
            self.harness.completed = 2
            self.harness._print_run(record)
        lines = summary.read_text(encoding="utf-8").splitlines()
        self.assertEqual(len(lines), 2)
        self.assertEqual(
            lines[0],
            "- [1/1] oom repeat 1: PatchResources -> pass "
            "(Recovered, 600s to Recovered, 1000+100 tokens, $0.0020)",
        )
        self.assertTrue(lines[1].startswith("- [2/1] oom repeat 1: PatchResources -> pass"))

    def test_no_step_summary_is_written_when_the_variable_is_unset(self) -> None:
        record = self.harness._record(scenario("oom"), 1, oom_triage(), [], self.evidence(), "")
        environment = {key: value for key, value in os.environ.items() if key != "GITHUB_STEP_SUMMARY"}
        with patch.dict(os.environ, environment, clear=True):
            self.harness._print_run(record)
        self.assertFalse((Path(self.directory.name) / "summary.md").exists())

    def test_a_run_failure_names_the_scenario_and_the_elapsed_time(self) -> None:
        with (
            patch.object(
                self.harness,
                "_run_once",
                side_effect=HarnessError("the Triage did not reach Proposed within 720s"),
            ),
            self.assertRaises(HarnessError) as context,
        ):
            self.harness._run_with_context(scenario("oom"), 2, MagicMock())
        message = str(context.exception)
        self.assertIn("'oom' repeat 2 stopped after", message)
        self.assertIn("did not reach Proposed", message)


if __name__ == "__main__":
    unittest.main()
