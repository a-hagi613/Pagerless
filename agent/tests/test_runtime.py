"""The runnable Job drives seam 3 with real MCP connections and the report HTTP boundary."""

from __future__ import annotations

from pathlib import Path

import httpx
import pytest
from mcp import Client

from pagerless_agent.model import Stage
from pagerless_agent.replay import RecordedTurn, Recording
from pagerless_agent.runtime import main, run
from pagerless_agent.settings import ConfigError, RuntimeSettings
from tests.fakes import ScriptedProvider, StandInServer, StaticToken, call, respond
from tests.test_live_context import evidence_server
from tests.test_settings import ENV

RUNTIME_ENV = {
    **ENV,
    "PAGERLESS_MCP_GATEWAY_URL": "http://mcp-gateway.agentgateway-system.svc.cluster.local",
    "PAGERLESS_KEYCLOAK_ISSUER": "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless",
    "PAGERLESS_KEYCLOAK_TOKEN_URL": (
        "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless/protocol/openid-connect/token"
    ),
    "PAGERLESS_KEYCLOAK_CLIENT_ID": "pagerless-agent",
    "PAGERLESS_KEYCLOAK_TOKEN_FILE": "/var/run/keycloak/token",
    "PAGERLESS_SANDBOX_OWNER": "example",
    "PAGERLESS_SANDBOX_REPO": "sandbox",
}


def test_runtime_defaults_to_replay_and_refuses_unsafe_configuration() -> None:
    assert RuntimeSettings.from_env(RUNTIME_ENV).provider == "replay"
    for addition, message in (
        ({"PAGERLESS_MODEL_PROVIDER": "other"}, "must be replay"),
        ({"PAGERLESS_ALLOWED_NAMESPACES": "production"}, "not allowlisted"),
        ({"PAGERLESS_RECORDING_FILE": "/tmp/live.json"}, "cannot produce a live recording"),
        ({"PAGERLESS_MCP_GATEWAY_URL": "http://user:secret@gateway"}, "without embedded credentials"),
        ({"PAGERLESS_MCP_GATEWAY_URL": "http://gateway/mcp"}, "without a path"),
        ({"PAGERLESS_KEYCLOAK_ISSUER": "http://keycloak:8080"}, "must name a realm"),
        ({"PAGERLESS_KEYCLOAK_ISSUER": "http://keycloak/realms/pagerless/"}, "trailing slash"),
        (
            {"PAGERLESS_KEYCLOAK_TOKEN_URL": "http://keycloak/realms/other/protocol/openid-connect/token"},
            "token endpoint",
        ),
        ({"PAGERLESS_KEYCLOAK_CLIENT_ID": "pagerless/agent"}, "single client identifier"),
    ):
        with pytest.raises(ConfigError, match=message):
            RuntimeSettings.from_env({**RUNTIME_ENV, **addition})
    assert main({}) == 1


def test_every_mcp_endpoint_is_a_route_of_the_gateway() -> None:
    token = StaticToken()
    settings = RuntimeSettings.from_env(RUNTIME_ENV)
    servers = settings.servers(token)
    assert [(config.name, config.url) for config in servers] == [
        ("k8s", "http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/kubernetes"),
        ("grafana", "http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/grafana"),
        ("argocd", "http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/argocd"),
        ("github", "http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/github"),
    ]
    assert {config.token_source for config in servers} == {token}
    assert settings.keycloak.issuer == "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless"
    assert settings.keycloak.client_id == "pagerless-agent"


async def test_replay_runs_real_tools_and_reports_the_result_without_creating_a_live_recording(
    tmp_path: Path,
) -> None:
    server, calls = evidence_server()
    token = tmp_path / "token"
    token.write_text("report-token", encoding="utf-8")
    recording = tmp_path / "scripted-test-replay.json"
    finding = {"kind": "Log", "source": "k8s__pods_log", "summary": "The container was OOMKilled."}
    Recording(
        scenario="synthetic-test-only",
        model="scripted-test-provider",
        workload="checkout",
        turns=(
            RecordedTurn(
                stage=Stage.INVESTIGATE,
                response=respond(
                    call(
                        "call_1",
                        "k8s__pods_log",
                        name="checkout-new",
                        container="app",
                        tail=50,
                        previous=True,
                    )
                ),
            ),
            RecordedTurn(
                stage=Stage.INVESTIGATE, response=respond(text="The evidence supports the Diagnosis.")
            ),
            RecordedTurn(
                stage=Stage.DIAGNOSE,
                response=respond(
                    structured={
                        "diagnosis": "The container exceeded its memory limit.",
                        "findings": [finding],
                    }
                ),
            ),
            RecordedTurn(stage=Stage.PROPOSE, response=respond(structured={"action": "NoAction"})),
        ),
    ).save(recording)
    env = {
        **RUNTIME_ENV,
        "PAGERLESS_PAGE": "page-1",
        "PAGERLESS_TOKEN_FILE": str(token),
        "PAGERLESS_REPLAY_FILE": str(recording),
        "PAGERLESS_TRIGGERING_POD": "checkout-new",
    }
    report_server = StandInServer(token="report-token", page="page-1")
    http = httpx.AsyncClient(transport=report_server.transport())
    outcome = await run(
        RuntimeSettings.from_env(env), client_factory=lambda _: Client(server), server_http=http
    )
    assert outcome.kind == "reported" and outcome.exit_code == 0
    assert calls.count("logs") == 3
    assert report_server.reports[0]["proposal"] == {"action": "NoAction"}
    assert http.is_closed
    assert not (tmp_path / "live.json").exists()


@pytest.mark.parametrize("accepted", [True, False])
async def test_only_a_completed_live_investigation_replaces_the_recording(
    tmp_path: Path,
    accepted: bool,
) -> None:
    server, _ = evidence_server()
    token = tmp_path / "token"
    token.write_text("report-token", encoding="utf-8")
    destination = tmp_path / "captured.json"
    destination.write_text("previous recording", encoding="utf-8")
    finding = {"kind": "Log", "source": "k8s__pods_log", "summary": "The container was OOMKilled."}
    provider = ScriptedProvider(
        [
            respond(text="The evidence supports the Diagnosis."),
            respond(structured={"diagnosis": "The memory limit is too low.", "findings": [finding]}),
            respond(structured={"action": "NoAction"}),
        ]
    )
    env = {
        **RUNTIME_ENV,
        "PAGERLESS_PAGE": "page-1",
        "PAGERLESS_TOKEN_FILE": str(token),
        "PAGERLESS_MODEL_PROVIDER": "anthropic",
        "PAGERLESS_RECORDING_FILE": str(destination),
    }
    report_server = StandInServer(token="report-token", page="page-1")
    report_server.investigating = accepted
    outcome = await run(
        RuntimeSettings.from_env(env),
        client_factory=lambda _: Client(server),
        server_http=httpx.AsyncClient(transport=report_server.transport()),
        provider=provider,
    )
    if accepted:
        assert outcome.kind == "reported"
        assert [turn.stage for turn in Recording.load(destination).turns] == [
            Stage.INVESTIGATE,
            Stage.DIAGNOSE,
            Stage.PROPOSE,
        ]
    else:
        assert outcome.kind == "rejected"
        assert destination.read_text(encoding="utf-8") == "previous recording"
    assert list(tmp_path.glob(".pagerless-recording-*")) == []
