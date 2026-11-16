"""The agent's environment, as the server's Job sets it."""

from __future__ import annotations

from pathlib import Path

import pytest

from pagerless_agent.settings import ConfigError, Settings, Target

ENV = {
    "PAGERLESS_SERVER_URL": "http://pagerless-server.pagerless.svc:8443",
    "PAGERLESS_TOKEN_FILE": "/var/run/pagerless/token",
    "PAGERLESS_TRIAGE": "checkout-abc12",
    "PAGERLESS_WORKLOAD_KIND": "Deployment",
    "PAGERLESS_WORKLOAD_NAMESPACE": "sandbox",
    "PAGERLESS_WORKLOAD_NAME": "checkout",
}


def test_the_settings_are_read_from_the_environment_the_job_sets() -> None:
    env = {
        **ENV,
        "PAGERLESS_TRIGGERING_POD": "checkout-7d9f8b6c5-x2k4q",
        "PAGERLESS_HINT": "after the deploy",
    }
    assert Settings.from_env(env) == Settings(
        server_url="http://pagerless-server.pagerless.svc:8443",
        token_file=Path("/var/run/pagerless/token"),
        target=Target(
            triage="checkout-abc12",
            kind="Deployment",
            namespace="sandbox",
            name="checkout",
            triggering_pod="checkout-7d9f8b6c5-x2k4q",
            hint="after the deploy",
        ),
    )


def test_the_pod_and_the_hint_are_optional() -> None:
    target = Settings.from_env(ENV).target
    assert target.triggering_pod is None
    assert target.hint is None


def test_every_missing_variable_is_named() -> None:
    env = {k: v for k, v in ENV.items() if k not in ("PAGERLESS_TRIAGE", "PAGERLESS_TOKEN_FILE")}
    with pytest.raises(ConfigError, match="PAGERLESS_TOKEN_FILE, PAGERLESS_TRIAGE"):
        Settings.from_env(env)


def test_a_workload_kind_that_is_not_a_workload_is_refused() -> None:
    with pytest.raises(ConfigError, match="Deployment or StatefulSet"):
        Settings.from_env({**ENV, "PAGERLESS_WORKLOAD_KIND": "Pod"})
