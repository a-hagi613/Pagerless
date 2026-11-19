"""The Narrative Job: one capped model call that writes the account of a finished Page.

The seam is the Job's own entry points (`run_narrative` and `main`) against the
agent API and the Replay recording in the repository. No test makes a model call.
"""

from __future__ import annotations

import json
from pathlib import Path

import httpx
import pytest

from pagerless_agent.contract import MAX_NARRATIVE_LENGTH
from pagerless_agent.identifiers import ReplayError
from pagerless_agent.model import ModelRequest, Stage
from pagerless_agent.narrative import NARRATIVE_MAX_OUTPUT_TOKENS, main, run_narrative
from pagerless_agent.replay import Recording, ReplayProvider
from pagerless_agent.server import ServerRefusedError
from pagerless_agent.settings import ConfigError, NarrativeSettings
from tests.fakes import ScriptedProvider, Spy, StandInServer, respond

# The recording that the agent image ships at /app/recordings/narrative.json.
RECORDING = Path(__file__).resolve().parent / "fixtures" / "narrative.json"


def narrative_env(tmp_path: Path, *, recording: Path = RECORDING) -> dict[str, str]:
    token = tmp_path / "token"
    token.write_text("report-token", encoding="utf-8")
    return {
        "PAGERLESS_SERVER_URL": "http://pagerless-server",
        "PAGERLESS_TOKEN_FILE": str(token),
        "PAGERLESS_PAGE": "checkout-abc12",
        "PAGERLESS_NARRATIVE_REPLAY_FILE": str(recording),
    }


