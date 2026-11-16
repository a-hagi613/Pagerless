"""Builders shared by the agent tests."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import UTC, datetime, timedelta

from pagerless_agent.bundle import BundleEvent, Commit, ContextBundle, PodLogs, PodState, WorkloadRef
from pagerless_agent.model import ModelResponse
from tests.fakes import call, respond


@dataclass(frozen=True)
class Run:
    """The volatile identifiers of one run of the OOMKilled Scenario."""

    pod: str
    replicaset: str
    old_pod: str
    commits: tuple[str, str]
    gathered_at: datetime


# The same Scenario, run twice. Every volatile identifier differs between the runs.
RECORDED = Run(
    pod="checkout-7d9f8b6c5-x2k4q",
    replicaset="checkout-7d9f8b6c5",
    old_pod="checkout-5f6d7c8b9-t8vwz",
    commits=("3f2a9c1d8e7b6a5f4c3d2e1f0a9b8c7d6e5f4a3b", "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"),
    gathered_at=datetime(2026, 11, 16, 10, 30, 15, 250_000, tzinfo=UTC),
)
REPLAYED = Run(
    pod="checkout-6b8c4d5f7-h9m2z",
    replicaset="checkout-6b8c4d5f7",
    old_pod="checkout-9wxzr2k4q-b5n7p",
    commits=("c0ffee11d00d22beef33cafe44babe55f00d6677", "0123456789abcdef0123456789abcdef01234567"),
    gathered_at=datetime(2026, 12, 2, 4, 5, 6, 789_000, tzinfo=UTC),
)


def make_bundle(
    run: Run = RECORDED, *, with_old_pod: bool = False, namespace: str = "sandbox"
) -> ContextBundle:
    """Return the Context Bundle that deterministic code would gather for a run."""
    at = run.gathered_at
    pods = [
        PodState(
            name=run.pod,
            phase="Running",
            ready=False,
            restarts=4,
            last_termination_reason="OOMKilled",
            created_at=at - timedelta(minutes=10),
        )
    ]
    if with_old_pod:
        pods.append(
            PodState(
                name=run.old_pod,
                phase="Succeeded",
                ready=False,
                restarts=0,
                created_at=at - timedelta(days=1),
            )
        )
    return ContextBundle(
        workload=WorkloadRef(kind="Deployment", namespace=namespace, name="checkout"),
        gathered_at=at,
        triggering_pod=run.pod,
        workload_status="replicas 1, ready 0, unavailable 1",
        pods=tuple(pods),
        events=(
            BundleEvent(
                time=at - timedelta(minutes=2),
                type="Warning",
                reason="BackOff",
                object=f"pod/{run.pod}",
                message="Back-off restarting failed container",
            ),
        ),
        logs=(PodLogs(pod=run.pod, container="app", lines="allocating 512 MiB\nKilled"),),
        argocd_status="Synced, Degraded",
        commits=(
            Commit(
                sha=run.commits[0],
                message="Lower the checkout memory limit",
                author="dev",
                time=at - timedelta(hours=1),
            ),
            Commit(sha=run.commits[1], message="Add checkout", author="dev", time=at - timedelta(days=3)),
        ),
    )


def oom_script(run: Run = RECORDED) -> list[ModelResponse]:
    """The model responses of a live run of the OOMKilled Scenario, with the run's own identifiers."""
    event_time = (run.gathered_at - timedelta(minutes=2)).isoformat().replace("+00:00", "Z")
    return [
        respond(
            call("toolu_01", "pods_log", namespace="sandbox", name=run.pod, previous=True),
            text=f"Reading the previous logs of {run.pod}.",
        ),
        respond(
            call("toolu_02", "resources_get", kind="Deployment", namespace="sandbox", name="checkout"),
            call(
                "toolu_03",
                "record_finding",
                kind="Event",
                source=f"events: pod/{run.pod}",
                summary=f"{run.pod} was OOMKilled at {event_time}",
            ),
        ),
        respond(text="The investigation is complete."),
        respond(
            structured={
                "diagnosis": (
                    f"The container of {run.pod} exceeds its 128Mi memory limit and is OOMKilled. "
                    f"Commit {run.commits[0][:7]} lowered the limit."
                ),
                "findings": [
                    {
                        "kind": "Log",
                        "source": f"pods_log sandbox/{run.pod} (previous)",
                        "summary": "The process allocated 512 MiB and was killed.",
                    },
                    {
                        "kind": "Commit",
                        "source": f"sandbox commit {run.commits[0]}",
                        "summary": "Lower the checkout memory limit",
                    },
                ],
            },
        ),
        respond(
            structured={
                "action": "PatchResources",
                "parameters": {"patchResources": {"container": "app", "limits": {"memory": "512Mi"}}},
            },
        ),
    ]
