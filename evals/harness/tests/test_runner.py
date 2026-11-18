"""Tests for the run-record mapping, the CI evidence gate, the budget, and reset."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import unittest
from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path
from unittest.mock import MagicMock, call, patch

from pagerless_eval import results
from pagerless_eval.answer_key import scenario
from pagerless_eval.runner import (
    ALERT_POLL_INTERVAL,
    ALERTMANAGER_ALERTS,
    EMAIL_SELECTOR,
    FLAG_WORKLOADS,
    GATEWAY_EVIDENCE_POLL_INTERVAL,
    GATEWAY_EVIDENCE_TIMEOUT,
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


def email_pod(
    *,
    phase: str = "Running",
    ready: bool = True,
    restart_count: int = 0,
) -> dict[str, object]:
    """Return an email pod as kubectl reports it, with the health the reset reads."""
    return {
        "metadata": {"name": "email-6c9f7d4b5-x2mnq"},
        "status": {
            "phase": phase,
            "conditions": [{"type": "Ready", "status": "True" if ready else "False"}],
            "containerStatuses": [{"name": "email", "restartCount": restart_count}],
        },
    }


def gateway_step(time: str = "2026-11-17T12:00:10Z") -> dict[str, str]:
    """Return the ToolCall step that records the injected Secret read."""
    return {
        "time": time,
        "kind": "ToolCall",
        "tool": "k8s__resources_get",
        "summary": "k8s__resources_get {'kind': 'Secret', 'name': 'github-write'}",
    }


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

    def new_harness(self, **overrides: object) -> Harness:
        harness = Harness(replace(self.harness.config, **overrides))  # type: ignore[arg-type]
        harness.provider = "replay"
        harness.evidence_mode = "replay"
        self.addCleanup(shutil.rmtree, harness.state_dir, True)
        return harness

    def test_a_repeat_number_runs_only_that_repeat_of_every_scenario(self) -> None:
        for repeat_number in (1, 2):
            with self.subTest(repeat_number=repeat_number):
                harness = self.new_harness(repeats=2, repeat_number=repeat_number)
                self.assertEqual(harness.total, 1)
                with (
                    patch.object(harness, "_preflight"),
                    patch.object(harness, "_write"),
                    patch.object(harness, "_print_run"),
                    patch.object(harness, "_assert_ci_evidence"),
                    patch.object(harness, "_add_spend"),
                    patch.object(harness, "_reset"),
                    patch.object(harness, "_run_with_context", return_value={"usage": {}}) as run_once,
                    patch("pagerless_eval.runner.provision", return_value=Path("kubeconfig")),
                    patch("pagerless_eval.runner.Kubectl"),
                    patch("pagerless_eval.runner.merge_run", return_value=[]),
                ):
                    harness.run()
                self.assertEqual([item.args[1] for item in run_once.call_args_list], [repeat_number])

    def test_a_repeat_number_outside_the_batch_is_refused(self) -> None:
        for repeat_number in (0, 3):
            with self.subTest(repeat_number=repeat_number), self.assertRaises(HarnessError):
                self.new_harness(repeats=2, repeat_number=repeat_number)

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
        # A healthy email pod keeps its age, so only the four flag readers are
        # replaced and ContainerMemoryGrowth keeps the pod it measures.
        self.harness.kube.json.return_value = {"items": [email_pod()]}
        self.harness._flag_dirty = True
        with (
            patch.object(self.harness, "_adapter_client") as adapter_reset,
            patch.object(self.harness, "_wait_sandbox_healthy") as healthy,
            patch.object(self.harness, "_wait_alert_inactive") as wait_alert,
        ):
            self.harness._reset(scenario("cpu"))
        adapter_reset.assert_called_once_with("reset")
        healthy.assert_called_once_with()
        wait_alert.assert_not_called()
        self.assertFalse(self.harness._flag_dirty)
        self.harness.kube.json.assert_called_once_with(
            "get",
            "pods",
            "-n",
            "sandbox",
            "-l",
            EMAIL_SELECTOR,
            "-o",
            "json",
        )
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

    def test_a_reset_keeps_a_healthy_email_pod_so_it_keeps_its_age(self) -> None:
        self.harness.kube = MagicMock()
        self.harness.kube.json.return_value = {"items": [email_pod()]}
        self.harness._flag_dirty = True
        with (
            patch.object(self.harness, "_adapter_client"),
            patch.object(self.harness, "_wait_sandbox_healthy"),
            patch.object(self.harness, "_wait_alert_inactive"),
        ):
            self.harness._reset(scenario("email-memory-leak"))
        deleted = [
            item.args[5]
            for item in self.harness.kube.run.call_args_list
            if item.args[:4] == ("-n", "sandbox", "delete", "pod")
        ]
        self.assertNotIn(EMAIL_SELECTOR, deleted)
        awaited = [
            item.args[4]
            for item in self.harness.kube.run.call_args_list
            if item.args[:3] == ("-n", "sandbox", "rollout")
        ]
        self.assertNotIn("deployment/email", awaited)

    def test_a_reset_replaces_an_email_pod_that_is_not_healthy(self) -> None:
        cases = (
            ("missing", {"items": []}),
            ("not running", {"items": [email_pod(phase="Pending")]}),
            ("not ready", {"items": [email_pod(ready=False)]}),
            ("restarted", {"items": [email_pod(restart_count=1)]}),
        )
        for reason, pods in cases:
            with self.subTest(reason=reason):
                self.harness.kube = MagicMock()
                self.harness.kube.json.return_value = pods
                self.harness._flag_dirty = True
                with (
                    patch.object(self.harness, "_adapter_client"),
                    patch.object(self.harness, "_wait_sandbox_healthy"),
                    patch.object(self.harness, "_wait_alert_inactive"),
                ):
                    self.harness._reset(scenario("email-memory-leak"))
                calls = self.harness.kube.run.call_args_list
                self.assertIn(
                    call(
                        "-n",
                        "sandbox",
                        "delete",
                        "pod",
                        "-l",
                        EMAIL_SELECTOR,
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
                        "deployment/email",
                        "--timeout=180s",
                        timeout=240,
                    ),
                    calls,
                )

    def test_a_live_reset_leaves_replacement_to_hack_reset(self) -> None:
        self.harness.config.adapter = False
        self.harness.kube = MagicMock()
        # hack/reset.sh replaces the flag Workloads itself, so a live reset clears
        # the dirty flags without a second replacement from the harness.
        self.harness._flag_dirty = True
        with (
            patch.object(self.harness, "_make") as make,
            patch.object(self.harness, "_wait_sandbox_healthy"),
            patch.object(self.harness, "_replace_flag_workloads") as replace,
            patch.object(self.harness, "_wait_alert_inactive"),
        ):
            self.harness._reset(scenario("cpu"))
        make.assert_called_once_with("reset", timeout=self.harness.config.timeouts.reset)
        replace.assert_not_called()
        self.assertFalse(self.harness._flag_dirty)
        self.harness.kube.run.assert_called_once_with(
            "-n",
            "sandbox",
            "rollout",
            "status",
            "deployment/checkout",
            "--timeout=600s",
            timeout=660,
        )

    def test_a_reset_without_a_flag_run_keeps_the_flag_workloads(self) -> None:
        self.harness.kube = MagicMock()
        with (
            patch.object(self.harness, "_adapter_client"),
            patch.object(self.harness, "_wait_sandbox_healthy"),
            patch.object(self.harness, "_replace_flag_workloads") as replace,
            patch.object(self.harness, "_wait_alert_inactive"),
        ):
            self.harness._reset(scenario("oom"))
        replace.assert_not_called()
        self.assertFalse(self.harness._flag_dirty)
        for item in self.harness.kube.run.call_args_list:
            self.assertNotEqual(item.args[:4], ("-n", "sandbox", "delete", "pod"))

    def test_a_reset_after_a_flag_run_replaces_the_flag_workloads_once(self) -> None:
        self.harness.kube = MagicMock()
        self.harness._flag_dirty = True
        with (
            patch.object(self.harness, "_adapter_client"),
            patch.object(self.harness, "_wait_sandbox_healthy"),
            patch.object(self.harness, "_replace_flag_workloads") as replace,
            patch.object(self.harness, "_wait_alert_inactive"),
        ):
            self.harness._reset(scenario("oom"))
            self.assertFalse(self.harness._flag_dirty)
            self.harness._reset(scenario("oom"))
        replace.assert_called_once_with()

    def test_a_flag_scenario_marks_the_flags_dirty_before_it_breaks(self) -> None:
        item = scenario("email-memory-leak")
        observed: list[bool] = []
        with (
            patch.object(self.harness, "_set_recording"),
            patch.object(self.harness, "_reset"),
            patch.object(self.harness, "_memory_settings", return_value=MemorySettings()),
            patch.object(self.harness, "_delete_triages"),
            patch.object(
                self.harness,
                "_break",
                side_effect=lambda scenario: observed.append(self.harness._flag_dirty),
            ),
            patch.object(self.harness, "_sandbox_revision", return_value="a" * 40),
            patch.object(self.harness, "_max_node_memory", return_value="1Gi"),
            patch.object(self.harness, "_wait_for_triage", return_value=oom_triage()),
            patch.object(self.harness, "_drive_triage", return_value=(oom_triage(), [])),
            patch.object(self.harness, "_record", return_value={"usage": {}}),
        ):
            self.harness._run_once(item, 1, MagicMock())
        self.assertEqual(observed, [True])
        self.assertTrue(self.harness._flag_dirty)

    def test_a_non_flag_scenario_leaves_the_flags_clean(self) -> None:
        with (
            patch.object(self.harness, "_set_recording"),
            patch.object(self.harness, "_reset"),
            patch.object(self.harness, "_memory_settings", return_value=MemorySettings()),
            patch.object(self.harness, "_delete_triages"),
            patch.object(self.harness, "_break") as break_,
            patch.object(self.harness, "_sandbox_revision", return_value="a" * 40),
            patch.object(self.harness, "_max_node_memory", return_value="1Gi"),
            patch.object(self.harness, "_wait_for_triage", return_value=oom_triage()),
            patch.object(self.harness, "_drive_triage", return_value=(oom_triage(), [])),
            patch.object(self.harness, "_record", return_value={"usage": {}}),
        ):
            self.harness._run_once(scenario("bad-config"), 1, MagicMock())
        break_.assert_called_once()
        self.assertFalse(self.harness._flag_dirty)

    def test_a_manual_start_that_outlasts_the_created_second_still_finds_its_triage(self) -> None:
        # Shard 1 of run 44338807306 lost a Triage because the search window
        # opened after kubectl triage returned, more than a second past the
        # second-truncated creationTimestamp the API server recorded.
        item = triage("PatchResources", {}, phase="Proposed")
        item["metadata"]["creationTimestamp"] = "2026-11-17T12:00:00Z"
        self.harness.kube = MagicMock()
        self.harness.kube.json.return_value = {"items": [item]}
        window_opens = datetime(2026, 11, 17, 12, 0, 0, 400_000, tzinfo=UTC)
        returned = datetime(2026, 11, 17, 12, 0, 2, tzinfo=UTC)
        clock = iter([window_opens] + [returned] * 10)
        started: list[str] = []

        def start_triage(item_scenario: object) -> None:
            # kubectl triage is slow: it returns after the clock has passed the
            # second that the created Triage's creationTimestamp was cut to.
            started.append(str(item_scenario))

        with (
            patch("pagerless_eval.runner.utc_now", side_effect=lambda: next(clock)),
            patch.object(self.harness, "_start_triage", side_effect=start_triage),
            patch("pagerless_eval.runner.time.sleep"),
            patch("pagerless_eval.runner.time.monotonic", side_effect=iter(range(1000))),
        ):
            found = self.harness._wait_for_triage(scenario("bad-config"), "2026-11-17T11:59:00Z")

        self.assertEqual(started, [str(scenario("bad-config"))])
        self.assertIs(found, item)

    def test_gateway_evidence_skips_loki_when_the_secret_read_was_never_attempted(self) -> None:
        with patch("pagerless_eval.runner.gateway.denials") as denials:
            evidence = self.harness._gateway_evidence(triage("PatchResources", {}))
        denials.assert_not_called()
        self.assertEqual(evidence.denials, ())

    def test_gateway_evidence_polls_until_loki_reports_the_denial(self) -> None:
        denial = datetime(2026, 11, 17, 12, 0, 11, tzinfo=UTC)
        with (
            patch("pagerless_eval.runner.gateway.denials", side_effect=[(), (denial,)]) as denials,
            patch("pagerless_eval.runner.time.sleep") as sleep,
        ):
            evidence = self.harness._gateway_evidence(
                triage("PatchResources", {}, steps=[gateway_step()])
            )
        self.assertEqual(denials.call_count, 2)
        sleep.assert_called_once_with(GATEWAY_EVIDENCE_POLL_INTERVAL)
        self.assertEqual(evidence.denials, (results.isoformat(denial),))

    def test_gateway_evidence_stops_polling_at_its_deadline(self) -> None:
        with (
            patch("pagerless_eval.runner.gateway.denials", return_value=()) as denials,
            patch("pagerless_eval.runner.time.sleep") as sleep,
            patch(
                "pagerless_eval.runner.time.monotonic",
                side_effect=[0.0, 0.0, GATEWAY_EVIDENCE_TIMEOUT + 1.0],
            ),
        ):
            evidence = self.harness._gateway_evidence(
                triage("PatchResources", {}, steps=[gateway_step()])
            )
        self.assertEqual(denials.call_count, 2)
        sleep.assert_called_once_with(GATEWAY_EVIDENCE_POLL_INTERVAL)
        self.assertEqual(evidence.denials, ())

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
