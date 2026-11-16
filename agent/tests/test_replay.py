"""The Replay recording format: recording a run, reading the file, and playing it back against another run."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from pagerless_agent.bundle import ContextBundle
from pagerless_agent.identifiers import ReplayError
from pagerless_agent.model import ModelRequest, ModelResponse, Stage, ToolCall
from pagerless_agent.replay import FORMAT, Recording, RecordingProvider, ReplayProvider
from tests.fakes import ScriptedProvider, call, respond
from tests.support import RECORDED, REPLAYED, make_bundle

STAGES = [Stage.INVESTIGATE, Stage.INVESTIGATE, Stage.DIAGNOSE, Stage.PROPOSE]


def request(stage: Stage, bundle: ContextBundle) -> ModelRequest:
    return ModelRequest(stage=stage, system="s", messages=(), tools=(), bundle=bundle)


def live_responses() -> list[ModelResponse]:
    """What a live provider answered in the recorded run, with that run's own identifiers."""
    return [
        respond(
            call("toolu_01KXZ", "pods_log", name=RECORDED.pod, since="2026-11-16T10:20:15Z"),
            call("toolu_02QRS", "resources_get", name="checkout"),
            text=f"Checking {RECORDED.pod}, which belongs to {RECORDED.replicaset}.",
        ),
        respond(text="Done."),
        respond(
            structured={
                "diagnosis": f"{RECORDED.pod} was OOMKilled after commit {RECORDED.commits[0][:7]}.",
                "findings": [{"kind": "Commit", "source": RECORDED.commits[0], "summary": "limit lowered"}],
            }
        ),
        respond(
            structured={"action": "Rollback", "parameters": {"rollback": {"commit": RECORDED.commits[0]}}}
        ),
    ]


async def record(path: Path) -> Recording:
    bundle = make_bundle(RECORDED)
    recorder = RecordingProvider(ScriptedProvider(live_responses()), scenario="oom", model="claude-haiku-5-5")
    for stage in STAGES:
        await recorder.complete(request(stage, bundle))
    recording = recorder.recording()
    recording.save(path)
    return recording


async def test_a_recording_replays_against_another_run_with_that_runs_identifiers(tmp_path: Path) -> None:
    path = tmp_path / "oom.json"
    await record(path)

    replay = ReplayProvider(Recording.load(path))
    bundle = make_bundle(REPLAYED)
    first = await replay.complete(request(Stage.INVESTIGATE, bundle))
    second = await replay.complete(request(Stage.INVESTIGATE, bundle))
    diagnosis = await replay.complete(request(Stage.DIAGNOSE, bundle))
    proposal = await replay.complete(request(Stage.PROPOSE, bundle))

    assert first.text == f"Checking {REPLAYED.pod}, which belongs to {REPLAYED.replicaset}."
    assert first.tool_calls == (
        ToolCall(
            id="call_1", name="pods_log", arguments={"name": REPLAYED.pod, "since": "2026-12-02T03:55:06Z"}
        ),
        ToolCall(id="call_2", name="resources_get", arguments={"name": "checkout"}),
    )
    assert second.text == "Done."
    assert diagnosis.structured is not None
    assert (
        diagnosis.structured["diagnosis"]
        == f"{REPLAYED.pod} was OOMKilled after commit {REPLAYED.commits[0][:7]}."
    )
    assert diagnosis.structured["findings"][0]["source"] == REPLAYED.commits[0]
    assert proposal.structured == {
        "action": "Rollback",
        "parameters": {"rollback": {"commit": REPLAYED.commits[0]}},
    }
    assert replay.remaining == 0


async def test_the_file_holds_no_identifier_of_the_recorded_run(tmp_path: Path) -> None:
    path = tmp_path / "oom.json"
    await record(path)
    text = path.read_text(encoding="utf-8")

    for volatile in (RECORDED.pod, RECORDED.replicaset, *RECORDED.commits, "2026-11-16", "toolu_"):
        assert volatile not in text
    assert "{{pod:trigger}}" in text
    assert "{{commit:0}}" in text
    assert "{{ts:-600.25:T0:Z}}" in text


async def test_the_recording_is_stable_so_that_a_changed_format_is_noticed(tmp_path: Path) -> None:
    path = tmp_path / "oom.json"
    await record(path)
    golden = Path(__file__).parent / "fixtures" / "replay-format-v1.json"
    assert path.read_text(encoding="utf-8") == golden.read_text(encoding="utf-8")
    assert Recording.load(golden).version == 1


async def test_recording_an_identifier_that_nothing_can_resolve_fails_when_recording(tmp_path: Path) -> None:
    unknown_sha = "9" * 40
    responses = [respond(text=f"The culprit is commit {unknown_sha}.")]
    recorder = RecordingProvider(ScriptedProvider(responses), scenario="oom", model="m")
    await recorder.complete(request(Stage.INVESTIGATE, make_bundle(RECORDED)))
    with pytest.raises(ReplayError, match=unknown_sha):
        recorder.recording()


async def test_a_pod_of_the_workload_that_is_not_in_the_bundle_fails_when_recording() -> None:
    gone = "checkout-5f6d7c8b9-t8vwz"
    recorder = RecordingProvider(ScriptedProvider([respond(text=f"Also {gone}.")]), scenario="oom", model="m")
    await recorder.complete(request(Stage.INVESTIGATE, make_bundle(RECORDED)))
    with pytest.raises(ReplayError, match=gone):
        recorder.recording()


