"""MCP connections through agentgateway with an explicit read-only, per-investigation policy."""

from __future__ import annotations

import json
from collections.abc import AsyncGenerator, AsyncIterator, Callable
from contextlib import AbstractAsyncContextManager, AsyncExitStack, asynccontextmanager
from dataclasses import dataclass
from typing import Any, Literal

import httpx2
import yaml
from mcp import Client
from mcp.client.streamable_http import streamable_http_client
from mcp.types import CallToolResult, TextContent

from pagerless_agent.keycloak import TokenProvider
from pagerless_agent.model import ToolSpec
from pagerless_agent.tools import MAX_TOOL_OUTPUT_CHARS, ToolResult, UnknownToolError

ServerName = Literal["k8s", "grafana", "argocd", "github"]

# Every MCP server is reached through agentgateway. The path selects the route
# whose AgentgatewayPolicy allowlists that server's tools.
GATEWAY_ENDPOINTS: dict[ServerName, str] = {
    "k8s": "/mcp/kubernetes",
    "grafana": "/mcp/grafana",
    "argocd": "/mcp/argocd",
    "github": "/mcp/github",
}


# Read-only server modes and Kubernetes RBAC remain independent boundaries.
_ALLOWED_TOOLS: dict[str, frozenset[str]] = {
    "k8s": frozenset(
        {"resources_get", "resources_list", "pods_get", "pods_list_in_namespace", "pods_log", "events_list"}
    ),
    "grafana": frozenset(
        {
            "list_datasources",
            "get_datasource_by_uid",
            "query_prometheus",
            "query_loki_logs",
            "query_loki_stats",
            "query_tempo",
            "get_tempo_trace",
        }
    ),
    "argocd": frozenset(
        {
            "get_application",
            "get_application_resource",
            "get_application_resource_tree",
            "get_application_workload_logs",
        }
    ),
    "github": frozenset({"list_commits", "get_commit", "get_file_contents", "get_pull_request"}),
}
# Each MCP tool is allowlisted by name. The Kubernetes resource kind is not
# filtered here on purpose: agentgateway's route-level AgentgatewayPolicy refuses
# a `resources_get` or `resources_list` whose `kind` is Secret (ADR-0022), and a
# denial the client made first would never reach the gateway's audit log. The
# Kubernetes MCP server's own denied-resource configuration and read-only Role
# remain the final boundaries.
_MAX_DISCOVERY_PAGES = 20


class ToolPolicyError(ValueError):
    """A tool request is outside this investigation's read-only scope."""


@dataclass(frozen=True)
class MCPServerConfig:
    """One MCP gateway route and the token source that authenticates every request."""

    name: ServerName
    url: str
    token_source: TokenProvider


def gateway_endpoints(base_url: str) -> tuple[tuple[ServerName, str], ...]:
    """Return the gateway route for every server, in a deterministic order."""
    base = base_url.rstrip("/")
    return tuple((name, base + path) for name, path in GATEWAY_ENDPOINTS.items())


@asynccontextmanager
async def connect(config: MCPServerConfig) -> AsyncIterator[Client]:
    """Own a bounded Streamable HTTP connection authenticated on every request."""
    async with httpx2.AsyncClient(
        auth=_GatewayToken(config.token_source), timeout=httpx2.Timeout(30, read=60)
    ) as http:
        transport = streamable_http_client(config.url, http_client=http, max_sse_event_size=1 << 20)
        async with Client(transport, read_timeout_seconds=60) as client:
            yield client


class _GatewayToken(httpx2.Auth):
    """Attach a current Keycloak access token to each request agentgateway receives."""

    def __init__(self, source: TokenProvider) -> None:
        self._source = source

    async def async_auth_flow(
        self, request: httpx2.Request
    ) -> AsyncGenerator[httpx2.Request, httpx2.Response]:
        request.headers["Authorization"] = f"Bearer {await self._source.access_token()}"
        yield request


