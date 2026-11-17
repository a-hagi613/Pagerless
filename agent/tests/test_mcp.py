"""The read-only MCP boundary, exercised against real in-process MCP servers."""

from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from types import SimpleNamespace
from typing import Any

import pytest
from mcp import Client
from mcp.server import MCPServer
from mcp.types import CallToolResult, TextContent, Tool

from pagerless_agent.mcp import MCPServerConfig, MCPTools, ToolPolicyError
from pagerless_agent.tools import UnknownToolError
from tests.fakes import StaticToken

TOKEN = StaticToken()


async def test_only_allowlisted_read_tools_are_exposed_and_called() -> None:
    server = MCPServer("Kubernetes")
    calls: list[str] = []

    @server.tool()
    def pods_log(namespace: str, name: str) -> str:
        calls.append(name)
        return "the container was OOMKilled"

    @server.tool()
    def pods_exec(namespace: str, name: str, command: str) -> str:
        raise AssertionError("a write-capable tool must never run")

    async with MCPTools(
        (MCPServerConfig("k8s", "http://kubernetes/mcp", TOKEN),),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=lambda _: Client(server),
    ) as tools:
        assert [spec.name for spec in tools.specs()] == ["k8s__pods_log"]
        result = await tools.call("k8s__pods_log", {"namespace": "sandbox", "name": "checkout-1"})
        assert result.text == "the container was OOMKilled"
        assert not result.is_error
        assert calls == ["checkout-1"]


async def test_namespace_and_repository_boundaries_refuse_calls_before_the_server() -> None:
    server = MCPServer("reads")
    calls: list[dict[str, str]] = []

    @server.tool()
    def resources_get(apiVersion: str, kind: str, name: str, namespace: str) -> dict[str, str]:  # noqa: N803
        calls.append({"kind": kind, "namespace": namespace})
        return {"kind": kind}

    @server.tool()
    def list_commits(owner: str, repo: str) -> list[dict[str, str]]:
        calls.append({"owner": owner, "repo": repo})
        return []

    async with MCPTools(
        (
            MCPServerConfig("k8s", "http://k8s/mcp", TOKEN),
            MCPServerConfig("github", "http://github/readonly", TOKEN),
        ),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=lambda _: Client(server),
    ) as tools:
        for args in (
            {"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout", "namespace": "kube-system"},
            {"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout", "allNamespaces": True},
            {"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout", "cluster": "other"},
        ):
            assert (await tools.call("k8s__resources_get", args)).is_error
        assert (await tools.call("github__list_commits", {"owner": "other", "repo": "private"})).is_error
        with pytest.raises(UnknownToolError):
            await tools.call("k8s__pods_exec", {})
        assert calls == []
        await tools.call("github__list_commits", {})
        assert calls == [{"owner": "example", "repo": "sandbox"}]


async def test_a_secret_read_reaches_the_gateway_so_its_policy_can_refuse_it() -> None:
    server = MCPServer("Kubernetes")
    seen: list[dict[str, str]] = []

    @server.tool()
    def resources_get(apiVersion: str, kind: str, name: str, namespace: str) -> dict[str, str]:  # noqa: N803
        seen.append({"apiVersion": apiVersion, "kind": kind, "name": name, "namespace": namespace})
        return {"kind": kind}

    async with MCPTools(
        (MCPServerConfig("k8s", "http://kubernetes/mcp", TOKEN),),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=lambda _: Client(server),
    ) as tools:
        await tools.call(
            "k8s__resources_get",
            {"apiVersion": "v1", "kind": "Secret", "name": "github-write", "namespace": "sandbox"},
        )

    # The client forwards the read, so agentgateway's policy is the boundary that
    # refuses it by argument and logs the denial (ADR-0022, Scenario 10).
    assert seen == [
        {
            "apiVersion": "v1",
            "kind": "Secret",
            "name": "github-write",
            "namespace": "sandbox",
        }
    ]


async def test_complete_objects_are_read_past_a_reduced_summary_table() -> None:
    complete = "- apiVersion: v1\n  kind: Pod\n  metadata:\n    name: checkout-1\n"
    summary = {"columns": ["Name"], "items": [{"Name": "checkout-1"}]}

    class SummaryClient:
        async def list_tools(self, cursor: str | None = None) -> SimpleNamespace:
            return SimpleNamespace(
                tools=[Tool(name="pods_list_in_namespace", description="", input_schema={"type": "object"})],
                next_cursor=None,
            )

        async def call_tool(self, name: str, arguments: dict[str, object]) -> CallToolResult:
            return CallToolResult(
                content=[TextContent(type="text", text=complete)],
                structured_content=summary,
                is_error=False,
            )

    @asynccontextmanager
    async def factory(_config: MCPServerConfig) -> AsyncIterator[Any]:
        yield SummaryClient()

    async with MCPTools(
        (MCPServerConfig("k8s", "http://kubernetes/mcp", TOKEN),),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=factory,
    ) as tools:
        assert await tools.read("k8s", "pods_list_in_namespace", {}) == summary
        objects = await tools.read_objects("k8s", "pods_list_in_namespace", {})

    assert objects == [{"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "checkout-1"}}]


async def test_structured_evidence_errors_and_large_outputs_preserve_the_tool_boundary() -> None:
    server = MCPServer("Kubernetes")

    @server.tool()
    def resources_get(apiVersion: str, kind: str, name: str, namespace: str) -> dict[str, object]:  # noqa: N803
        return {"kind": kind, "metadata": {"name": name, "namespace": namespace}}

    @server.tool()
    def pods_log(namespace: str, name: str) -> str:
        if name == "missing":
            raise ValueError("pod not found")
        return "untrusted log " * 4000

    async with MCPTools(
        (MCPServerConfig("k8s", "http://k8s/mcp", TOKEN),),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=lambda _: Client(server),
    ) as tools:
        data = await tools.read(
            "k8s", "resources_get", {"apiVersion": "v1", "kind": "Pod", "name": "checkout"}
        )
        assert data == {"kind": "Pod", "metadata": {"name": "checkout", "namespace": "sandbox"}}
        error = await tools.call("k8s__pods_log", {"name": "missing"})
        assert error.is_error and "pods_log" in error.text
        result = await tools.call("k8s__pods_log", {"name": "checkout"})
        assert len(result.text) < 8100 and result.text.endswith("[tool output truncated]")
    with pytest.raises(UnknownToolError):
        await tools.call("k8s__pods_log", {"name": "checkout"})


async def test_partial_initialization_closes_connections_and_never_leaves_tools_callable() -> None:
    server = MCPServer("Kubernetes")
    closed: list[str] = []

    @server.tool()
    def pods_log(namespace: str, name: str) -> str:
        return "logs"

    @asynccontextmanager
    async def connection(config: MCPServerConfig) -> AsyncIterator[Client]:
        async with Client(server) as client:
            try:
                yield client
            finally:
                closed.append(config.name)

    tools = MCPTools(
        (MCPServerConfig("k8s", "http://k8s/mcp", TOKEN), MCPServerConfig("k8s", "http://other/mcp", TOKEN)),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=connection,
    )
    with pytest.raises(ToolPolicyError, match="duplicate"):
        async with tools:
            raise AssertionError("invalid configuration must not be entered")
    assert closed == ["k8s"]
    assert tools.specs() == []
