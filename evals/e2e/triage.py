"""Observe and validate the real Triage created by the kind integration run."""

from __future__ import annotations

import argparse
import json
import os
import selectors
import signal
import subprocess
import sys
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any


def triage_snapshot() -> dict[str, Any] | None:
    result = subprocess.run(
        [
            "kubectl",
            "--context",
            "kind-pagerless",
            "-n",
            "pagerless",
            "get",
            "triages",
            "-o",
            "json",
        ],
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode:
        raise RuntimeError("kubectl could not read Triages")
    payload = json.loads(result.stdout)
    items = [
        item
        for item in payload.get("items", [])
        if item.get("spec", {}).get("workload", {}).get("name") == "checkout"
    ]
    if not items:
        return None
    return max(items, key=lambda item: item.get("metadata", {}).get("creationTimestamp", ""))


def complete_lines(buffer: bytearray) -> list[bytes]:
    lines = []
    while (newline := buffer.find(b"\n")) >= 0:
        lines.append(bytes(buffer[:newline]))
        del buffer[: newline + 1]
    return lines


def collect(history: Path, snapshot: Path, timeout: int) -> None:
    command = [
        "kubectl",
        "--context",
        "kind-pagerless",
        "-n",
        "pagerless",
        "get",
        "triages",
        "--watch",
        "--no-headers",
        "-o",
        "custom-columns=NAME:.metadata.name,WORKLOAD:.spec.workload.name,PHASE:.status.phase",
    ]
    try:
        watcher = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=0)
    except OSError as error:
        raise RuntimeError("could not start the Triage watch") from error

    def terminate(_number: int, _frame: Any) -> None:
        raise SystemExit(143)

    previous = signal.signal(signal.SIGTERM, terminate)
    deadline = time.monotonic() + timeout
    last_phase = ""
    try:
        if watcher.stdout is None:
            raise RuntimeError("kubectl did not provide a Triage watch stream")
        with selectors.DefaultSelector() as selector:
            selector.register(watcher.stdout, selectors.EVENT_READ)
            pending = bytearray()
            while time.monotonic() < deadline:
                if not selector.select(timeout=min(1, deadline - time.monotonic())):
                    if watcher.poll() is not None:
                        raise RuntimeError("the Kubernetes Triage watch exited unexpectedly")
                    continue
                chunk = os.read(watcher.stdout.fileno(), 65536)
                if not chunk:
                    if watcher.poll() is not None:
                        raise RuntimeError("the Kubernetes Triage watch exited unexpectedly")
                    continue
                pending.extend(chunk)
                for raw_line in complete_lines(pending):
                    line = raw_line.decode("utf-8", errors="replace")
                    fields = line.split()
                    if len(fields) < 3 or fields[1] != "checkout":
                        continue
                    phase = fields[-1]
                    if phase and phase != last_phase:
                        observed = datetime.now(UTC)
                        current = triage_snapshot()
                        if current is not None:
                            snapshot.write_text(json.dumps(current), encoding="utf-8")
                        event = {"phase": phase, "observedAt": observed.isoformat()}
                        with history.open("a", encoding="utf-8") as output:
                            output.write(json.dumps(event) + "\n")
                        last_phase = phase
                    if phase == "Recovered":
                        return
                    if phase in {
                        "NotRecovered",
                        "Failed",
                        "Invalid",
                        "Expired",
                        "Rejected",
                    }:
                        raise RuntimeError(f"the checkout Triage ended in {phase}")
        raise RuntimeError("the checkout Triage did not reach Recovered before timeout")
    finally:
        signal.signal(signal.SIGTERM, previous)
        watcher.terminate()
        try:
            watcher.wait(timeout=5)
        except subprocess.TimeoutExpired:
            watcher.kill()
            watcher.wait(timeout=5)


def wait_proposed(timeout: int) -> str:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        current = triage_snapshot()
        if current is not None:
            phase = current.get("status", {}).get("phase")
            if phase == "Proposed":
                return current["metadata"]["name"]
            if phase in {"Failed", "Invalid", "Expired", "Rejected", "NotRecovered"}:
                raise RuntimeError(f"the checkout Triage ended in {phase} before Approval")
        time.sleep(2)
    raise RuntimeError("the checkout Triage did not reach Proposed before timeout")