class MCPTools:
    """A ToolRegistry that exposes only known reads, scoped to one Workload's namespace."""

    def __init__(
        self,
        configs: tuple[MCPServerConfig, ...],
        *,
        namespace: str,
        owner: str = "",
        repository: str = "",
        application: str = "sandbox",
        client_factory: Callable[[MCPServerConfig], AbstractAsyncContextManager[Client]] = connect,
    ) -> None:
        self._configs = configs
        self._namespace = namespace
        self._owner = owner
        self._repository = repository
        self._application = application
        self._factory = client_factory
        self._stack = AsyncExitStack()
        self._specs: list[ToolSpec] = []
        self._tools: dict[str, tuple[str, str, Client]] = {}

    async def __aenter__(self) -> MCPTools:
        try:
            for config in self._configs:
                if config.name not in _ALLOWED_TOOLS:
                    raise ToolPolicyError(f"unknown MCP server {config.name!r}")
                if any(name.startswith(config.name + "__") for name in self._tools):
                    raise ToolPolicyError(f"duplicate MCP server {config.name!r}")
                client = await self._stack.enter_async_context(self._factory(config))
                cursor = None
                for _ in range(_MAX_DISCOVERY_PAGES):
                    page = await client.list_tools(cursor=cursor)
                    for tool in page.tools:
                        if tool.name not in _ALLOWED_TOOLS[config.name]:
                            continue
                        name = f"{config.name}__{tool.name}"
                        if name in self._tools:
                            raise ToolPolicyError(f"duplicate MCP tool {name!r}")
                        self._tools[name] = (config.name, tool.name, client)
                        self._specs.append(
                            ToolSpec(
                                name=name,
                                description=(tool.description or "")[:1000],
                                input_schema=tool.input_schema,
                            )
                        )
                    cursor = page.next_cursor
                    if cursor is None:
                        break
                else:
                    raise ToolPolicyError(f"MCP server {config.name!r} returned too many tool pages")
            self._specs.sort(key=lambda spec: spec.name)
            return self
        except BaseException:
            await self.aclose()
            raise

    async def __aexit__(self, *_: object) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        """Close every owned connection, including after partial initialization."""
        await self._stack.aclose()
        self._tools.clear()
        self._specs.clear()

    def has_server(self, server: ServerName) -> bool:
        """Report whether the named MCP server is configured for this investigation."""
        return any(config.name == server for config in self._configs)

    def specs(self) -> list[ToolSpec]:
        """Return deterministically ordered, explicitly allowlisted tools."""
        return list(self._specs)

    async def call(self, name: str, arguments: dict[str, Any]) -> ToolResult:
        """Execute an allowed read and return bounded, untrusted tool content."""
        try:
            result = await self._invoke(name, arguments)
        except ToolPolicyError as error:
            return ToolResult(str(error), is_error=True)
        text = _result_text(result)
        if len(text) > MAX_TOOL_OUTPUT_CHARS:
            text = text[:MAX_TOOL_OUTPUT_CHARS] + "\n[tool output truncated]"
        return ToolResult(text, is_error=result.is_error)

    async def read(self, server: ServerName, tool: str, arguments: dict[str, Any]) -> Any:
        """Read structured evidence for deterministic Context Bundle gathering."""
        result = await self._invoke(f"{server}__{tool}", arguments)
        if result.is_error:
            raise ToolPolicyError(f"{server}__{tool} failed: {_result_text(result)[:500]}")
        if result.structured_content is not None:
            return result.structured_content
        text = _result_text(result)
        try:
            return json.loads(text)
        except ValueError:
            return text

    async def read_objects(self, server: ServerName, tool: str, arguments: dict[str, Any]) -> Any:
        """Read complete objects when a server also returns a reduced summary table.

        kubernetes-mcp-server reports a list of columns and summary rows as the
        structured result, while the complete objects arrive as YAML text.
        """
        result = await self._invoke(f"{server}__{tool}", arguments)
        if result.is_error:
            raise ToolPolicyError(f"{server}__{tool} failed: {_result_text(result)[:500]}")
        text = _result_text(result)
        try:
            value = yaml.safe_load(text)
        except yaml.YAMLError as error:
            raise ToolPolicyError(f"{server}__{tool} returned unparseable evidence") from error
        if value is not None:
            return value
        if result.structured_content is not None:
            return result.structured_content
        raise ToolPolicyError(f"{server}__{tool} returned no evidence")

    async def _invoke(self, name: str, arguments: dict[str, Any]) -> CallToolResult:
        if name not in self._tools:
            raise UnknownToolError(name)
        server, tool, client = self._tools[name]
        args = dict(arguments)
        if server == "k8s":
            if any(args.get(key) for key in ("context", "cluster", "kubeconfig", "allNamespaces")):
                raise ToolPolicyError("Kubernetes reads cannot select another cluster or all namespaces")
            if args.get("namespace", self._namespace) != self._namespace:
                raise ToolPolicyError("Kubernetes reads are restricted to the Workload's namespace")
            args["namespace"] = self._namespace
        elif server == "github":
            if not self._owner or not self._repository:
                raise ToolPolicyError("GitHub reads require a configured Sandbox repository")
            if (
                args.get("owner", self._owner) != self._owner
                or args.get("repo", self._repository) != self._repository
            ):
                raise ToolPolicyError("GitHub reads are restricted to the Sandbox repository")
            args.update(owner=self._owner, repo=self._repository)
        elif server == "argocd":
            if args.get("applicationName", self._application) != self._application:
                raise ToolPolicyError("Argo CD reads are restricted to the Sandbox application")
            if args.get("applicationNamespace", "argocd") != "argocd":
                raise ToolPolicyError("the Sandbox application is in the argocd namespace")
            args["applicationName"] = self._application
        return await client.call_tool(tool, args)


def _result_text(result: CallToolResult) -> str:
    texts = [block.text for block in result.content if isinstance(block, TextContent)]
    if texts:
        return "\n".join(texts)
    if result.structured_content is not None:
        return json.dumps(result.structured_content, ensure_ascii=False, sort_keys=True)
    return "[the tool returned no text or structured content]"
