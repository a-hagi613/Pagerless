"""The Context Bundle: the evidence gathered by deterministic code before the first model call."""

from __future__ import annotations

from datetime import datetime
from typing import Literal

from pydantic import AwareDatetime, BaseModel, ConfigDict, model_validator

# Limits on what is shown to the model, which keep the bundle small (ADR-0010).
MAX_RENDERED_EVENTS = 20
MAX_RENDERED_LOG_LINES = 50
MAX_RENDERED_COMMITS = 20


class _Frozen(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


class WorkloadRef(_Frozen):
    kind: Literal["Deployment", "StatefulSet"]
    namespace: str
    name: str


class PodState(_Frozen):
    name: str
    phase: str
    ready: bool
    restarts: int
    last_termination_reason: str | None = None
    created_at: AwareDatetime


class BundleEvent(_Frozen):
    time: AwareDatetime
    type: str
    reason: str
    object: str
    message: str


class PodLogs(_Frozen):
    pod: str
    container: str
    lines: str


class Commit(_Frozen):
    sha: str
    message: str
    author: str
    time: AwareDatetime


class ContextBundle(_Frozen):
    """The fixed set of evidence gathered before the agent's first model call.

    `gathered_at` is the reference time of the investigation. Replay resolves
    recorded timestamps relative to it. Pods are kept in creation order and
    commits newest first, so that an index into either list names the same
    entity in every run of the same Scenario.
    """

    workload: WorkloadRef
    gathered_at: AwareDatetime
    triggering_pod: str | None = None
    workload_status: str
    pods: tuple[PodState, ...]
    events: tuple[BundleEvent, ...]
    logs: tuple[PodLogs, ...]
    argocd_status: str
    commits: tuple[Commit, ...]

    @model_validator(mode="before")
    @classmethod
    def _canonical_order(cls, data: object) -> object:
        if isinstance(data, dict) and data.get("pods") is not None:
            pods = [PodState.model_validate(p) if isinstance(p, dict) else p for p in data["pods"]]
            data = {**data, "pods": tuple(sorted(pods, key=lambda p: (p.created_at, p.name)))}
        return data

    def render(self) -> str:
        """Return the bundle as the text that the model reads."""
        w = self.workload
        out = [
            f"Workload: {w.kind} {w.namespace}/{w.name}",
            f"Gathered at: {_iso(self.gathered_at)}",
        ]
        if self.triggering_pod:
            out.append(f"Triggering pod: {self.triggering_pod}")
        out += ["", "## Workload status", self.workload_status, "", "## Pods"]
        for pod in self.pods:
            reason = (
                f", last termination: {pod.last_termination_reason}" if pod.last_termination_reason else ""
            )
            out.append(
                f"- {pod.name}: phase {pod.phase}, ready {str(pod.ready).lower()}, "
                f"restarts {pod.restarts}{reason}, created {_iso(pod.created_at)}"
            )
        out += ["", "## Events (most recent last)"]
        for event in self.events[-MAX_RENDERED_EVENTS:]:
            out.append(f"- {_iso(event.time)} {event.type} {event.reason} {event.object}: {event.message}")
        out += ["", "## Recent logs"]
        for logs in self.logs:
            lines = logs.lines.splitlines()[-MAX_RENDERED_LOG_LINES:]
            out.append(f"### {logs.pod} / {logs.container}")
            out.extend(lines)
        out += ["", "## Argo CD status", self.argocd_status, "", "## Recent Sandbox commits (newest first)"]
        for commit in self.commits[:MAX_RENDERED_COMMITS]:
            out.append(f"- {commit.sha} {_iso(commit.time)} {commit.author}: {commit.message}")
        return "\n".join(out)


def _iso(value: datetime) -> str:
    return value.isoformat().replace("+00:00", "Z")
