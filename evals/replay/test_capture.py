"""Capture helper checks use temporary synthetic data and never produce the live artifact."""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
from typing import Any

import pytest

from pagerless_agent.identifiers import ReplayError

SPEC = importlib.util.spec_from_file_location("replay_capture", Path(__file__).with_name("capture.py"))
assert SPEC and SPEC.loader
capture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(capture)


@pytest.mark.parametrize("value", ["test-secret-key", "'test-secret-key'", '"test-secret-key" # ignored'])
def test_the_ignored_configuration_is_read_without_shell_execution(tmp_path: Path, value: str) -> None:
    path = tmp_path / ".env"
    path.write_text(f"ANTHROPIC_API_KEY={value}\n", encoding="utf-8")
    assert capture.read_api_key(path) == "test-secret-key"


def test_an_empty_key_is_refused_without_echoing_configuration(tmp_path: Path) -> None:
    path = tmp_path / ".env"
    path.write_text("ANTHROPIC_API_KEY=''\n", encoding="utf-8")
    with pytest.raises(RuntimeError, match="no nonempty ANTHROPIC_API_KEY"):
        capture.read_api_key(path)


def test_credentials_are_sent_on_stdin_never_as_command_arguments_or_error_details(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    seen: list[tuple[list[str], bytes]] = []

    def run(arguments: list[str], *, input: bytes, **_: Any) -> Any:
        seen.append((arguments, input))
        return type("Result", (), {"returncode": 1, "stdout": b"", "stderr": b"test-secret-key"})()

    monkeypatch.setattr(capture.subprocess, "run", run)
    with pytest.raises(RuntimeError) as error:
        capture.Cluster("kind-pagerless").apply(
            {
                "apiVersion": "v1",
                "kind": "Secret",
                "stringData": {"ANTHROPIC_API_KEY": "test-secret-key"},
            }
        )
    assert "test-secret-key" not in str(error.value)
    assert "test-secret-key" not in " ".join(seen[0][0])
    assert b"test-secret-key" in seen[0][1]


def test_a_flat_proposal_is_validated_the_way_the_agent_validates_it(tmp_path: Path) -> None:
    artifact = tmp_path / "live.json"
    artifact.write_text(
        json.dumps(
            {
                "format": "pagerless.dev/replay",
                "version": 1,
                "scenario": "oom",
                "model": capture.ANTHROPIC_MODEL,
                "workload": "checkout",
                "turns": [
                    {"stage": "investigate", "response": {"usage": {"input_tokens": 10, "output_tokens": 5}}},
                    {
                        "stage": "diagnose",
                        "response": {
                            "structured": {
                                "diagnosis": "The memory limit is below the allocation.",
                                "findings": [
                                    {
                                        "kind": "Log",
                                        "source": "k8s__resources_get apps/v1 Deployment sandbox/checkout",
                                        "summary": "The limit is 32Mi.",
                                    }
                                ],
                            },
                            "usage": {"input_tokens": 10, "output_tokens": 5},
                        },
                    },
                    {
                        "stage": "propose",
                        "response": {
                            "structured": {
                                "action": "PatchResources",
                                "parameters": {"container": "checkout", "limits": {"memory": "128Mi"}},
                            },
                            "usage": {"input_tokens": 10, "output_tokens": 5},
                        },
                    },
                ],
            }
        ),
        encoding="utf-8",
    )
    recording = capture.validate_recording(artifact)
    assert len(recording.turns) == 3


def oom_document(proposal: dict[str, Any], diagnosis: dict[str, Any] | None = None) -> dict[str, Any]:
    """Build one live-shaped Scenario 1 recording with the given Proposal and optional Diagnosis."""
    default_diagnosis: dict[str, Any] = {
        "diagnosis": "The memory limit is below the allocation.",
        "findings": [
            {
                "kind": "Log",
                "source": "k8s__resources_get apps/v1 Deployment sandbox/checkout",
                "summary": "The limit is 32Mi.",
            }
        ],
    }
    return {
        "format": "pagerless.dev/replay",
        "version": 1,
        "scenario": "oom",
        "model": capture.ANTHROPIC_MODEL,
        "workload": "checkout",
        "turns": [
            {"stage": "investigate", "response": {"usage": {"input_tokens": 10, "output_tokens": 5}}},
            {
                "stage": "diagnose",
                "response": {
                    "structured": diagnosis if diagnosis is not None else default_diagnosis,
                    "usage": {"input_tokens": 10, "output_tokens": 5},
                },
            },
            {
                "stage": "propose",
                "response": {"structured": proposal, "usage": {"input_tokens": 10, "output_tokens": 5}},
            },
        ],
    }


def test_either_root_cause_fix_can_be_published_for_scenario_one(tmp_path: Path) -> None:
    # Spec #16 accepts a PatchResources inside the bound and a Rollback of exactly
    # the injected commit, so a genuine recording of either Action is publishable.
    artifact = tmp_path / "live.json"
    for proposal in (
        {
            "action": "PatchResources",
            "parameters": {"container": "memory-holder", "limits": {"memory": "64Mi"}},
        },
        {"action": "Rollback", "parameters": {"rollback": {"commit": "9eff68b"}}},
    ):
        artifact.write_text(json.dumps(oom_document(proposal)), encoding="utf-8")
        recording = capture.validate_recording(artifact)
        assert recording.turns[-1].response.structured["action"] == proposal["action"]

    artifact.write_text(
        json.dumps(oom_document({"action": "Scale", "parameters": {"scale": {"replicas": 0}}})),
        encoding="utf-8",
    )
    with pytest.raises(RuntimeError, match="PatchResources or a Rollback"):
        capture.validate_recording(artifact)


def test_synthetic_or_credential_containing_artifacts_cannot_be_promoted(
    tmp_path: Path,
) -> None:
    artifact = tmp_path / "candidate.json"
    source = Path(__file__).parents[1] / "e2e" / "fixtures" / "synthetic-oom.json"
    artifact.write_bytes(source.read_bytes())
    with pytest.raises(RuntimeError, match="not a live Haiku"):
        capture.validate_recording(artifact)
    artifact.write_bytes(b'{"response":"test-secret-key"}')
    with pytest.raises(RuntimeError, match="contains a credential"):
        capture.validate_recording(artifact, ("test-secret-key",))


def narrative_document(**overrides: Any) -> dict[str, Any]:
    document: dict[str, Any] = {
        "format": "pagerless.dev/replay",
        "version": 1,
        "scenario": "narrative",
        "model": capture.ANTHROPIC_MODEL,
        "workload": "checkout",
        "turns": [
            {
                "stage": "narrative",
                "response": {
                    "text": "The checkout Deployment recovered.",
                    "usage": {"input_tokens": 10, "output_tokens": 5},
                },
            }
        ],
    }
    document.update(overrides)
    return document


def test_a_narrative_recording_is_exactly_one_call(tmp_path: Path) -> None:
    artifact = tmp_path / "narrative.json"
    artifact.write_text(json.dumps(narrative_document()), encoding="utf-8")
    assert len(capture.validate_narrative_recording(artifact).turns) == 1

    for turns, message in (
        (narrative_document()["turns"] * 2, "exactly one call"),
        (
            [{"stage": "investigate", "response": {"usage": {"input_tokens": 1, "output_tokens": 1}}}],
            "exactly one call",
        ),
        (
            [
                {
                    "stage": "narrative",
                    "response": {
                        "text": " ",
                        "usage": {"input_tokens": 1, "output_tokens": 1},
                    },
                }
            ],
            "is empty",
        ),
    ):
        artifact.write_text(json.dumps(narrative_document(turns=turns)), encoding="utf-8")
        with pytest.raises(RuntimeError, match=message):
            capture.validate_narrative_recording(artifact)


def test_a_narrative_recording_of_another_scenario_cannot_be_promoted(tmp_path: Path) -> None:
    artifact = tmp_path / "narrative.json"
    artifact.write_text(json.dumps(narrative_document(scenario="oom")), encoding="utf-8")
    with pytest.raises(RuntimeError, match="not a live Haiku Narrative"):
        capture.validate_narrative_recording(artifact)
    artifact.write_text(json.dumps(narrative_document()), encoding="utf-8")
    with pytest.raises(RuntimeError, match="contains a credential"):
        capture.validate_narrative_recording(artifact, ("The checkout",))


def test_the_repository_narrative_recording_has_the_shape_the_job_plays() -> None:
    recording = capture.Recording.load(Path(__file__).with_name("narrative.json"))
    assert (recording.scenario, recording.workload) == ("narrative", "checkout")
    assert [turn.stage for turn in recording.turns] == [capture.Stage.NARRATIVE]
    assert recording.turns[0].response.text.strip()


@pytest.mark.parametrize(
    "proposal",
    [
        {"action": "Rollback", "parameters": {"commit": "{{commit:0}}"}},
        {"action": "Rollback", "parameters": {"commit": "{{commit:0:10}}"}},
        {"action": "Rollback", "parameters": {"rollback": {"commit": "{{commit:0}}"}}},
    ],
)
def test_normalized_rollback_proposals_pass_capture_validation(
    tmp_path: Path, proposal: dict[str, Any]
) -> None:
    artifact = tmp_path / "live.json"
    artifact.write_text(json.dumps(oom_document(proposal)), encoding="utf-8")
    recording = capture.validate_recording(artifact)
    assert recording.turns[-1].response.structured == proposal


def test_normalized_diagnosis_placeholders_pass_capture_validation(tmp_path: Path) -> None:
    artifact = tmp_path / "live.json"
    diagnosis = {
        "diagnosis": (
            "Pod {{pod:trigger}} (matching {{pod:0}} in ReplicaSet {{replicaset:0}}) "
            "was OOMKilled after commit {{commit:0:8}} at {{ts:-60:T0:Z}}."
        ),
        "findings": [
            {
                "kind": "Commit",
                "source": "github__list_commits {{commit:0:8}}",
                "summary": "Commit {{commit:0:8}} at {{ts:-60:T0:Z}} lowered the limit.",
            }
        ],
    }
    proposal = {"action": "Rollback", "parameters": {"commit": "{{commit:0}}"}}
    artifact.write_text(json.dumps(oom_document(proposal, diagnosis=diagnosis)), encoding="utf-8")
    recording = capture.validate_recording(artifact)
    assert recording.turns[1].response.structured == diagnosis


def test_out_of_range_placeholder_is_refused_by_capture_validation(tmp_path: Path) -> None:
    artifact = tmp_path / "live.json"
    proposal = {"action": "Rollback", "parameters": {"commit": "{{commit:99}}"}}
    artifact.write_text(json.dumps(oom_document(proposal)), encoding="utf-8")
    with pytest.raises(ReplayError, match="cannot resolve index 99"):
        capture.validate_recording(artifact)


def test_the_repository_oom_recording_passes_capture_validation() -> None:
    assert capture.validate_recording(Path(__file__).with_name("oom.json"))
