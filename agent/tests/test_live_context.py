"""The Context Bundle gathers real MCP reads before the model sees any evidence."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

import yaml
from mcp import Client
from mcp.server import MCPServer

from pagerless_agent.live_context import MCPContextSource
from pagerless_agent.mcp import MCPServerConfig, MCPTools
from pagerless_agent.settings import Target
from tests.fakes import StaticToken

TOKEN = StaticToken()


def evidence_server() -> tuple[MCPServer, list[str]]:
    """Pinned upstream tool shapes, with deterministic external evidence for seam tests."""
    server = MCPServer("evidence")
    calls: list[str] = []

    @server.tool()
    def resources_get(apiVersion: str, kind: str, name: str, namespace: str) -> dict[str, Any]:  # noqa: N803
        calls.append("workload")
        return {"spec": {"selector": {"matchLabels": {"app": "checkout"}}}, "status": {"readyReplicas": 0}}

    @server.tool()
    def pods_list_in_namespace(namespace: str, labelSelector: str) -> dict[str, Any]:  # noqa: N803
        assert labelSelector == "app=checkout"
        calls.append("pods")
        return {
            "items": [
                {
                    "metadata": {"name": name, "creationTimestamp": created},
                    "spec": {"containers": [{"name": "app"}]},
                    "status": {
                        "phase": "Running",
                        "containerStatuses": [
                            {
                                "ready": False,
                                "restartCount": 2,
                                "lastState": {"terminated": {"reason": "OOMKilled"}},
                            }
                        ],
                    },
                }
                for name, created in (
                    ("checkout-new", "2026-11-16T12:01:00Z"),
                    ("checkout-old", "2026-11-16T12:00:00Z"),
                )
            ]
        }

    @server.tool()
    def events_list(namespace: str) -> dict[str, Any]:
        calls.append("events")
        return {
            "columns": ["Namespace", "Timestamp", "Type", "Reason", "InvolvedObject", "Message"],
            "items": [
                {
                    "Namespace": namespace,
                    "Timestamp": "2026-11-16 12:02:00.123456789 +0000 UTC",
                    "Type": "Warning",
                    "Reason": "BackOff",
                    "InvolvedObject": "Pod/checkout-new",
                    "Message": "back-off restarting",
                },
                {
                    "Namespace": namespace,
                    "Timestamp": "2026-11-16T12:02:00Z",
                    "Type": "Warning",
                    "Reason": "BackOff",
                    "InvolvedObject": "Pod/other",
                    "Message": "unrelated",
                },
            ],
        }

    @server.tool()
    def pods_log(namespace: str, name: str, container: str, tail: int, previous: bool) -> str:
        calls.append("logs")
        assert previous and tail == 50
        return "allocating memory\nKilled"

    @server.tool()
    def resources_list(apiVersion: str, kind: str, namespace: str) -> str:  # noqa: N803
        # Pinned Chaos Mesh 2.8.8 status shape, with one experiment per case the
        # Context Bundle decides: a match, a namespace-wide match, another
        # Workload, a recovered run, and another namespace.
        # kubernetes-mcp-server returns complete objects as YAML text.
        calls.append("experiments")
        assert (apiVersion, kind, namespace) == ("chaos-mesh.org/v1alpha1", "StressChaos", "sandbox")
        return yaml.safe_dump(
            [
                {
                    "metadata": {"name": "cpu-stress-checkout"},
                    "spec": {
                        "selector": {
                            "namespaces": ["sandbox"],
                            "labelSelectors": {"app": "checkout"},
                        },
                        "duration": "30m",
                    },
                    "status": {
                        "conditions": [
                            {"type": "AllInjected", "status": "True"},
                            {"type": "AllRecovered", "status": "False"},
                        ],
                        "experiment": {"desiredPhase": "Run"},
                    },
                },
                {
                    "metadata": {"name": "cpu-stress-sandbox"},
                    "spec": {"selector": {"namespaces": ["sandbox"]}, "duration": "5m"},
                    "status": {"experiment": {"desiredPhase": "Run"}},
                },
                {
                    "metadata": {"name": "cpu-stress-cart"},
                    "spec": {
                        "selector": {
                            "namespaces": ["sandbox"],
                            "labelSelectors": {"app": "cart"},
                        },
                        "duration": "30m",
                    },
                    "status": {"experiment": {"desiredPhase": "Run"}},
                },
                {
                    "metadata": {"name": "cpu-stress-recovered"},
                    "spec": {
                        "selector": {
                            "namespaces": ["sandbox"],
                            "labelSelectors": {"app": "checkout"},
                        },
                        "duration": "30m",
                    },
                    "status": {
                        "conditions": [
                            {"type": "AllInjected", "status": "False"},
                            {"type": "AllRecovered", "status": "True"},
                        ],
                        "experiment": {"desiredPhase": "Run"},
                    },
                },
                {
                    "metadata": {"name": "memory-stress-elsewhere"},
                    "spec": {
                        "selector": {
                            "namespaces": ["other"],
                            "labelSelectors": {"app": "checkout"},
                        },
                        "duration": "10m",
                    },
                },
            ]
        )

    @server.tool()
    def get_application(applicationName: str) -> dict[str, Any]:  # noqa: N803
        calls.append("argocd")
        assert applicationName == "sandbox"
        return {
            "status": {"sync": {"status": "Synced", "revision": "a" * 40}, "health": {"status": "Degraded"}}
        }

    @server.tool()
    def list_commits(owner: str, repo: str, perPage: int) -> list[dict[str, Any]]:  # noqa: N803
        calls.append("commits")
        assert (owner, repo, perPage) == ("example", "sandbox", 20)
        return [
            {
                "sha": "b" * 40,
                "commit": {
                    "message": "Lower memory",
                    "author": {"name": "dev", "date": "2026-11-16T11:00:00Z"},
                },
            }
        ]

    return server, calls


async def test_the_bundle_uses_scoped_live_reads_and_canonical_evidence_order() -> None:
    server, calls = evidence_server()
    async with MCPTools(
        (
            MCPServerConfig("k8s", "http://evidence/mcp", TOKEN),
            MCPServerConfig("argocd", "http://evidence/mcp", TOKEN),
            MCPServerConfig("github", "http://evidence/mcp", TOKEN),
        ),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=lambda _: Client(server),
    ) as tools:
        source = MCPContextSource(tools, clock=lambda: datetime(2026, 11, 16, 12, 3, tzinfo=UTC))
        bundle = await source.gather(Target("triage-1", "Deployment", "sandbox", "checkout", "checkout-new"))
    assert [pod.name for pod in bundle.pods] == ["checkout-old", "checkout-new"]
    assert bundle.pods[0].last_termination_reason == "OOMKilled"
    assert bundle.events[0].message == "back-off restarting" and len(bundle.events) == 1
    assert bundle.events[0].time == datetime(2026, 11, 16, 12, 2, 0, 123456, tzinfo=UTC)
    assert bundle.logs[0].pod == "checkout-new" and "Killed" in bundle.logs[0].lines
    assert "Degraded" in bundle.argocd_status
    assert bundle.commits[0].sha == "b" * 40
    # Only the active experiments that select the Workload's pods are evidence:
    # one by label, and one that selects every pod in the namespace.
    assert [experiment.name for experiment in bundle.experiments] == [
        "cpu-stress-checkout",
        "cpu-stress-sandbox",
    ]
    assert bundle.experiments[0].kind == "StressChaos"
    assert bundle.experiments[0].target == "Deployment sandbox/checkout"
    assert bundle.experiments[0].duration == "30m"
    assert bundle.experiments[1].duration == "5m"
    assert calls == ["workload", "pods", "events", "logs", "logs", "argocd", "experiments", "commits"]


async def test_a_deleted_triggering_pod_does_not_prevent_investigating_the_workload() -> None:
    server, _ = evidence_server()
    async with MCPTools(
        (
            MCPServerConfig("k8s", "http://evidence/mcp", TOKEN),
            MCPServerConfig("argocd", "http://evidence/mcp", TOKEN),
            MCPServerConfig("github", "http://evidence/mcp", TOKEN),
        ),
        namespace="sandbox",
        owner="example",
        repository="sandbox",
        client_factory=lambda _: Client(server),
    ) as tools:
        bundle = await MCPContextSource(tools).gather(
            Target("triage-1", "Deployment", "sandbox", "checkout", "checkout-deleted")
        )
    assert bundle.triggering_pod is None
    assert len(bundle.pods) == 2


async def test_bundle_tolerates_disabled_optional_mcp_servers_and_missing_chaos_mesh_crd() -> None:
    server = MCPServer("minimal-k8s")
    calls: list[str] = []

    @server.tool()
    def resources_get(apiVersion: str, kind: str, name: str, namespace: str) -> dict[str, Any]:  # noqa: N803
        calls.append("workload")
        return {"spec": {"selector": {"matchLabels": {"app": "checkout"}}}, "status": {"readyReplicas": 1}}

    @server.tool()
    def pods_list_in_namespace(namespace: str, labelSelector: str) -> dict[str, Any]:  # noqa: N803
        calls.append("pods")
        return {
            "items": [
                {
                    "metadata": {"name": "checkout-1", "creationTimestamp": "2026-11-16T12:00:00Z"},
                    "spec": {"containers": [{"name": "app"}]},
                    "status": {
                        "phase": "Running",
                        "containerStatuses": [{"name": "app", "ready": True, "restartCount": 0}],
                    },
                }
            ]
        }

    @server.tool()
    def events_list(namespace: str) -> dict[str, Any]:
        calls.append("events")
        return {"items": []}

    @server.tool()
    def pods_log(namespace: str, name: str, container: str, tail: int, previous: bool) -> str:
        calls.append("logs")
        return "listening on :8080"

    @server.tool()
    def resources_list(apiVersion: str, kind: str, namespace: str) -> str:  # noqa: N803
        calls.append("experiments")
        raise ValueError('no matches for kind "StressChaos" in version "chaos-mesh.org/v1alpha1"')

    async with MCPTools(
        (MCPServerConfig("k8s", "http://evidence/mcp", TOKEN),),
        namespace="production",
        client_factory=lambda _: Client(server),
    ) as tools:
        assert tools.has_server("k8s")
        assert not tools.has_server("argocd")
        assert not tools.has_server("github")
        bundle = await MCPContextSource(tools).gather(
            Target("triage-live", "Deployment", "production", "checkout")
        )

    assert bundle.argocd_status == "Argo CD integration is not enabled for this investigation."
    assert bundle.experiments == ()
    assert bundle.commits == ()
    assert calls == ["workload", "pods", "events", "logs", "experiments"]


async def test_bundle_tolerates_unmanaged_argocd_app_unconfigured_github_and_nonlist_chaos_response() -> None:
    server = MCPServer("unmanaged")

    @server.tool()
    def resources_get(apiVersion: str, kind: str, name: str, namespace: str) -> dict[str, Any]:  # noqa: N803
        return {"spec": {"selector": {"matchLabels": {"app": "checkout"}}}, "status": {"readyReplicas": 1}}

    @server.tool()
    def pods_list_in_namespace(namespace: str, labelSelector: str) -> dict[str, Any]:  # noqa: N803
        return {"items": []}

    @server.tool()
    def events_list(namespace: str) -> dict[str, Any]:
        return {"items": []}

    @server.tool()
    def resources_list(apiVersion: str, kind: str, namespace: str) -> str:  # noqa: N803
        return "No resources found in sandbox namespace."

    @server.tool()
    def get_application(applicationName: str) -> dict[str, Any]:  # noqa: N803
        raise ValueError("applications.argoproj.io 'sandbox' not found")

    @server.tool()
    def list_commits(owner: str, repo: str, perPage: int) -> list[dict[str, Any]]:  # noqa: N803
        raise AssertionError("unconfigured GitHub repository must be refused before reaching the server")

    async with MCPTools(
        (
            MCPServerConfig("k8s", "http://evidence/mcp", TOKEN),
            MCPServerConfig("argocd", "http://evidence/mcp", TOKEN),
            MCPServerConfig("github", "http://evidence/mcp", TOKEN),
        ),
        namespace="sandbox",
        owner="",
        repository="",
        client_factory=lambda _: Client(server),
    ) as tools:
        bundle = await MCPContextSource(tools).gather(
            Target("triage-unmanaged", "Deployment", "sandbox", "checkout")
        )

    assert bundle.argocd_status.startswith("Argo CD status unavailable:")
    assert bundle.experiments == ()
    assert bundle.commits == ()
