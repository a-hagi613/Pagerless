"""The scoped tool broker client: attribution, idempotency, errors, and retries."""

from __future__ import annotations

from pathlib import Path
from uuid import uuid4

import httpx
import pytest

from pagerless_agent.contract import Capabilities, DiagnosisOnly, Page, Report, Usage
from pagerless_agent.toolclient import ServerError, ServerRefusedError, ToolClient
from pagerless_agent.tools import UnknownToolError
from tests.fakes import TOKEN, StandInBroker
from tests.support import manifest, settings


def client(
    tmp_path: Path, *, uid: str | None = None, broker: StandInBroker | None = None
) -> tuple[ToolClient, StandInBroker, object]:
    """Return a client, its broker, and the settings that bind them."""
    page_uid = uid or str(uuid4())
    selected = settings(tmp_path, uid=page_uid)
    server = broker or StandInBroker(manifest(uid=page_uid))
    http = httpx.AsyncClient(transport=server.transport())
    return (
        ToolClient(selected.server_url, selected.page, selected.page_uid, selected.token_file, http=http),
        server,
        selected,
    )


async def test_the_manifest_arrives_in_one_call_with_the_catalogue_and_limits(tmp_path: Path) -> None:
    selected_client, broker, _ = client(tmp_path)

    investigation = await selected_client.load_investigation()

    assert investigation.page.uid == broker.uid
    assert [spec.name for spec in investigation.tools] == ["pods_logs", "object_get", "repo_file"]
    assert investigation.limits.tool_calls == 15
    assert investigation.scope.namespaces == ("shop",)
    assert len([request for request in broker.requests if request.url.path.endswith("/investigation")]) == 1


async def test_a_manifest_for_another_page_is_refused(tmp_path: Path) -> None:
    page_uid = str(uuid4())
    broker = StandInBroker(manifest(uid=str(uuid4())))
    # The token is honoured, so the refusal must come from the local UID comparison.
    broker.uid = page_uid
    selected_client, _, _ = client(tmp_path, uid=page_uid, broker=broker)

    with pytest.raises(ServerError, match="different Page UID"):
        await selected_client.load_investigation()


async def test_a_tool_call_carries_the_page_uid_and_a_scoped_idempotency_key(tmp_path: Path) -> None:
    selected_client, broker, selected = client(tmp_path)
    await selected_client.load_investigation()

    result = await selected_client.call("pods_logs", "call_7", {"namespace": "shop", "pod": "checkout"})

    request = next(request for request in broker.requests if "/tools/" in request.url.path)
    assert request.headers["Authorization"] == f"Bearer {TOKEN}"
    assert request.headers["X-Pagerless-Page-Uid"] == selected.page_uid  # type: ignore[attr-defined]
    assert request.headers["X-Page-UID"] == selected.page_uid  # type: ignore[attr-defined]
    assert request.headers["Idempotency-Key"] == f"{selected.page_uid}/call_7"  # type: ignore[attr-defined]
    assert result.payload["ok"] is True
    assert result.call_id == "srv-1"


async def test_a_tool_outside_the_catalogue_never_becomes_a_request(tmp_path: Path) -> None:
    selected_client, broker, _ = client(tmp_path)
    await selected_client.load_investigation()

    with pytest.raises(UnknownToolError):
        await selected_client.call("k8s_delete_pod", "call_8", {"name": "checkout"})

    assert all("/tools/" not in request.url.path for request in broker.requests)


async def test_an_out_of_scope_answer_is_a_model_visible_tool_error(tmp_path: Path) -> None:
    page_uid = str(uuid4())
    broker = StandInBroker(manifest(uid=page_uid), out_of_scope=frozenset({"repo_file"}))
    selected_client, _, _ = client(tmp_path, uid=page_uid, broker=broker)
    await selected_client.load_investigation()

    result = await selected_client.call("repo_file", "call_9", {"path": "apps/x.yaml"})

    assert result.is_error is True
    assert result.payload["error"]["code"] == "out_of_scope"
    assert result.payload["error"]["field"] == "namespace"


async def test_a_missing_object_is_a_model_visible_tool_error(tmp_path: Path) -> None:
    class Missing(StandInBroker):
        def _tool(self, name: str, arguments: dict[str, object]) -> httpx.Response:
            return httpx.Response(
                404, json={"ok": False, "error": {"code": "not_found", "message": "the pod was replaced"}}
            )

    page_uid = str(uuid4())
    broker = Missing(manifest(uid=page_uid))
    selected_client, _, _ = client(tmp_path, uid=page_uid, broker=broker)
    await selected_client.load_investigation()

    result = await selected_client.call("pods_logs", "call_10", {"namespace": "shop", "pod": "gone"})

    assert result.is_error is True
    assert result.payload["error"]["code"] == "not_found"


