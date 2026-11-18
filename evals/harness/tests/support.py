"""Builders shared by the evaluation-harness tests."""

from __future__ import annotations

from datetime import datetime
from typing import Any

from pagerless_eval.results import build_document

HASH = "ab" * 32
STARTED = "2026-11-17T12:00:00Z"
TERMINAL = "2026-11-17T12:10:00Z"
VERIFICATION_STARTED = "2026-11-17T12:05:00Z"


def triage(
    action: str | None = None,
    parameters: dict[str, Any] | None = None,
    *,
    phase: str = "Recovered",
    diagnosis: str = "",
    steps: list[dict[str, Any]] | None = None,
    validation_attempts: int = 0,
    refusals: list[str] | None = None,
    name: str = "triage-eval",
    uid: str = "uid-1",
    namespace: str = "pagerless",
    started: str = STARTED,
    terminal: str = TERMINAL,
    verification_started: str = VERIFICATION_STARTED,
    usage: dict[str, Any] | None = None,
    checks: list[dict[str, Any]] | None = None,
    decision: str | None = "eval-approver",
) -> dict[str, Any]:
    status: dict[str, Any] = {
        "phase": phase,
        "startedAt": started,
        "terminalAt": terminal,
        "verificationStartedAt": verification_started,
        "validationAttempts": validation_attempts,
        "steps": steps or [],
        "usage": usage or {"inputTokens": 1000, "outputTokens": 100, "costUSD": 0.002},
        "verificationChecks": checks
        or [
            {"name": "ArgoCDSynced", "passed": True},
            {"name": "RolloutAvailable", "passed": True},
            {"name": "NoNewRestarts", "passed": True},
            {"name": "AlertResolved", "passed": True},
        ],
    }
    if refusals:
        # Each value is one line of the ProposalRefused condition, in the format
        # internal/reportapi writes: "attempt N: <Action> <parameters> refused: <errors>".
        status["conditions"] = [
            {
                "type": "ProposalRefused",
                "status": "True",
                "reason": "ValidationRefused",
                "message": "\n".join(refusals),
                "lastTransitionTime": STARTED,
            }
        ]
    if diagnosis:
        status["diagnosis"] = diagnosis
    if decision:
        status["decision"] = {
            "outcome": "Approved",
            "approver": decision,
            "decidedAt": VERIFICATION_STARTED,
        }
    if action is not None:
        status["proposal"] = {"action": action, "hash": HASH}
        if parameters is not None:
            status["proposal"]["parameters"] = parameters
    return {
        "apiVersion": "pagerless.dev/v1alpha1",
        "kind": "Triage",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "uid": uid,
            "creationTimestamp": "2026-11-17T11:59:00Z",
        },
        "spec": {"workload": {"kind": "Deployment", "namespace": "sandbox", "name": "checkout"}},
        "status": status,
    }


def run(*, scenario: str = "oom", number: int = 1, repeat: int = 1, result: str = "pass") -> dict[str, Any]:
    return {
        "scenario": scenario,
        "scenarioNumber": number,
        "workload": "checkout",
        "repeat": repeat,
        "triage": "triage-eval",
        "startedAt": STARTED,
        "terminalAt": TERMINAL,
        "phase": "Recovered",
        "phaseHistory": [{"phase": "Proposed", "observedAt": STARTED}],
        "approvedBy": "eval-approver",
        "action": "PatchResources",
        "validationAttempts": 0,
        "proposalAttempt": 1,
        "refusals": [],
        "result": result,
        "score": {
            "rule": "patch-resources-oom",
            "checks": [{"name": "expectedAction", "passed": result == "pass", "detail": ""}],
        },
        "timeToRecoveredSeconds": 600.0,
        "timeToTerminalSeconds": 600.0,
        "verification": {"outcome": "Recovered", "checks": [], "windowSeconds": 300.0},
        "usage": {"inputTokens": 1000, "outputTokens": 100, "costUSD": 0.002},
        "evidence": {},
        "failure": None,
    }


def document(
    runs: list[dict[str, Any]] | None = None,
    *,
    provider: str = "anthropic",
    evidence: str = "live",
    dirty: bool = False,
) -> dict[str, Any]:
    return build_document(
        commit="f" * 40,
        dirty=dirty,
        provider=provider,
        evidence=evidence,
        scenarios=("oom",),
        repeats=1,
        started_at=datetime.fromisoformat(STARTED.replace("Z", "+00:00")),
        budget={
            "maxTokens": 0,
            "maxCostUSD": 0.0,
            "spentTokens": 1100,
            "spentCostUSD": 0.002,
            "exhausted": False,
        },
        runs=runs if runs is not None else [run()],
    )
