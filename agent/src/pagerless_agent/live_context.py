"""Gather a bounded Context Bundle through the same read-only MCP boundary as the agent."""

from __future__ import annotations

import json
import logging
import re
from collections.abc import Callable
from datetime import UTC, datetime
from typing import Any

from pagerless_agent.bundle import (
    MAX_EXPERIMENTS,
    BundleEvent,
    Commit,
    ContextBundle,
    Experiment,
    PodLogs,
    PodState,
    WorkloadRef,
)
from pagerless_agent.mcp import MCPTools, ToolPolicyError
from pagerless_agent.settings import Target

log = logging.getLogger(__name__)

_MAX_PODS = 10
_MAX_LOG_PODS = 3
_MAX_CONTAINERS = 3
_MAX_TEXT = 4000
# The Chaos Mesh experiment kinds that can make a Workload fail without any
# defect in its own manifests, so that NoAction is the correct Proposal.
_EXPERIMENT_KINDS = ("StressChaos",)
_EXPERIMENT_API_VERSION = "chaos-mesh.org/v1alpha1"


class ContextReadError(ValueError):
    """Evidence returned by a read-only service is missing or malformed."""


class MCPContextSource:
    """The deterministic, pre-model reads required by spec #1 and ADR-0016."""

    def __init__(self, tools: MCPTools, *, clock: Callable[[], datetime] = lambda: datetime.now(UTC)) -> None:
        self._tools = tools
        self._clock = clock

    async def gather(self, target: Target) -> ContextBundle:
        """Read Workload status, ordered pods, events, logs, Argo CD status, experiments, commits."""
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

        if self._tools.has_server("argocd"):
            try:
                application = _object(await self._tools.read("argocd", "get_application", {}))
                argocd_status = _text(application.get("status", {}))[:_MAX_TEXT]
            except (ToolPolicyError, ContextReadError) as error:
                argocd_status = f"Argo CD status unavailable: {error}"[:_MAX_TEXT]
        else:
            argocd_status = "Argo CD integration is not enabled for this investigation."

        experiments = await self._experiments(target, _pod_labels(workload))

        commits: list[Commit] = []
        if self._tools.has_server("github"):
            try:
                raw_commits = await self._tools.read("github", "list_commits", {"perPage": 20})
                commits = [_commit(c) for c in _items(raw_commits)]
                commits.sort(key=lambda c: (c.time, c.sha), reverse=True)
            except (ToolPolicyError, ContextReadError) as error:
                log.debug("skipping GitHub commit gathering: %s", error)
                commits = []

        return ContextBundle(
            workload=WorkloadRef(kind=target.kind, namespace=target.namespace, name=target.name),
            gathered_at=gathered_at,
            triggering_pod=triggering_pod,
            workload_status=_text(workload.get("status", {}))[:_MAX_TEXT],
            pods=pods,
            events=events,
            logs=tuple(logs),
            argocd_status=argocd_status,
            experiments=experiments,
            commits=tuple(commits[:20]),
        )

    async def _experiments(self, target: Target, labels: dict[str, str]) -> tuple[Experiment, ...]:
        """Read the active experiments that select the Workload's pods.

        Only experiments whose selector admits the Workload's namespace and
        whose label selector is satisfied by the Workload's pod labels are
        reported. An experiment with no label selector selects every pod in the
        namespace, so it matches. Selector fields the agent does not evaluate,
        such as an explicit pod list, never prevent a reported match.
        """
        found: list[Experiment] = []
        for kind in _EXPERIMENT_KINDS:
            try:
                listed = await self._tools.read_objects(
                    "k8s",
                    "resources_list",
                    {"apiVersion": _EXPERIMENT_API_VERSION, "kind": kind, "namespace": target.namespace},
                )
                items = _items(listed)
            except (ToolPolicyError, ContextReadError) as error:
                log.debug("skipping %s experiment discovery in %s: %s", kind, target.namespace, error)
                continue
            for item in items:
                spec = _object(item.get("spec", {}))
                if not _matches(_object(spec.get("selector", {})), target.namespace, labels):
                    continue
                if not _active(item):
                    continue
                found.append(
                    Experiment(
                        name=str(_object(item.get("metadata"))["name"]),
                        kind=kind,
                        target=f"{target.kind} {target.namespace}/{target.name}",
                        duration=_duration(spec.get("duration")),
                    )
                )
        return tuple(found[:MAX_EXPERIMENTS])


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


def _pod_labels(workload: dict[str, Any]) -> dict[str, str]:
    """Return the labels that every pod of the Workload carries."""
    labels: dict[str, str] = {}
    template = _object(_object(workload.get("spec", {})).get("template", {}))
    for source in (
        _object(_object(template.get("metadata", {})).get("labels", {})),
        _object(_object(workload.get("spec", {})).get("selector", {})).get("matchLabels", {}),
    ):
        for key, value in source.items():
            labels[str(key)] = str(value)
    return labels


def _matches(selector: dict[str, Any], namespace: str, labels: dict[str, str]) -> bool:
    """Report whether a Chaos Mesh selector can select the Workload's pods."""
    namespaces = selector.get("namespaces") or []
    if not isinstance(namespaces, list) or any(not isinstance(ns, str) for ns in namespaces):
        raise ContextReadError("an experiment selector has a malformed namespace list")
    if namespaces and namespace not in namespaces:
        return False
    # Chaos Mesh stores the label selector as one map of key to value.
    label_selector = selector.get("labelSelectors") or {}
    if not isinstance(label_selector, dict):
        raise ContextReadError("an experiment selector has a malformed label selector")
    return all(labels.get(str(key)) == str(value) for key, value in label_selector.items())


def _active(experiment: dict[str, Any]) -> bool:
    """Report whether the controller still runs the experiment.

    Chaos Mesh records the injection in the `AllInjected` and `AllRecovered`
    conditions. An experiment whose duration has elapsed reports
    `AllRecovered=True` and is no longer a cause, so it is not evidence.
    """
    status = _object(experiment.get("status", {}))
    desired = _object(status.get("experiment", {})).get("desiredPhase")
    if desired in {"Stop", "Paused"}:
        return False
    for condition in status.get("conditions") or []:
        condition = _object(condition)
        if condition.get("status") == "True" and condition.get("type") in {"AllRecovered", "Paused"}:
            return False
    return True


def _duration(value: Any) -> str:
    """Render an experiment's duration as Chaos Mesh writes it."""
    if value is None or value == "":
        return "none (until the experiment is removed)"
    if not isinstance(value, str):
        raise ContextReadError("an experiment has a malformed duration")
    return value


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
