"""The agent's environment, as the server's Job sets it."""

from __future__ import annotations

from pathlib import Path

import pytest

from pagerless_agent.settings import (
    ENV_MCP_SERVERS,
    ConfigError,
    RuntimeSettings,
    Settings,
    Target,
)
from tests.fakes import StaticToken

ENV = {
    "PAGERLESS_SERVER_URL": "http://pagerless-server.pagerless.svc:8443",
    "PAGERLESS_TOKEN_FILE": "/var/run/pagerless/token",
    "PAGERLESS_PAGE": "checkout-abc12",
    "PAGERLESS_TARGET_KIND": "Deployment",
    "PAGERLESS_TARGET_NAMESPACE": "sandbox",
    "PAGERLESS_TARGET_NAME": "checkout",
}

GATEWAY_ENV = {
    **ENV,
    "PAGERLESS_MCP_GATEWAY_URL": "http://mcp-gateway.agentgateway-system.svc.cluster.local",
    "PAGERLESS_KEYCLOAK_ISSUER": "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless",
    "PAGERLESS_KEYCLOAK_TOKEN_URL": (
        "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless/protocol/openid-connect/token"
    ),
    "PAGERLESS_KEYCLOAK_CLIENT_ID": "pagerless-agent",
    "PAGERLESS_KEYCLOAK_TOKEN_FILE": "/var/run/keycloak/token",
}


def test_the_settings_are_read_from_the_environment_the_job_sets() -> None:
    env = {
        **ENV,
        "PAGERLESS_TRIGGERING_POD": "checkout-7d9f8b6c5-x2k4q",
        "PAGERLESS_HINT": "after the deploy",
    }
    assert Settings.from_env(env) == Settings(
        server_url="http://pagerless-server.pagerless.svc:8443",
        token_file=Path("/var/run/pagerless/token"),
        target=Target(
            page="checkout-abc12",
            kind="Deployment",
            namespace="sandbox",
            name="checkout",
            triggering_pod="checkout-7d9f8b6c5-x2k4q",
            hint="after the deploy",
        ),
    )


def test_the_pod_and_the_hint_are_optional() -> None:
    target = Settings.from_env(ENV).target
    assert target.triggering_pod is None
    assert target.hint is None


def test_every_missing_variable_is_named() -> None:
    env = {k: v for k, v in ENV.items() if k not in ("PAGERLESS_PAGE", "PAGERLESS_TOKEN_FILE")}
    with pytest.raises(ConfigError, match="PAGERLESS_TOKEN_FILE, PAGERLESS_PAGE"):
        Settings.from_env(env)


def test_a_workload_kind_that_is_not_a_workload_is_refused() -> None:
    with pytest.raises(ConfigError, match="Deployment or StatefulSet"):
        Settings.from_env({**ENV, "PAGERLESS_TARGET_KIND": "Pod"})


def test_mcp_servers_default_to_all_when_sandbox_is_configured() -> None:
    settings = RuntimeSettings.from_env(
        {
            **GATEWAY_ENV,
            "PAGERLESS_SANDBOX_OWNER": "example",
            "PAGERLESS_SANDBOX_REPO": "sandbox",
        }
    )
    assert settings.mcp_servers == ("k8s", "grafana", "argocd", "github")
    assert settings.sandbox_owner == "example"
    assert settings.sandbox_repository == "sandbox"
    assert settings.application == "sandbox"


def test_mcp_servers_default_without_sandbox_and_enable_argocd_when_application_set() -> None:
    live_only = RuntimeSettings.from_env(GATEWAY_ENV)
    assert live_only.mcp_servers == ("k8s", "grafana")
    assert live_only.sandbox_owner == ""
    assert live_only.sandbox_repository == ""
    assert [cfg.name for cfg in live_only.servers(StaticToken())] == ["k8s", "grafana"]

    with_argocd = RuntimeSettings.from_env({**GATEWAY_ENV, "PAGERLESS_ARGOCD_APPLICATION": "payments"})
    assert with_argocd.mcp_servers == ("k8s", "grafana", "argocd")
    assert with_argocd.application == "payments"
    assert [cfg.name for cfg in with_argocd.servers(StaticToken())] == ["k8s", "grafana", "argocd"]


def test_explicit_mcp_servers_deduplicate_and_filter_gateway_routes() -> None:
    assert ENV_MCP_SERVERS == "PAGERLESS_MCP_SERVERS"
    settings = RuntimeSettings.from_env(
        {
            **GATEWAY_ENV,
            ENV_MCP_SERVERS: "k8s, grafana, k8s",
            "PAGERLESS_SANDBOX_OWNER": "ignored-when-github-disabled",
        }
    )
    assert settings.mcp_servers == ("k8s", "grafana")
    assert settings.sandbox_owner == ""
    assert settings.sandbox_repository == ""
    assert [(cfg.name, cfg.url) for cfg in settings.servers(StaticToken())] == [
        ("k8s", "http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/kubernetes"),
        ("grafana", "http://mcp-gateway.agentgateway-system.svc.cluster.local/mcp/grafana"),
    ]


def test_mcp_servers_validation_requires_k8s_and_known_server_names() -> None:
    with pytest.raises(ConfigError, match="PAGERLESS_MCP_SERVERS must include k8s"):
        RuntimeSettings.from_env({**GATEWAY_ENV, "PAGERLESS_MCP_SERVERS": "grafana,argocd"})

    with pytest.raises(ConfigError, match="PAGERLESS_MCP_SERVERS.*pagerduty"):
        RuntimeSettings.from_env({**GATEWAY_ENV, "PAGERLESS_MCP_SERVERS": "k8s,pagerduty"})

    with pytest.raises(ConfigError, match="PAGERLESS_SANDBOX_OWNER, PAGERLESS_SANDBOX_REPO"):
        RuntimeSettings.from_env({**GATEWAY_ENV, "PAGERLESS_MCP_SERVERS": "k8s,github"})
