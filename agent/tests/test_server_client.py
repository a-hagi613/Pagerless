"""The agent's HTTP client for pagerless-server, against the stand-in server."""

from __future__ import annotations

from pathlib import Path

import httpx
import pytest

from pagerless_agent.contract import Report, StepEvent
from pagerless_agent.server import ServerClient, ServerError, ServerRefusedError
from tests.fakes import StandInServer

REPORT = Report.model_validate(
    {
        "diagnosis": "d",
        "findings": [{"kind": "Log", "source": "s", "summary": "m"}],
        "proposal": {"action": "NoAction"},
    }
)
EVENT = StepEvent(kind="Progress", summary="Investigating.")


class Sleeps:
    def __init__(self) -> None:
        self.delays: list[float] = []

    async def __call__(self, delay: float) -> None:
        self.delays.append(delay)


def client(server: StandInServer, token_file: Path, sleeps: Sleeps | None = None) -> ServerClient:
    return ServerClient(
        "http://pagerless-server/",
        server.triage,
        token_file,
        http=httpx.AsyncClient(transport=server.transport()),
        sleep=sleeps or Sleeps(),
    )


@pytest.fixture
def token_file(tmp_path: Path) -> Path:
    path = tmp_path / "token"
    path.write_text("token-1\n")
    return path


async def test_the_token_is_read_again_for_every_request_because_the_kubelet_rotates_it(
    token_file: Path,
) -> None:
    server = StandInServer()
    api = client(server, token_file)

    await api.post_event(EVENT)
    server.token = "token-2"
    token_file.write_text("token-2")
    await api.post_event(EVENT)

    assert [r.headers["Authorization"] for r in server.requests] == ["Bearer token-1", "Bearer token-2"]
    assert len(server.events) == 2


async def test_an_event_is_posted_to_the_triages_events_path(token_file: Path) -> None:
    server = StandInServer(triage="triage-1")
    await client(server, token_file).post_event(
        StepEvent(kind="ToolCall", tool="pods_log", summary="pods_log {}")
    )
    assert [r.url.path for r in server.requests] == ["/v1/triages/triage-1/events"]
    assert server.events == [{"kind": "ToolCall", "tool": "pods_log", "summary": "pods_log {}"}]


async def test_a_triage_name_cannot_change_the_path(token_file: Path) -> None:
    server = StandInServer(triage="a/../b")
    api = client(server, token_file)
    await api.post_event(EVENT)
    assert server.requests[0].url.raw_path == b"/v1/triages/a%2F..%2Fb/events"


class TestEvents:
    async def test_an_unauthenticated_agent_is_refused(self, token_file: Path) -> None:
        server = StandInServer()
        token_file.write_text("wrong")
        with pytest.raises(ServerRefusedError) as refused:
            await client(server, token_file).post_event(EVENT)
        assert refused.value.status == 401

    async def test_a_triage_that_is_no_longer_investigating_refuses_the_agent(self, token_file: Path) -> None:
        server = StandInServer()
        server.investigating = False
        with pytest.raises(ServerRefusedError) as refused:
            await client(server, token_file).post_event(EVENT)
        assert refused.value.status == 409

    async def test_an_unavailable_server_is_retried_with_a_growing_pause_and_then_forgiven(
        self, token_file: Path
    ) -> None:
        server = StandInServer()
        server.unavailable_for = 2
        sleeps = Sleeps()
        await client(server, token_file, sleeps).post_event(EVENT)
        assert len(server.events) == 1
        assert sleeps.delays == [0.5, 1.0]

        server.unavailable_for = 99
        await client(server, token_file, sleeps).post_event(EVENT)
        assert len(server.events) == 1

    async def test_an_unreadable_token_is_an_error_for_the_report_and_not_a_crash_for_an_event(
        self, tmp_path: Path
    ) -> None:
        server = StandInServer()
        api = client(server, tmp_path / "absent")
        await api.post_event(EVENT)
        with pytest.raises(ServerError, match="cannot read the service account token"):
            await api.post_report(REPORT)
        assert server.requests == []


class TestReport:
    async def test_an_accepted_report_returns_the_new_phase(self, token_file: Path) -> None:
        server = StandInServer()
        answer = await client(server, token_file).post_report(REPORT)
        assert answer.accepted
        assert answer.phase == "Proposed"
        assert server.reports == [
            {
                "diagnosis": "d",
                "findings": [{"kind": "Log", "source": "s", "summary": "m"}],
                "proposal": {"action": "NoAction"},
            }
        ]

    async def test_validation_errors_are_returned_for_the_revision(self, token_file: Path) -> None:
        server = StandInServer()
        server.refuse_report(
            ("proposal.action", "action Scale is not accepted"), ("findings[0].source", "cite it")
        )
        answer = await client(server, token_file).post_report(REPORT)
        assert not answer.accepted
        assert [(e.field, e.message) for e in answer.errors] == [
            ("proposal.action", "action Scale is not accepted"),
            ("findings[0].source", "cite it"),
        ]

    async def test_a_report_for_a_triage_that_has_moved_on_is_a_refusal_of_the_agent(
        self, token_file: Path
    ) -> None:
        server = StandInServer()
        server.investigating = False
        with pytest.raises(ServerRefusedError) as refused:
            await client(server, token_file).post_report(REPORT)
        assert refused.value.status == 409
        assert "not Investigating" in str(refused.value)

    @pytest.mark.parametrize(
        "answer",
        [
            httpx.Response(422, json={"errors": []}),
            httpx.Response(422, text="nope"),
            httpx.Response(422, json={"errors": [{"field": "x"}]}),
            httpx.Response(413, json={"message": "body exceeds 1048576 bytes"}),
            httpx.Response(400, json={"message": "malformed JSON body"}),
            httpx.Response(500, json={"message": "internal error"}),
        ],
    )
    async def test_an_answer_the_agent_cannot_act_on_is_an_error(
        self, token_file: Path, answer: httpx.Response
    ) -> None:
        server = StandInServer()
        server.report_answers.append(answer)
        with pytest.raises(ServerError):
            await client(server, token_file).post_report(REPORT)

    async def test_a_server_that_stays_unavailable_is_an_error_after_three_attempts(
        self, token_file: Path
    ) -> None:
        server = StandInServer()
        server.unavailable_for = 99
        with pytest.raises(ServerError, match="unavailable after 3 attempts"):
            await client(server, token_file).post_report(REPORT)
        assert len(server.requests) == 3
