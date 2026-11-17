"""Gather a bounded Context Bundle through the same read-only MCP boundary as the agent."""

from __future__ import annotations

import json
import re
from collections.abc import Callable
from datetime import UTC, datetime
from typing import Any

from pagerless_agent.bundle import BundleEvent, Commit, ContextBundle, PodLogs, PodState, WorkloadRef
from pagerless_agent.mcp import MCPTools, ToolPolicyError
from pagerless_agent.settings import Target

_MAX_PODS = 10
_MAX_LOG_PODS = 3
_MAX_CONTAINERS = 3
_MAX_TEXT = 4000


class ContextReadError(ValueError):
    """Evidence returned by a read-only service is missing or malformed."""


class MCPContextSource:
    """The deterministic, pre-model reads required by spec #1 and ADR-0016."""

    def __init__(self, tools: MCPTools, *, clock: Callable[[], datetime] = lambda: datetime.now(UTC)) -> None:
        self._tools = tools
        self._clock = clock

    async def gather(self, target: Target) -> ContextBundle:
        """Read Workload status, ordered pods, events, logs, Argo CD status, and Sandbox commits."""
        gathered_at = self._clock()
        workload = _object(
            await self._tools.read(
                "k8s",
                "resources_get",
                {
                    "apiVersion": "apps/v1",
                    "kind": target.kind,
                    "name": target.name,
                    "namespace": target.namespace,
                },
            )
        )
        selector = _selector(_object(_object(workload.get("spec")).get("selector")))
        pod_list = await self._tools.read_objects(
            "k8s",
            "pods_list_in_namespace",
            {
                "namespace": target.namespace,
                "labelSelector": selector,
            },
        )
        raw_pods = sorted(
            _items(pod_list),
            key=lambda p: (
                _object(p.get("metadata")).get("creationTimestamp", ""),
                _object(p.get("metadata")).get("name", ""),
            ),
        )
        selected = raw_pods[-_MAX_PODS:]
        triggering = next(
            (p for p in raw_pods if _object(p.get("metadata")).get("name") == target.triggering_pod), None
        )
        if triggering is not None and triggering not in selected:
            selected = [triggering, *selected[1:]]
        raw_pods = selected
        pods = tuple(_pod(p) for p in raw_pods)
        pod_names = {p.name for p in pods}
        triggering_pod = target.triggering_pod if target.triggering_pod in pod_names else None

        raw_events = await self._tools.read("k8s", "events_list", {"namespace": target.namespace})
        objects = {f"Pod/{name}" for name in pod_names} | {f"{target.kind}/{target.name}"}
        events = tuple(
            sorted(
                (_event(e) for e in _items(raw_events) if e.get("InvolvedObject") in objects),
                key=lambda e: (e.time, e.object, e.reason),
            )[-20:]
        )

        logs: list[PodLogs] = []
        log_pods = sorted(
            raw_pods,
            key=lambda p: (
                _object(p.get("metadata")).get("name") != target.triggering_pod,
                _object(p.get("metadata")).get("name", ""),
            ),
        )
        for pod in log_pods[:_MAX_LOG_PODS]:
            name = str(_object(pod.get("metadata"))["name"])
            statuses = _object(pod.get("status")).get("containerStatuses", [])
            for container in _object(pod.get("spec")).get("containers", [])[:_MAX_CONTAINERS]:
                container_name = str(container["name"])
                previous = any(
                    s.get("name", container_name) == container_name and s.get("restartCount", 0) > 0
                    for s in statuses
                )
                args = {
                    "namespace": target.namespace,
                    "name": name,
                    "container": container_name,
                    "tail": 50,
                    "previous": previous,
                }
                try:
                    lines = await self._tools.read("k8s", "pods_log", args)
                except ToolPolicyError as error:
                    lines = f"Logs unavailable: {error}"
                logs.append(PodLogs(pod=name, container=container_name, lines=_text(lines)[:_MAX_TEXT]))

        application = _object(await self._tools.read("argocd", "get_application", {}))
        commits = [
            _commit(c) for c in _items(await self._tools.read("github", "list_commits", {"perPage": 20}))
        ]
        commits.sort(key=lambda c: (c.time, c.sha), reverse=True)
        return ContextBundle(
            workload=WorkloadRef(kind=target.kind, namespace=target.namespace, name=target.name),
            gathered_at=gathered_at,
            triggering_pod=triggering_pod,
            workload_status=_text(workload.get("status", {}))[:_MAX_TEXT],
            pods=pods,
            events=events,
            logs=tuple(logs),
            argocd_status=_text(application.get("status", {}))[:_MAX_TEXT],
            commits=tuple(commits[:20]),
        )