def verify(snapshot: Path, history: Path) -> None:
    if not snapshot.exists():
        raise RuntimeError("the Triage observer did not record a snapshot")
    triage = json.loads(snapshot.read_text(encoding="utf-8"))
    status = triage.get("status", {})
    if status.get("phase") != "Recovered":
        raise RuntimeError(f"the final Triage phase is {status.get('phase')!r}, not Recovered")
    if (status.get("proposal") or {}).get("action") != "PatchResources":
        raise RuntimeError("the agent did not propose PatchResources")
    events = [json.loads(line) for line in history.read_text(encoding="utf-8").splitlines()]
    phases = [event["phase"] for event in events]
    required = [
        "Pending",
        "Investigating",
        "Proposed",
        "Approved",
        "Executing",
        "Verifying",
        "Recovered",
    ]
    cursor = 0
    for phase in phases:
        if cursor < len(required) and phase == required[cursor]:
            cursor += 1
    if cursor != len(required):
        raise RuntimeError(f"phase history missed {required[cursor:]}; observed {phases}")
    execution = status.get("execution") or {}
    merge_commit = execution.get("mergeCommit")
    if not merge_commit:
        raise RuntimeError("the Triage has no Sandbox merge commit")
    argo = subprocess.run(
        [
            "kubectl",
            "--context",
            "kind-pagerless",
            "-n",
            "argocd",
            "get",
            "application",
            "sandbox",
            "-o",
            "json",
        ],
        check=False,
        capture_output=True,
        text=True,
    )
    if argo.returncode:
        raise RuntimeError("kubectl could not read the Sandbox Argo CD Application")
    app = json.loads(argo.stdout)
    revision = app.get("status", {}).get("sync", {}).get("revision")
    sync = app.get("status", {}).get("sync", {}).get("status")
    health = app.get("status", {}).get("health", {}).get("status")
    if (revision, sync, health) != (merge_commit, "Synced", "Healthy"):
        raise RuntimeError(
            f"Sandbox Argo CD is {sync}/{health} at {revision}, expected Synced/Healthy at {merge_commit}"
        )
    started = status.get("verificationStartedAt")
    if not started:
        raise RuntimeError("the Triage has no Verification start timestamp")
    start_time = datetime.fromisoformat(started.replace("Z", "+00:00"))
    elapsed = (datetime.now(UTC) - start_time).total_seconds()
    if elapsed < 300:
        raise RuntimeError(f"Verification ended after {elapsed:.1f}s; at least 300s is required")
    verifying_event = next((event for event in events if event["phase"] == "Verifying"), None)
    recovered_event = next((event for event in events if event["phase"] == "Recovered"), None)
    if not verifying_event or not recovered_event:
        raise RuntimeError("the observer did not promptly record Verifying and Recovered")
    verify_observed = datetime.fromisoformat(verifying_event["observedAt"])
    recovered_observed = datetime.fromisoformat(recovered_event["observedAt"])
    if abs((verify_observed - start_time).total_seconds()) > 60:
        raise RuntimeError("the Verifying phase was not observed near its server start timestamp")
    # Receipt delays differ between events. The server timestamp gives a stable,
    # one-sided check without rejecting a valid window when Verifying arrives late.
    observed_window = (recovered_observed - start_time).total_seconds()
    if observed_window < 300:
        raise RuntimeError(
            f"the observed Verification window was {observed_window:.1f}s; at least 300s is required"
        )
    checks = status.get("verificationChecks", [])
    expected_checks = {"ArgoCDSynced", "RolloutAvailable", "NoNewRestarts", "AlertResolved"}
    if (
        len(checks) != len(expected_checks)
        or {check.get("name") for check in checks} != expected_checks
        or any(check.get("passed") is not True for check in checks)
    ):
        raise RuntimeError(f"the deterministic Verification checks did not all pass: {checks}")
    print(triage["metadata"]["name"])


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("collect", "proposed", "verify"))
    parser.add_argument("--history", type=Path, default=Path("phase-history.txt"))
    parser.add_argument("--snapshot", type=Path, default=Path("triage.json"))
    parser.add_argument("--timeout", type=int, default=900)
    args = parser.parse_args()
    if args.command == "collect":
        collect(args.history, args.snapshot, args.timeout)
    elif args.command == "proposed":
        print(wait_proposed(args.timeout))
    else:
        verify(args.snapshot, args.history)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, json.JSONDecodeError, KeyError, TypeError) as error:
        print(f"Triage E2E failed: {error}", file=sys.stderr)
        raise SystemExit(1) from None