async def test_an_empty_recorder_has_nothing_to_save() -> None:
    with pytest.raises(ReplayError, match="nothing was recorded"):
        RecordingProvider(ScriptedProvider([]), scenario="oom", model="m").recording()


def document(**overrides: Any) -> dict[str, Any]:
    base: dict[str, Any] = {
        "format": FORMAT,
        "version": 1,
        "scenario": "oom",
        "model": "claude-haiku-5-5",
        "workload": "checkout",
        "turns": [
            {
                "stage": "investigate",
                "response": {
                    "text": "hello",
                    "tool_calls": [],
                    "structured": None,
                    "usage": {"input_tokens": 1, "output_tokens": 1},
                },
            }
        ],
    }
    base.update(overrides)
    return base


def write(tmp_path: Path, content: Any) -> Path:
    path = tmp_path / "recording.json"
    path.write_text(content if isinstance(content, str) else json.dumps(content), encoding="utf-8")
    return path


class TestLoading:
    def test_a_valid_document_loads(self, tmp_path: Path) -> None:
        assert Recording.load(write(tmp_path, document())).scenario == "oom"

    @pytest.mark.parametrize(
        ("content", "message"),
        [
            ("not json", "cannot read the recording"),
            ("[]", "not a Replay recording"),
            (document(format="something-else"), "not a Replay recording"),
            (document(version=2), "version 2"),
            (document(version="1"), "version '1'"),
            (document(extra="field"), "not a valid recording"),
            (document(turns="none"), "not a valid recording"),
            (document(turns=[{"stage": "dream", "response": {}}]), "not a valid recording"),
        ],
    )
    def test_a_document_that_is_not_a_version_1_recording_is_refused(
        self, tmp_path: Path, content: Any, message: str
    ) -> None:
        with pytest.raises(ReplayError, match=message):
            Recording.load(write(tmp_path, content))

    def test_a_missing_file_is_refused(self, tmp_path: Path) -> None:
        with pytest.raises(ReplayError, match="cannot read the recording"):
            Recording.load(tmp_path / "absent.json")

    @pytest.mark.parametrize(
        ("text", "message"),
        [
            ("sha " + "a" * 40, "was not normalized"),
            ("pod checkout-5f6d7c8b9-t8vwz", "was not normalized"),
            ("at 2026-11-16T10:30:15Z", "was not normalized"),
            ("stray {{ braces", "malformed placeholder"),
            ("{{pod:later}}", "unknown placeholder"),
        ],
    )
    def test_a_hand_edited_recording_with_a_volatile_identifier_or_a_bad_placeholder_is_refused(
        self, tmp_path: Path, text: str, message: str
    ) -> None:
        content = document()
        content["turns"][0]["response"]["text"] = text
        with pytest.raises(ReplayError, match=message):
            Recording.load(write(tmp_path, content))

    def test_a_volatile_identifier_hidden_in_a_tool_argument_is_found(self, tmp_path: Path) -> None:
        content = document()
        content["turns"][0]["response"]["tool_calls"] = [
            {
                "id": "call_1",
                "name": "pods_log",
                "arguments": {"deep": [{"name": "checkout-5f6d7c8b9-t8vwz"}]},
            }
        ]
        with pytest.raises(ReplayError, match="was not normalized"):
            Recording.load(write(tmp_path, content))


class TestPlayback:
    def recording(self) -> Recording:
        return Recording.model_validate(
            document(
                turns=[
                    {
                        "stage": "investigate",
                        "response": {"text": "a", "usage": {"input_tokens": 1, "output_tokens": 1}},
                    },
                    {
                        "stage": "diagnose",
                        "response": {"text": "b", "usage": {"input_tokens": 1, "output_tokens": 1}},
                    },
                ]
            )
        )

    async def test_responses_play_in_sequence_and_the_recording_can_run_out(self) -> None:
        replay = ReplayProvider(self.recording())
        bundle = make_bundle(REPLAYED)
        assert (await replay.complete(request(Stage.INVESTIGATE, bundle))).text == "a"
        assert (await replay.complete(request(Stage.DIAGNOSE, bundle))).text == "b"
        with pytest.raises(ReplayError, match="exhausted after 2 responses"):
            await replay.complete(request(Stage.PROPOSE, bundle))

    async def test_an_agent_that_asks_for_another_stage_has_diverged_from_the_recording(self) -> None:
        replay = ReplayProvider(self.recording())
        with pytest.raises(ReplayError, match="response 1 was recorded for the investigate stage"):
            await replay.complete(request(Stage.DIAGNOSE, make_bundle(REPLAYED)))

    async def test_a_placeholder_the_current_bundle_cannot_resolve_stops_the_run(self) -> None:
        content = document()
        content["turns"][0]["response"]["text"] = "see {{pod:7}}"
        replay = ReplayProvider(Recording.model_validate(content))
        with pytest.raises(ReplayError, match="cannot resolve index 7"):
            await replay.complete(request(Stage.INVESTIGATE, make_bundle(REPLAYED)))
