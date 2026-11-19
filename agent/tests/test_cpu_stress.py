"""Scenario 9 at the agent Replay seam: an experiment in the bundle yields NoAction."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path

import httpx

from pagerless_agent.agent import Agent, Outcome
from pagerless_agent.bundle import ContextBundle, Experiment, PodLogs, PodState, WorkloadRef
from pagerless_agent.model import ModelProvider, Stage
from pagerless_agent.replay import Recording, ReplayProvider
from pagerless_agent.server import ServerClient
from pagerless_agent.settings import Target
from tests.fakes import FixedSource, RecordedTools, Spy, StandInServer

FIXTURE = Path(__file__).parent / "fixtures" / "synthetic-cpu-stress.json"
EXPERIMENT = Experiment(
    name="cpu-stress-payment",
    kind="StressChaos",
    target="Deployment sandbox/payment",
    duration="30m",
)


def payment_bundle(pod: str = "payment-7d9f8b6c5-x2k4q") -> ContextBundle:
    """Return the Context Bundle of the CPU-stress Scenario."""
    at = datetime(2026, 11, 17, 9, 0, tzinfo=UTC)
    return ContextBundle(
        workload=WorkloadRef(kind="Deployment", namespace="sandbox", name="payment"),
        gathered_at=at,
        triggering_pod=pod,
        workload_status="replicas 1, ready 1, available 1",
        pods=(
            PodState(name=pod, phase="Running", ready=True, restarts=0, created_at=at - timedelta(hours=1)),
        ),
        events=(),
        logs=(PodLogs(pod=pod, container="payment", lines="charging card"),),
        argocd_status="Synced, Healthy",
        experiments=(EXPERIMENT,),
        commits=(),
    )


async def no_sleep(_: float) -> None:
    return None


async def run(
    provider: ModelProvider, tmp_path: Path, bundle: ContextBundle
) -> tuple[Outcome, StandInServer, Spy]:
    """Run the agent against one bundle, as a Job does, and return what it did."""
    server = StandInServer()
    http = httpx.AsyncClient(transport=server.transport())
    tmp_path.mkdir(parents=True, exist_ok=True)
    token_file = tmp_path / "token"
    token_file.write_text("token-1")
    client = ServerClient("http://pagerless-server", server.page, token_file, http=http, sleep=no_sleep)
    model = Spy(provider)
    outcome = await Agent(
        target=Target(server.page, "Deployment", "sandbox", "payment", bundle.triggering_pod),
        provider=model,
        tools=RecordedTools([], {}),
        source=FixedSource(bundle),
        server=client,
        nonce="test-nonce",
    ).run()
    await client.aclose()
    return outcome, server, model


async def test_an_active_experiment_replays_to_no_action_that_names_it(tmp_path: Path) -> None:
    recording = Recording.load(FIXTURE)

    outcome, server, model = await run(ReplayProvider(recording), tmp_path, payment_bundle())

    assert outcome == Outcome("reported", "The report was accepted.", phase="Proposed")
    assert server.reports[0]["proposal"] == {"action": "NoAction"}
    assert "cpu-stress-payment" in server.reports[0]["diagnosis"]
    assert "Chaos Mesh experiment" in server.reports[0]["diagnosis"]
    assert [report["findings"][0]["kind"] for report in server.reports] == ["Event"]
    # The model reads the experiment in the bundle, and the Finding cites that section.
    first = model.requests[0].messages[0].content
    assert "## Active chaos experiments" in first
    assert "cpu-stress-payment (StressChaos) targets Deployment sandbox/payment; duration 30m" in first
    assert (
        "Finding",
        None,
        "Event: cpu-stress-payment (StressChaos) targets Deployment sandbox/payment; duration 30m",
    ) in [(event["kind"], event.get("tool"), event["summary"]) for event in server.events]
    assert [request.stage for request in model.requests] == [
        Stage.INVESTIGATE,
        Stage.DIAGNOSE,
        Stage.PROPOSE,
    ]


async def test_the_same_recording_replays_for_a_new_run_of_the_scenario(tmp_path: Path) -> None:
    recording = Recording.load(FIXTURE)

    _, first, _ = await run(ReplayProvider(recording), tmp_path / "first", payment_bundle())
    _, second, _ = await run(
        ReplayProvider(Recording.load(FIXTURE)),
        tmp_path / "second",
        payment_bundle("payment-5f6d7c8b9-t8vwz"),
    )

    assert first.reports == second.reports


def test_a_bundle_without_experiments_states_that_none_are_active() -> None:
    rendered = payment_bundle().model_copy(update={"experiments": ()}).render()

    assert "## Active chaos experiments\n- none\n" in rendered
