"""The read-only MCP boundary, exercised against real in-process MCP servers."""

from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import pytest
from mcp import Client
from mcp.server import MCPServer

from pagerless_agent.mcp import MCPServerConfig, MCPTools, ToolPolicyError
from pagerless_agent.tools import UnknownToolError


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
        (MCPServerConfig("k8s", "http://kubernetes/mcp"),),
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


async def test_namespace_resource_and_repository_boundaries_refuse_calls_before_the_server() -> None:
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
        (MCPServerConfig("k8s", "http://k8s/mcp"), MCPServerConfig("github", "http://github/readonly")),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=lambda _: Client(server),
    ) as tools:
        for args in (
            {"apiVersion": "v1", "kind": "Secret", "name": "token"},
            {"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout", "namespace": "kube-system"},
            {"apiVersion": "example.dev/v1", "kind": "Pod", "name": "checkout"},
        ):
            assert (await tools.call("k8s__resources_get", args)).is_error
        assert (await tools.call("github__list_commits", {"owner": "other", "repo": "private"})).is_error
        with pytest.raises(UnknownToolError):
            await tools.call("k8s__pods_exec", {})
        assert calls == []
        await tools.call("github__list_commits", {})
        assert calls == [{"owner": "example", "repo": "sandbox"}]


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
        (MCPServerConfig("k8s", "http://k8s/mcp"),),
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
        (MCPServerConfig("k8s", "http://k8s/mcp"), MCPServerConfig("k8s", "http://other/mcp")),
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
