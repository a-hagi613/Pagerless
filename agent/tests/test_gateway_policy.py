"""The gateway policy must stay aligned with the tools the agent may call."""

from __future__ import annotations

from pathlib import Path

import yaml

from pagerless_agent.mcp import _ALLOWED_TOOLS, GATEWAY_ENDPOINTS

ROOT = Path(__file__).resolve().parents[2]
VALUES = yaml.safe_load((ROOT / "deploy" / "gateway" / "values.yaml").read_text(encoding="utf-8"))
# The agent calls the Kubernetes server "k8s"; the chart names it after the route.
SERVER_KEYS = {"k8s": "kubernetes", "grafana": "grafana", "argocd": "argocd", "github": "github"}


def test_every_gateway_route_carries_exactly_the_agent_tool_allowlist() -> None:
    servers = VALUES["mcp"]["servers"]
    assert set(servers) == set(SERVER_KEYS.values())
    for agent_name, server_name in SERVER_KEYS.items():
        server = servers[server_name]
        policy_tools = set(server.get("tools", [])) | set(server.get("resourceTools", []))
        assert policy_tools == set(_ALLOWED_TOOLS[agent_name]), server_name
        assert server["path"].startswith("/")


def test_the_route_paths_match_the_endpoints_the_agent_derives() -> None:
    servers = VALUES["mcp"]["servers"]
    for agent_name, path in GATEWAY_ENDPOINTS.items():
        assert path == f"/mcp/{SERVER_KEYS[agent_name]}"
        server = servers[SERVER_KEYS[agent_name]]
        assert server["service"].startswith("mcp-")
        assert isinstance(server["port"], int)


def test_the_kubernetes_route_refuses_a_secret_by_argument() -> None:
    kubernetes = VALUES["mcp"]["servers"]["kubernetes"]
    assert kubernetes["resourceTools"] == ["resources_get", "resources_list"]
    assert kubernetes["deniedResourceKinds"] == ["secret"]
    assert "tokenKey" not in kubernetes


def test_the_realm_and_issuer_are_the_ones_the_bootstrap_provisions() -> None:
    assert VALUES["keycloak"]["issuer"].endswith("/realms/pagerless")
    assert VALUES["keycloak"]["audiences"] == ["account"]
    bootstrap = (ROOT / "hack" / "provision-keycloak.py").read_text(encoding="utf-8")
    assert 'REALM = "pagerless"' in bootstrap
    assert 'AGENT_CLIENT = "pagerless-agent"' in bootstrap
    assert 'DASHBOARD_CLIENT = "pagerless-dashboard"' in bootstrap
    assert 'APPROVERS_GROUP = "pagerless-approvers"' in bootstrap
