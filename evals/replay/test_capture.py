"""Capture helper checks use temporary synthetic data and never produce the live artifact."""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
from typing import Any

import pytest

SPEC = importlib.util.spec_from_file_location(
    "replay_capture", Path(__file__).with_name("capture.py")
)
assert SPEC and SPEC.loader
capture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(capture)


@pytest.mark.parametrize(
    "value", ["test-secret-key", "'test-secret-key'", '"test-secret-key" # ignored']
)
def test_the_ignored_configuration_is_read_without_shell_execution(
    tmp_path: Path, value: str
) -> None:
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
        return type(
            "Result", (), {"returncode": 1, "stdout": b"", "stderr": b"test-secret-key"}
        )()

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