def _object(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ContextReadError("a service returned evidence that is not a JSON object")
    return value


def _items(value: Any) -> list[dict[str, Any]]:
    if isinstance(value, dict):
        value = value.get("items", value.get("result", value.get("commits")))
    if not isinstance(value, list) or any(not isinstance(item, dict) for item in value):
        raise ContextReadError("a service returned evidence that is not a JSON list")
    return value


def _time(value: Any) -> datetime:
    if not isinstance(value, str):
        raise ContextReadError("evidence has no timestamp")
    # kubernetes-mcp-server v0.1.3 formats its event rows with Go's time.Time.String().
    # Resource and GitHub timestamps remain RFC3339; preserve offsets in both formats.
    go_time = re.fullmatch(
        r"(\d{4}-\d{2}-\d{2}) (\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?) ([+-]\d{4})(?: [\w:+-]+)?",
        value,
    )
    if go_time:
        value = f"{go_time[1]}T{go_time[2]}{go_time[3]}"
    result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.tzinfo is None:
        raise ContextReadError("evidence timestamp has no timezone")
    return result


def _pod(value: dict[str, Any]) -> PodState:
    metadata = _object(value.get("metadata"))
    status = _object(value.get("status", {}))
    containers = status.get("containerStatuses", [])
    reasons = [
        s.get("lastState", {}).get("terminated", {}).get("reason")
        or s.get("state", {}).get("terminated", {}).get("reason")
        for s in containers
    ]
    return PodState(
        name=metadata["name"],
        created_at=_time(metadata.get("creationTimestamp")),
        phase=status.get("phase", "Unknown"),
        ready=bool(containers) and all(s.get("ready", False) for s in containers),
        restarts=sum(s.get("restartCount", 0) for s in containers),
        last_termination_reason=next((r for r in reasons if r), None),
    )


def _event(value: dict[str, Any]) -> BundleEvent:
    return BundleEvent(
        time=_time(value.get("Timestamp")),
        type=value.get("Type", ""),
        reason=value.get("Reason", ""),
        object=value["InvolvedObject"],
        message=str(value.get("Message", ""))[:1000],
    )


def _commit(value: dict[str, Any]) -> Commit:
    body = _object(value.get("commit"))
    author = _object(body.get("author") or body.get("committer"))
    return Commit(
        sha=value["sha"],
        message=str(body.get("message", ""))[:1000],
        author=str(author.get("name", "")),
        time=_time(author.get("date")),
    )


def _text(value: Any) -> str:
    return value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, sort_keys=True)


def _selector(selector: dict[str, Any]) -> str:
    parts = [f"{key}={value}" for key, value in sorted(selector.get("matchLabels", {}).items())]
    for expression in selector.get("matchExpressions", []):
        key, op = expression["key"], expression["operator"]
        values = ",".join(sorted(expression.get("values", [])))
        if op in {"In", "NotIn"}:
            parts.append(f"{key} {'in' if op == 'In' else 'notin'} ({values})")
        elif op in {"Exists", "DoesNotExist"}:
            parts.append(key if op == "Exists" else f"!{key}")
        else:
            raise ContextReadError(f"unknown Workload selector operator {op!r}")
    if not parts:
        raise ContextReadError("the Workload has no pod selector")
    return ",".join(parts)