async def test_the_job_writes_the_recorded_narrative_without_a_paid_model_call(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    server = StandInServer(token="report-token", page="checkout-abc12")
    http = httpx.AsyncClient(transport=server.transport())
    settings = NarrativeSettings.from_env(narrative_env(tmp_path))
    assert settings.provider == "replay"

    outcome = await run_narrative(settings, server_http=http)

    assert outcome.kind == "written" and outcome.exit_code == 0
    assert server.narratives == [{"narrative": Recording.load(RECORDING).turns[0].response.text}]
    assert [request.method for request in server.requests] == ["GET", "POST"]
    assert http.is_closed


async def test_the_narrative_is_one_call_capped_at_two_thousand_output_tokens(tmp_path: Path) -> None:
    server = StandInServer(token="report-token", page="checkout-abc12")
    provider = Spy(ScriptedProvider([respond(text="The checkout Deployment recovered.\n")]))

    outcome = await run_narrative(
        NarrativeSettings.from_env(narrative_env(tmp_path)),
        server_http=httpx.AsyncClient(transport=server.transport()),
        provider=provider,
    )

    assert outcome.kind == "written"
    assert len(provider.requests) == 1
    request = provider.requests[0]
    assert request.stage is Stage.NARRATIVE
    assert request.max_output_tokens == NARRATIVE_MAX_OUTPUT_TOKENS == 2000
    assert request.output_schema is None
    assert request.tools == ()
    assert request.bundle is None
    assert "Narrative" in request.system
    assert server.narratives == [{"narrative": "The checkout Deployment recovered."}]


async def test_a_live_narrative_is_recorded_only_when_it_names_no_volatile_identifier(
    tmp_path: Path,
) -> None:
    destination = tmp_path / "narrative.json"
    env = {
        **narrative_env(tmp_path),
        "PAGERLESS_MODEL_PROVIDER": "anthropic",
        "PAGERLESS_RECORDING_FILE": str(destination),
    }
    settings = NarrativeSettings.from_env(env)

    naming_a_pod = ScriptedProvider([respond(text="The pod checkout-7d9f8b6c5-x2k4q was restarted.")])
    server = StandInServer(token="report-token", page="checkout-abc12")
    with pytest.raises(ReplayError, match="checkout-7d9f8b6c5-x2k4q"):
        await run_narrative(
            settings,
            server_http=httpx.AsyncClient(transport=server.transport()),
            provider=naming_a_pod,
        )
    assert not destination.exists()

    provider = ScriptedProvider([respond(text="The checkout Deployment recovered.")])
    server = StandInServer(token="report-token", page="checkout-abc12")
    outcome = await run_narrative(
        settings,
        server_http=httpx.AsyncClient(transport=server.transport()),
        provider=provider,
    )
    assert outcome.kind == "written"
    recording = Recording.load(destination)
    assert recording.scenario == "narrative" and recording.workload == "checkout"
    assert [turn.stage for turn in recording.turns] == [Stage.NARRATIVE]
    assert recording.turns[0].response.text == "The checkout Deployment recovered."


async def test_replay_refuses_a_placeholder_that_the_narrative_cannot_resolve() -> None:
    content = json.loads(RECORDING.read_text(encoding="utf-8"))
    content["turns"][0]["response"]["text"] = "The pod {{pod:0}} recovered."
    replay = ReplayProvider(Recording.model_validate(content))

    with pytest.raises(ReplayError, match="no Context Bundle"):
        await replay.complete(ModelRequest(stage=Stage.NARRATIVE, system="s", messages=(), tools=()))


async def test_a_literal_brace_in_a_narrative_round_trips_for_replay(tmp_path: Path) -> None:
    destination = tmp_path / "narrative.json"
    env = {
        **narrative_env(tmp_path),
        "PAGERLESS_MODEL_PROVIDER": "anthropic",
        "PAGERLESS_RECORDING_FILE": str(destination),
    }
    provider = ScriptedProvider([respond(text="The log said {{ and the Page recovered.")])

    outcome = await run_narrative(
        NarrativeSettings.from_env(env),
        server_http=httpx.AsyncClient(
            transport=StandInServer(token="report-token", page="checkout-abc12").transport()
        ),
        provider=provider,
    )

    assert outcome.kind == "written"
    replay = ReplayProvider(Recording.load(destination))
    replayed = await replay.complete(ModelRequest(stage=Stage.NARRATIVE, system="s", messages=(), tools=()))
    assert replayed.text == "The log said {{ and the Page recovered."


def test_the_narrative_runs_from_the_repository_recording_by_default(tmp_path: Path) -> None:
    env = narrative_env(tmp_path)
    del env["PAGERLESS_NARRATIVE_REPLAY_FILE"]

    settings = NarrativeSettings.from_env(env)

    assert settings.provider == "replay"
    assert settings.replay_file == Path("/app/recordings/narrative.json")
    assert settings.recording_file is None
    recording = Recording.load(RECORDING)
    assert recording.scenario == "narrative"
    assert [turn.stage for turn in recording.turns] == [Stage.NARRATIVE]
    assert recording.workload == "checkout"
    assert len(recording.turns[0].response.text) < MAX_NARRATIVE_LENGTH


async def test_a_refused_narrative_is_posted_once_and_stops_the_job(tmp_path: Path) -> None:
    server = StandInServer(token="report-token", page="checkout-abc12")
    server.narrative_answers = [httpx.Response(409, json={"message": "the Page is not Verifying"})]

    with pytest.raises(ServerRefusedError):
        await run_narrative(
            NarrativeSettings.from_env(narrative_env(tmp_path)),
            server_http=httpx.AsyncClient(transport=server.transport()),
        )
    assert len(server.narratives) == 1


def test_the_narrative_task_refuses_incomplete_or_unknown_configuration() -> None:
    with pytest.raises(ConfigError, match="PAGERLESS_PAGE"):
        NarrativeSettings.from_env({"PAGERLESS_SERVER_URL": "http://server"})
    with pytest.raises(ConfigError, match="cannot produce a live recording"):
        NarrativeSettings.from_env(
            {
                "PAGERLESS_SERVER_URL": "http://server",
                "PAGERLESS_TOKEN_FILE": "/token",
                "PAGERLESS_PAGE": "checkout-abc12",
                "PAGERLESS_RECORDING_FILE": "/recordings/narrative.json",
            }
        )
    assert main({"PAGERLESS_TASK": "narrative"}) == 1
    assert main({"PAGERLESS_TASK": "something-else"}) == 1
