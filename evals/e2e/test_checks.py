"""Behavioral checks for the host's exact-head manifest-check workflow."""

import base64
import json
import unittest
from unittest.mock import patch

from checks import check_remediation, validate_head

HEAD = "a" * 40
MERGE = "b" * 40
ENVIRONMENT = {
    "PAGERLESS_E2E_REPOSITORY": "pagerless-test/sandbox",
    "PAGERLESS_E2E_API_URL": "https://localhost:18443",
    "PAGERLESS_E2E_CONTROL_URL": "http://127.0.0.1:18080",
    "PAGERLESS_E2E_GIT_URL": "https://localhost:18443/git/pagerless-test/sandbox.git",
    "PAGERLESS_E2E_TOKEN": "e2e-write-disposable",
    "GITHUB_READ_TOKEN": "ghp_e2e-disposable",
    "PAGERLESS_E2E_CA_FILE": "/private/ca.crt",
    "PAGERLESS_E2E_KUBECONFORM_BIN": "/private/kubeconform",
}
PULL_REQUEST = {
    "head": {"ref": "pagerless/recovery", "sha": HEAD},
    "state": "open",
    "auto_merge": {"merge_method": "SQUASH"},
}


def successful_command(arguments, **_kwargs):
    if arguments[0] == "kubectl":
        return json.dumps(
            {
                "status": {
                    "phase": "Executing",
                    "execution": {
                        "pullRequestURL": "https://github.com/pagerless-test/sandbox/pull/2",
                    },
                }
            }
        )
    if arguments[-1] == "-v":
        return "v0.8.0"
    if arguments[-2:] == ["rev-parse", "HEAD"]:
        return HEAD
    return ""


class RequiredCheckTests(unittest.TestCase):
    @patch("checks.command", side_effect=successful_command)
    @patch("checks.request", side_effect=[PULL_REQUEST, {"merge_commit_sha": MERGE}])
    def test_validates_the_exact_head_with_read_auth_before_signaling_success(self, request, command):
        self.assertEqual(check_remediation("triage-test", 10, ENVIRONMENT), MERGE)
        self.assertEqual(request.call_args_list[0].args[1], ENVIRONMENT["GITHUB_READ_TOKEN"])
        self.assertEqual(request.call_args_list[-1].args[-1], {"status": "success", "sha": HEAD})
        self.assertEqual(request.call_args_list[-1].args[0], ENVIRONMENT["PAGERLESS_E2E_CONTROL_URL"])
        calls = command.call_args_list
        self.assertTrue(any(call.args[0][-2:] == ["origin", HEAD] for call in calls))
        strict = next(call for call in calls if "-strict" in call.args[0])
        self.assertIn("-kubernetes-version", strict.args[0])
        self.assertTrue(strict.args[0][-1].endswith("/workloads"))
        clone = next(call for call in calls if "clone" in call.args[0])
        git_env = clone.kwargs["env"]
        self.assertEqual(git_env["GIT_SSL_CAINFO"], ENVIRONMENT["PAGERLESS_E2E_CA_FILE"])
        auth = git_env["GIT_CONFIG_VALUE_0"].removeprefix("Authorization: Basic ")
        self.assertEqual(base64.b64decode(auth).decode(), "pagerless:ghp_e2e-disposable")
        self.assertNotIn("e2e-write-disposable", " ".join(clone.args[0]))

    @patch("checks.request", return_value=PULL_REQUEST)
    def test_a_failed_manifest_check_never_reports_success(self, request):
        def fail_manifests(arguments, **kwargs):
            if "-strict" in arguments:
                raise RuntimeError("the required manifest check failed")
            return successful_command(arguments, **kwargs)

        with (
            patch("checks.command", side_effect=fail_manifests),
            self.assertRaisesRegex(RuntimeError, "manifest check failed"),
        ):
            check_remediation("triage-test", 10, ENVIRONMENT)
        self.assertEqual(request.call_count, 1)
        self.assertEqual(request.call_args.args[2], "GET")

    def test_checks_cannot_clone_a_remote_repository_or_use_a_real_read_token(self):
        for key, value in (
            ("PAGERLESS_E2E_GIT_URL", "https://github.com/pagerless-test/sandbox.git"),
            ("GITHUB_READ_TOKEN", "github_pat_example"),
        ):
            environment = ENVIRONMENT | {key: value}
            with self.subTest(key=key), patch("checks.command") as command:
                with self.assertRaises(RuntimeError):
                    validate_head(PULL_REQUEST, environment)
                command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
