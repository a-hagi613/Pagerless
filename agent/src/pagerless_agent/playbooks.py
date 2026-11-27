"""Optional diagnostic techniques.

Playbooks describe *how to look*, never what the answer is. They are advisory text the
model may request; the available diagnoses come from evidence, not from this list.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Playbook:
    """One technique the model may read."""

    name: str
    summary: str
    body: str


PLAYBOOKS: dict[str, Playbook] = {
    "first-response": Playbook(
        name="first-response",
        summary="Separate the reported symptom from the underlying cause and build a timeline.",
        body=(
            "Start by restating the observable symptom without assuming its cause. Establish when "
            "the behavior began and whether it is continuous or intermittent. Compare the desired "
            "state with the observed state: what should be running, ready, listening or serving, and "
            "what is actually doing so? Read the object's status and conditions before its logs. "
            "Then read the most recent relevant events for ordering. Only after the timeline is "
            "clear should you form competing explanations. State which observation would distinguish "
            "them, and prefer the cheapest read that does so. Treat missing evidence as missing, not "
            "as proof."
        ),
    ),
    "workload-health": Playbook(
        name="workload-health",
        summary="Check scheduling, startup, restarts, probes and resource pressure in that order.",
        body=(
            "For a workload that is not serving, check in this order: whether its pods exist and are "
            "scheduled; whether containers started; whether they restarted and why; whether readiness "
            "and liveness probes currently pass; and whether resource requests or limits are causing "
            "eviction, throttling or termination. Read container statuses and conditions before raw "
            "logs, because they tell you which container and which attempt the logs belong to. Use a "
            "previous-container read only when a restart makes the current log incomplete. Compare "
            "the owning controller's desired replica count with the observed count. Do not infer a "
            "cause from a single restart count; correlate it with events and timing."
        ),
    ),
    "change-review": Playbook(
        name="change-review",
        summary="Correlate a behavior change with the source and rollout that introduced it.",
        body=(
            "When the behavior changed, look for a change that could explain it. Identify the "
            "owning source for the running configuration, verify the repository and path rather than "
            "guessing from a name, and read the base revision you intend to cite. Compare the "
            "rendered or applied object with the source when both are available. Check rollout "
            "history and timestamps, and distinguish a change that was applied from one that is "
            "still pending. A plausible change is not proof: state the mechanism by which it would "
            "produce the symptom and cite the observation that connects them. If the owner or base "
            "revision is ambiguous, stop and explain what is missing instead of proposing an edit."
        ),
    ),
    "open-question": Playbook(
        name="open-question",
        summary="Answer an inventory or capacity question with bounded reads and explicit scope.",
        body=(
            "For a question with no failing target, first define the population the question asks "
            "about and the property it asks for. Read a bounded sample or an explicit inventory, and "
            "say which namespaces and resources the read covered. Distinguish objects that lack the "
            "property from objects you did not read. If the tool result was truncated, say so and "
            "narrow the query rather than generalizing. A useful answer names the affected objects, "
            "the evidence for each, and the limits of the scan. No source change is required for an "
            "informational question; recommend one only if the evidence shows a concrete defect."
        ),
    ),
}


def catalogue() -> tuple[Playbook, ...]:
    """Return the playbooks in a stable order."""
    return tuple(PLAYBOOKS[name] for name in sorted(PLAYBOOKS))


def get(name: str) -> Playbook | None:
    """Return one playbook, or None for an unknown name."""
    return PLAYBOOKS.get(name)
