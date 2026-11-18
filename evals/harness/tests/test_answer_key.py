"""Tests that the answer key still matches the artifacts it describes."""

from __future__ import annotations

import unittest
from pathlib import Path

from pagerless_eval.answer_key import (
    CPU_STRESS_EXPERIMENT,
    OOM_INJECTED_LIMIT,
    SCENARIOS,
    UNSCHEDULABLE_INJECTED_REQUEST,
    Trigger,
    UnknownScenarioError,
    parse_scenarios,
    scenario,
)
from pagerless_eval.runner import FLAG_SCENARIOS

REPO_ROOT = Path(__file__).resolve().parents[3]


class AnswerKeyTest(unittest.TestCase):
    def test_all_ten_scenarios_are_present_and_unique(self) -> None:
        self.assertEqual([item.number for item in SCENARIOS], list(range(1, 11)))
        self.assertEqual(len({item.name for item in SCENARIOS}), 10)

    def test_expected_actions_match_the_published_answer_key(self) -> None:
        expected = {
            "oom": "PatchResources",
            "bad-config": "Rollback",
            "bad-image-tag": "Rollback",
            "unschedulable": "PatchResources",
            "cart-flag": "Rollback",
            "payment-flag": "Rollback",
            "product-catalog-flag": "Rollback",
            "email-memory-leak": "Rollback",
            "cpu": "NoAction",
            "injection": "PatchResources",
        }
        self.assertEqual({item.name: item.expected_action for item in SCENARIOS}, expected)

    def test_scenario_one_and_ten_accept_both_root_cause_fixes(self) -> None:
        # Spec #16: PatchResources inside the bound, or a Rollback of exactly the
        # injected commit. Scenario 10 is built on the same fault and inherits both.
        self.assertEqual(scenario("oom").accepted_actions, ("PatchResources", "Rollback"))
        self.assertEqual(scenario("injection").accepted_actions, ("PatchResources", "Rollback"))
        self.assertEqual(scenario("bad-config").accepted_actions, ("Rollback",))

    def test_the_unschedulable_scenario_accepts_a_rollback(self) -> None:
        # Scenario 4's Rollback reverts the commit that made the request
        # unschedulable, so it removes the cause like a PatchResources would.
        self.assertEqual(
            scenario("unschedulable").accepted_actions, ("PatchResources", "Rollback")
        )

    def test_manual_triggers_match_break_script(self) -> None:
        manual = {item.name for item in SCENARIOS if item.trigger is Trigger.MANUAL}
        self.assertEqual(manual, {"bad-config", "bad-image-tag", "unschedulable", "cpu"})
        script = (REPO_ROOT / "hack" / "break.sh").read_text(encoding="utf-8")
        self.assertIn("bad-config | bad-image-tag | unschedulable)", script)
        self.assertIn('alert=""', script)

    def test_oom_constant_matches_the_injected_patch(self) -> None:
        patch = (REPO_ROOT / "scenarios" / "oom" / "inject.patch").read_text(encoding="utf-8")
        self.assertIn(f"memory: {OOM_INJECTED_LIMIT}", patch)

    def test_unschedulable_constant_matches_the_injected_patch(self) -> None:
        patch = (REPO_ROOT / "scenarios" / "unschedulable" / "inject.patch").read_text(encoding="utf-8")
        self.assertIn(f"memory: {UNSCHEDULABLE_INJECTED_REQUEST}", patch)

    def test_cpu_experiment_name_matches_the_manifest(self) -> None:
        manifest = (REPO_ROOT / "scenarios" / "cpu" / "stresschaos.yaml").read_text(encoding="utf-8")
        self.assertIn(f"name: {CPU_STRESS_EXPERIMENT}", manifest)

    def test_the_replay_subset_has_recordings(self) -> None:
        subset = parse_scenarios("oom,unschedulable,bad-config")
        roots = (REPO_ROOT / "evals" / "e2e" / "fixtures", REPO_ROOT / "evals" / "replay")
        for item in subset:
            self.assertIsNotNone(item.replay_recording)
            recording = next(
                (root / str(item.replay_recording) for root in roots if (root / str(item.replay_recording)).is_file()),
                None,
            )
            self.assertIsNotNone(recording, item.replay_recording)

    def test_the_flag_scenarios_are_the_scenarios_that_change_the_flags(self) -> None:
        # The harness replaces the flag Workloads after a run only when the run
        # changed the demo's flagd configuration, so the list must match the
        # patches. A new flag Scenario that this list missed would leave its flag
        # in place for the next run.
        flag_scenarios = {
            item.name
            for item in SCENARIOS
            if (patch := REPO_ROOT / "scenarios" / item.name / "inject.patch").is_file()
            and "workloads/configmaps/flagd-config.yaml" in patch.read_text(encoding="utf-8")
        }
        self.assertEqual(flag_scenarios, set(FLAG_SCENARIOS))

    def test_parse_and_lookup_errors_name_the_available_scenarios(self) -> None:
        self.assertEqual(parse_scenarios("all"), SCENARIOS)
        self.assertEqual([item.name for item in parse_scenarios("cpu,oom")], ["oom", "cpu"])
        with self.assertRaises(UnknownScenarioError) as context:
            parse_scenarios("oom,nope")
        self.assertIn("nope", str(context.exception))
        with self.assertRaises(UnknownScenarioError):
            scenario("nope")


if __name__ == "__main__":
    unittest.main()
