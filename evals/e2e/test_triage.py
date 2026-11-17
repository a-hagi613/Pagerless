"""Regression checks for the Triage watch stream's buffered line handling."""

import unittest

from triage import complete_lines


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


if __name__ == "__main__":
    unittest.main()