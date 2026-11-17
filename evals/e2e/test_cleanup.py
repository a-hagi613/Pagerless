"""Exercise failure cleanup without contacting Kubernetes or Docker."""

import os
import subprocess
import tempfile
import unittest
from pathlib import Path


class CleanupTests(unittest.TestCase):
    def run_cleanup(self, *, keep=0, created=0, restore_fails=0):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            state = root / "private state"
            state.mkdir()
            log = root / "calls"
            restore = root / "restore.sh"
            restore.write_text('echo restore >>"$CLEANUP_LOG"\nexit "$RESTORE_FAILS"\n')
            cleanup = str(Path(__file__).with_name("cleanup.sh"))
            script = r"""
source "$1"
state="$2"; script_dir="$3"; keep="$4"; created="$5"
state_is_temporary=1; policies_saved=1; KIND_CLUSTER_NAME=pagerless
kind() { echo delete >>"$CLEANUP_LOG"; }
sleep 30 & observer=$!
cleanup_e2e 7
result=$?
if kill -0 "$observer" 2>/dev/null; then echo observer-alive; fi
echo result=$result
test ! -d "$state" || echo state-retained
"""
            result = subprocess.run(
                [
                    "bash",
                    "-c",
                    script,
                    "cleanup-test",
                    cleanup,
                    str(state),
                    str(root),
                    str(keep),
                    str(created),
                ],
                env=os.environ | {"CLEANUP_LOG": str(log), "RESTORE_FAILS": str(restore_fails)},
                check=True,
                capture_output=True,
                text=True,
                timeout=10,
            )
            self.assertNotIn("observer-alive", result.stdout)
            calls = log.read_text().splitlines() if log.exists() else []
            return result.stdout, calls

    def test_failed_reuse_restores_automation_before_discarding_recovery_state(self):
        output, calls = self.run_cleanup()
        self.assertEqual(calls, ["restore"])
        self.assertIn("result=7", output)
        self.assertNotIn("state-retained", output)

    def test_failed_restore_preserves_recovery_files(self):
        output, calls = self.run_cleanup(restore_fails=1)
        self.assertEqual(calls, ["restore"])
        self.assertIn("result=1", output)
        self.assertIn("state-retained", output)

    def test_new_cluster_is_deleted_and_retained_mode_changes_neither_cluster_nor_policy(
        self,
    ):
        output, calls = self.run_cleanup(created=1)
        self.assertEqual(calls, ["delete"])
        self.assertNotIn("state-retained", output)
        output, calls = self.run_cleanup(keep=1)
        self.assertEqual(calls, [])
        self.assertIn("state-retained", output)


if __name__ == "__main__":
    unittest.main()
