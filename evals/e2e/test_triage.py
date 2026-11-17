"""Regression checks for the Triage watch stream's buffered line handling."""

import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from datetime import UTC, datetime, timedelta
from pathlib import Path
from subprocess import CompletedProcess
from unittest.mock import patch

from triage import complete_lines, verify


class CompleteLinesTests(unittest.TestCase):
    def test_drains_all_complete_events_and_keeps_partial_record(self) -> None:
        pending = bytearray(b"triage-a checkout Proposed\ntriage-a checkout Approved\ntriage-a che")

        lines = complete_lines(pending)

        self.assertEqual(lines, [b"triage-a checkout Proposed", b"triage-a checkout Approved"])
        self.assertEqual(pending, bytearray(b"triage-a che"))

        pending.extend(b"ckout Verifying\ntriage-a checkout Recovered\n")
        self.assertEqual(
            complete_lines(pending),
            [b"triage-a checkout Verifying", b"triage-a checkout Recovered"],
        )
        self.assertEqual(pending, bytearray())

    def test_verification_window_tolerates_different_watch_receipt_delays(self) -> None:
        started = datetime.now(UTC) - timedelta(seconds=301)
        phases = [
            "Pending",
            "Investigating",
            "Proposed",
            "Approved",
            "Executing",
            "Verifying",
            "Recovered",
        ]
        events = [{"phase": phase, "observedAt": started.isoformat()} for phase in phases]
        events[-2]["observedAt"] = (started + timedelta(seconds=0.5)).isoformat()
        events[-1]["observedAt"] = (started + timedelta(seconds=300.1)).isoformat()
        triage = {
            "metadata": {"name": "triage-test"},
            "status": {
                "phase": "Recovered",
                "proposal": {"action": "PatchResources"},
                "execution": {"mergeCommit": "merge-sha"},
                "verificationStartedAt": started.isoformat(),
                "verificationChecks": [
                    {"name": name, "passed": True}
                    for name in ("ArgoCDSynced", "RolloutAvailable", "NoNewRestarts", "AlertResolved")
                ],
            },
        }
        application = {
            "status": {
                "sync": {"revision": "merge-sha", "status": "Synced"},
                "health": {"status": "Healthy"},
            }
        }
        with tempfile.TemporaryDirectory() as directory:
            snapshot, history = (
                Path(directory) / "triage.json",
                Path(directory) / "history",
            )
            snapshot.write_text(json.dumps(triage))
            history.write_text("\n".join(json.dumps(event) for event in events))
            result = CompletedProcess([], 0, stdout=json.dumps(application))
            with patch("triage.subprocess.run", return_value=result):
                verify(snapshot, history)

            events[-1]["observedAt"] = (started + timedelta(seconds=299.9)).isoformat()
            history.write_text("\n".join(json.dumps(event) for event in events))
            with (
                patch("triage.subprocess.run", return_value=result),
                self.assertRaisesRegex(RuntimeError, "Verification window"),
            ):
                verify(snapshot, history)

    def test_terminating_the_observer_also_reaps_its_kubectl_watch(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            child_pid = root / "watch.pid"
            kubectl = root / "kubectl"
            kubectl.write_text('#!/usr/bin/env bash\necho $$ >"$WATCH_PID_FILE"\nexec sleep 30\n')
            kubectl.chmod(0o700)
            observer = subprocess.Popen(
                [
                    sys.executable,
                    str(Path(__file__).with_name("triage.py")),
                    "collect",
                    "--history",
                    str(root / "history"),
                    "--snapshot",
                    str(root / "snapshot"),
                    "--timeout",
                    "30",
                ],
                env=os.environ | {"PATH": f"{root}:{os.environ['PATH']}", "WATCH_PID_FILE": str(child_pid)},
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            try:
                deadline = time.monotonic() + 5
                while not child_pid.exists() and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertTrue(child_pid.exists(), "the observer did not start its Kubernetes watch")
                watcher = int(child_pid.read_text())
                observer.terminate()
                observer.wait(timeout=5)
                with self.assertRaises(ProcessLookupError):
                    os.kill(watcher, 0)
            finally:
                if observer.poll() is None:
                    observer.kill()
                    observer.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()