async def test_an_unauthenticated_request_stops_the_run(tmp_path: Path) -> None:
    class Unauthenticated(StandInBroker):
        def handler(self, request: httpx.Request) -> httpx.Response:
            return httpx.Response(401, json={"message": "unauthenticated"})

    page_uid = str(uuid4())
    broker = Unauthenticated(manifest(uid=page_uid))
    selected_client, _, _ = client(tmp_path, uid=page_uid, broker=broker)

    with pytest.raises(ServerRefusedError) as raised:
        await selected_client.load_investigation()
    assert raised.value.status == 401


async def test_a_missing_projected_token_is_a_server_error(tmp_path: Path) -> None:
    page_uid = str(uuid4())
    selected = settings(tmp_path, uid=page_uid)
    selected.token_file.unlink()
    broker = StandInBroker(manifest(uid=page_uid))
    http = httpx.AsyncClient(transport=broker.transport())
    selected_client = ToolClient(
        selected.server_url, selected.page, selected.page_uid, selected.token_file, http=http
    )

    with pytest.raises(ServerError, match="cannot read the projected service account token"):
        await selected_client.load_investigation()


async def test_a_transient_server_failure_is_retried(tmp_path: Path) -> None:
    class Flaky(StandInBroker):
        def __init__(self, *args: object, **kwargs: object) -> None:
            super().__init__(*args, **kwargs)  # type: ignore[arg-type]
            self.failures = 1
            self.attempts = 0

        def handler(self, request: httpx.Request) -> httpx.Response:
            self.attempts += 1
            if self.failures:
                self.failures -= 1
                return httpx.Response(503, json={"message": "unavailable"})
            return super().handler(request)

    page_uid = str(uuid4())
    broker = Flaky(manifest(uid=page_uid))
    selected = settings(tmp_path, uid=page_uid)
    http = httpx.AsyncClient(transport=broker.transport())

    async def no_sleep(seconds: float) -> None:
        return None

    selected_client = ToolClient(
        selected.server_url,
        selected.page,
        selected.page_uid,
        selected.token_file,
        http=http,
        sleep=no_sleep,
    )

    investigation = await selected_client.load_investigation()

    assert investigation.page.uid == page_uid
    assert broker.attempts == 2


async def test_the_report_returns_the_servers_validation_errors(tmp_path: Path) -> None:
    selected_client, broker, selected = client(tmp_path)
    broker.refuse_report(("proposal.files[0].path", "the base commit moved"))
    report = Report(
        page=await _page(selected_client),
        diagnosis="diagnosed",
        findings=[],
        proposal=DiagnosisOnly(
            kind="DiagnosisOnly", reason="infrastructure", recommendation="Drain the node."
        ),
        usage=Usage(input_tokens=1, output_tokens=1),
        capabilities=Capabilities(),
    )

    answer = await selected_client.post_report(report)

    assert answer.accepted is False
    assert answer.errors[0].field == "proposal.files[0].path"


async def _page(selected_client: ToolClient) -> Page:
    investigation = await selected_client.load_investigation()
    return investigation.page


async def test_an_event_failure_does_not_stop_the_run(tmp_path: Path) -> None:
    class NoEvents(StandInBroker):
        def handler(self, request: httpx.Request) -> httpx.Response:
            if request.url.path.endswith("/events"):
                return httpx.Response(500, json={"message": "the events store is full"})
            return super().handler(request)

    page_uid = str(uuid4())
    broker = NoEvents(manifest(uid=page_uid))
    selected_client, _, _ = client(tmp_path, uid=page_uid, broker=broker)
    from pagerless_agent.contract import StepEvent

    await selected_client.post_event(StepEvent(kind="Progress", summary="Investigating."))
    assert broker.events == []


async def test_the_narrative_endpoints_round_trip(tmp_path: Path) -> None:
    selected_client, broker, _ = client(tmp_path)
    await selected_client.load_investigation()

    record = await selected_client.narrative_record()
    await selected_client.post_narrative("The Page recovered.")

    assert record.target.name == "checkout"
    assert broker.narratives[0]["narrative"] == "The Page recovered."
