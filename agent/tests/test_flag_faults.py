"""Scenarios 5 to 8 at the agent Replay seam: a flag fault yields a Rollback.

The evidence set is the one the Context Bundle carries for a flag Scenario: the
Workload is ready and restart-free, its server spans fail, the newest Sandbox
commit changes the shared flagd configuration, and the Workload declares that
path in pagerless.dev/depends-on-paths, so a Rollback of the commit is the
Proposal the answer key expects (spec #16, ADR-0021).
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path

import httpx
import pytest

from pagerless_agent.agent import Agent, Outcome
from pagerless_agent.bundle import Commit, ContextBundle, PodLogs, PodState, WorkloadRef
from pagerless_agent.model import ModelProvider, Stage
from pagerless_agent.replay import Recording, ReplayProvider
from pagerless_agent.server import ServerClient
from pagerless_agent.settings import Target
from tests.fakes import FixedSource, RecordedTools, Spy, StandInServer

FIXTURE = Path(__file__).parent / "fixtures" / "synthetic-cart-flag.json"
# The commit that injects the Scenario, and the commit before it.
INJECTION = "3f2a9c1d8e7b6a5f4c3d2e1f0a9b8c7d6e5f4a3b"
KNOWN_GOOD = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"


def cart_bundle(
    pod: str = "cart-7d9f8b6c5-x2k4q", commits: tuple[str, str] = (INJECTION, KNOWN_GOOD)
) -> ContextBundle:
    """Return the Context Bundle that deterministic code gathers for a cart flag fault."""
    at = datetime(2026, 11, 17, 9, 0, tzinfo=UTC)
    return ContextBundle(
        workload=WorkloadRef(kind="Deployment", namespace="sandbox", name="cart"),
        gathered_at=at,
        triggering_pod=pod,
        workload_status="replicas 1, ready 1, available 1, updated 1",
        pods=(
            PodState(
                name=pod,
                phase="Running",
                ready=True,
                restarts=0,
                created_at=at - timedelta(hours=2),
            ),
        ),
        events=(),
        logs=(
            PodLogs(
                pod=pod,
                container="cart",
                lines=(
                    "info: CartService[0] EmptyCart called with user id 42\n"
                    "fail: CartService[0] FailedPrecondition: RpcException while emptying the cart: "
                    "failed to empty user cart during checkout\n"
                    "info: CartService[0] GetCart called with user id 42"
                ),
            ),
        ),
        argocd_status="Synced, Healthy",
        commits=(
            Commit(
                sha=commits[0],
                message=(
                    "Inject the cart-flag Scenario into cart\n\n"
                    "The Scenario patch sets the cartFailure flag to 100% in the shared flagd "
                    "configuration, so every empty-cart call fails and checkout cannot complete."
                ),
                author="pagerless",
                time=at - timedelta(minutes=12),
            ),
            Commit(
                sha=commits[1],
                message="Add the trimmed OpenTelemetry Demo",
                author="pagerless",
                time=at - timedelta(days=2),
            ),
        ),
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
    client = ServerClient("http://pagerless-server", server.triage, token_file, http=http, sleep=no_sleep)
    model = Spy(provider)
    outcome = await Agent(
        target=Target(server.triage, "Deployment", "sandbox", "cart", bundle.triggering_pod),
        provider=model,
        tools=RecordedTools([], {}),
        source=FixedSource(bundle),
        server=client,
        nonce="test-nonce",
    ).run()
    await client.aclose()
    return outcome, server, model


async def test_a_flag_fault_replays_to_a_rollback_of_the_injected_commit(tmp_path: Path) -> None:
    recording = Recording.load(FIXTURE)

    outcome, server, model = await run(ReplayProvider(recording), tmp_path, cart_bundle())

    assert outcome == Outcome("reported", "The report was accepted.", phase="Proposed")
    assert server.reports[0]["proposal"] == {
        "action": "Rollback",
        "parameters": {"rollback": {"commit": INJECTION}},
    }
    diagnosis = server.reports[0]["diagnosis"]
    assert "cartFailure" in diagnosis
    assert "flagd" in diagnosis
    assert [finding["kind"] for finding in server.reports[0]["findings"]] == ["Commit", "Log", "Metric"]

    # The model reads the evidence that makes the Rollback valid: the commit, and
    # the failure in the logs of a Workload that never restarted.
    first = model.requests[0].messages[0].content
    assert "Inject the cart-flag Scenario into cart" in first
    assert f"- {INJECTION} " in first
    assert "FailedPrecondition" in first
    assert "restarts 0" in first
    assert [request.stage for request in model.requests] == [
        Stage.INVESTIGATE,
        Stage.DIAGNOSE,
        Stage.PROPOSE,
    ]


async def test_the_same_recording_replays_the_new_runs_commit(tmp_path: Path) -> None:
    recording = Recording.load(FIXTURE)
    later_injection = "c0ffee11d00d22beef33cafe44babe55f00d6677"

    _, first, _ = await run(ReplayProvider(recording), tmp_path / "first", cart_bundle())
    _, second, _ = await run(
        ReplayProvider(Recording.load(FIXTURE)),
        tmp_path / "second",
        cart_bundle(
            pod="cart-5f6d7c8b9-t8vwz",
            commits=(later_injection, "0123456789abcdef0123456789abcdef01234567"),
        ),
    )

    # The recording names the commit by placeholder, so the second run rolls back its
    # own injection commit rather than the one recorded.
    assert first.reports[0]["proposal"]["parameters"]["rollback"]["commit"] == INJECTION
    assert second.reports[0]["proposal"]["parameters"]["rollback"]["commit"] == later_injection
    assert first.reports[0]["proposal"]["action"] == second.reports[0]["proposal"]["action"] == "Rollback"


@pytest.mark.parametrize(
    ("flag", "variant"),
    [("cartFailure", "100%"), ("paymentFailure", "100%"), ("productCatalogFailure", "on")],
)
def test_a_flag_fault_evidence_set_states_the_flag_and_its_commit(flag: str, variant: str) -> None:
    """The bundle a flag Scenario produces carries what the Rollback must cite."""
    rendered = (
        cart_bundle()
        .model_copy(
            update={
                "commits": (
                    Commit(
                        sha=INJECTION,
                        message=f"Inject the flag fault: {flag} is {variant}",
                        author="pagerless",
                        time=datetime(2026, 11, 17, 8, 48, tzinfo=UTC),
                    ),
                )
            }
        )
        .render()
    )

    assert f"{flag} is {variant}" in rendered
    assert INJECTION in rendered
    assert "## Recent Sandbox commits (newest first)" in rendered
