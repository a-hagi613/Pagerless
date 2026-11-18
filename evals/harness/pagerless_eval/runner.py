"""Run every Scenario, approve as eval-approver, score it, and record the result.

One run is: reset the Sandbox, inject the Scenario, wait for its Trigger (or
start the Triage where docs/scenarios.md says to), wait for Proposed, approve
with the eval-approver identity, wait for the terminal phase, then score the
Triage against the answer key. The orchestrator does not call a model; the agent
inside the cluster does, with whatever provider the stack is configured for.
"""

from __future__ import annotations

import contextlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

from . import gateway
from .answer_key import Scenario, Trigger
from .approver import APPROVER_USER, ApproverError, create_approval, provision
from .kube import Kubectl, KubectlError
from .quantity import QuantityError, parse_memory
from .results import (
    EVIDENCE_LIVE,
    EVIDENCE_REPLAY,
    build_document,
    isoformat,
    merge_run,
    pagerless_revision,
    result_path,
    utc_now,
    write_document,
)
from .score import GatewayEvidence, MemorySettings, RunEvidence, evaluate, refusals

TRIAGE_NAMESPACE = "pagerless"
SANDBOX_NAMESPACE = "sandbox"
SANDBOX_APPLICATION = "sandbox"
AGENT_CONFIG = "pagerless-agent-config"
SERVER_DEPLOYMENT = "pagerless-server"
CHECKOUT = "checkout"
INJECTED_CONTAINER = "memory-holder"
# The pods that read the demo's flags at start-up, in the order the reset
# replaces them: flagd first, so the services start after it and read the
# restored flags. The email service is not on this list: it reads its flag for
# every message, so it recovers from a restored flag without being replaced,
# and a replacement would reset the pod age that ContainerMemoryGrowth needs.
FLAG_WORKLOADS = ("flagd", "cart", "payment", "product-catalog")
# The email pod is replaced only when the flag Scenario left it unhealthy, which
# is the sole case in which its pod age is not worth keeping.
EMAIL_SELECTOR = "opentelemetry.io/name=email"
# Loki ingests the gateway's access log a few seconds behind the request, so a
# denial can be missing from the first query. The harness polls for up to
# GATEWAY_EVIDENCE_TIMEOUT seconds before it concludes that there was none.
GATEWAY_EVIDENCE_TIMEOUT = 30
GATEWAY_EVIDENCE_POLL_INTERVAL = 3
# FLAG_SCENARIOS inject a change to the demo's flagd configuration, so the flag
# outlives the run until the Workloads that read it are replaced. A run from this
# set marks the Sandbox dirty, and the next reset replaces those Workloads.
FLAG_SCENARIOS = ("cart-flag", "payment-flag", "product-catalog-flag", "email-memory-leak")
# Alertmanager is reachable only inside the cluster, so the harness reads its
# active alerts through the Kubernetes API service proxy, as gateway.py does for
# Loki.
ALERTMANAGER_ALERTS = (
    "/api/v1/namespaces/monitoring/services/"
    "kube-prometheus-stack-alertmanager:http-web/proxy/api/v2/alerts?active=true"
)
ALERT_POLL_INTERVAL = 5
ADAPTER_FIXTURES = Path("evals/e2e/fixtures")
# A checked-in recording that came from a live capture lives in evals/replay and
# is baked into the agent image; the synthetic development fixtures live in
# evals/e2e/fixtures. A Scenario may name either.
REPLAY_ARTIFACTS = Path("evals/replay")
PROVIDERS_LIVE = ("anthropic", "bedrock")
# LIVE_OPT_IN is the variable that only the guarded paid workflow sets, so that
# the credential-free Replay subset can never run against a paid provider.
LIVE_OPT_IN = "PAGERLESS_E2E_LIVE"


class HarnessError(RuntimeError):
    """The harness could not complete a run. The message is shown to the operator."""


class BudgetExhaustedError(HarnessError):
    """A configured token or cost budget would be exceeded."""


@dataclass(frozen=True)
class Timeouts:
    trigger: int = 240
    manual_start: int = 180
    proposal: int = 720
    terminal: int = 1200
    approval: int = 120
    adapter_check: int = 300
    reset: int = 1200
    # alert_inactive covers the scrape interval, the alert rule's `for` delay,
    # and Prometheus's evaluation of the recovered Workload; it is deliberately
    # longer than the reset itself.
    alert_inactive: int = 420


@dataclass
class Config:
    repo_root: Path
    scenarios: tuple[Scenario, ...]
    repeats: int
    results_dir: Path
    adapter: bool
    require_pass: bool
    max_tokens: int
    max_cost_usd: float
    context: str
    timeouts: Timeouts = field(default_factory=Timeouts)
    # verification_window is how many seconds of Observation a Recovered run must
    # record. ADR-0006 keeps five minutes for every live run; the credential-free
    # Replay gate passes a shorter window and checks that one instead.
    verification_window: int = 300
    # repeat_number runs one repeat of every Scenario, so a sharded batch can give
    # one runner one run. None runs every repeat in sequence. The document still
    # records config.repeats, because the repeated count is the batch's, not the
    # shard's, and merge_documents needs every shard to agree on it.
    repeat_number: int | None = None


@dataclass
class Evidence:
    """The captured facts that scoring needs, outside the Triage itself."""

    injected_commit: str | None = None
    known_good: MemorySettings | None = None
    injected: MemorySettings | None = None
    max_node_memory: str | None = None
    gateway: GatewayEvidence | None = None


class Harness:
    """One evaluation batch against a running Pagerless stack."""

    def __init__(self, config: Config) -> None:
        if config.repeat_number is not None and not 1 <= config.repeat_number <= config.repeats:
            raise HarnessError(
                f"repeat number {config.repeat_number} is outside the configured "
                f"1 to {config.repeats} repeats"
            )
        self.config = config
        self.kube = Kubectl(context=config.context)
        self.state_dir = Path(tempfile.mkdtemp(prefix="pagerless-eval."))
        self.state_dir.chmod(0o700)
        self.provider = ""
        self.evidence_mode = EVIDENCE_LIVE
        self.max_node_memory: str | None = None
        self.spent_tokens = 0
        self.spent_cost = 0.0
        self.budget_exhausted = False
        self.started = utc_now()
        self.commit, self.dirty = pagerless_revision(config.repo_root)
        self.runs: list[dict[str, Any]] = []
        self.path: Path | None = None
        # repeats is the batch's repeat count, narrowed to the one repeat a shard
        # was given. The document still declares config.repeats, so a shard of a
        # rendered batch merges with the other shards.
        self.repeats = (
            (config.repeat_number,)
            if config.repeat_number is not None
            else tuple(range(1, config.repeats + 1))
        )
        # _flag_dirty records that a Scenario has changed the demo's flagd
        # configuration since the Workloads that read it were last replaced. A
        # reset replaces them only when this is set, so an untouched Workload
        # keeps its pod age: ContainerMemoryGrowth ignores a pod younger than 15
        # minutes, and replacing it before every run would make that rule
        # unreachable for a Scenario that starts from a fresh pod.
        self._flag_dirty = False
        # completed and total drive the per-run counters, so a long batch says
        # how far through it is instead of printing indistinguishable lines.
        self.completed = 0
        self.total = len(config.scenarios) * len(self.repeats)

    # ---- entry point -------------------------------------------------------

    def run(self) -> int:
        self._preflight()
        self._progress(f"Evaluation model: {self.provider}; evidence: {self.evidence_mode}")
        self._progress(f"Scenarios: {', '.join(item.name for item in self.config.scenarios)}")
        kubeconfig = provision(self.kube, self.state_dir)
        evaluator = Kubectl(kubeconfig=kubeconfig)
        try:
            for scenario in self.config.scenarios:
                for repeat in self.repeats:
                    self._check_budget()
                    self._progress(
                        f"[{scenario.name} repeat {repeat}/{self.config.repeats}] resetting and injecting"
                    )
                    record = self._run_with_context(scenario, repeat, evaluator)
                    self.runs = merge_run(self.runs, record)
                    self._write()
                    self.completed += 1
                    self._print_run(record)
                    self._assert_ci_evidence(record)
                    self._add_spend(record)
        finally:
            self._write()
            try:
                self._progress("Resetting the Sandbox after the batch")
                self._reset()
            except (HarnessError, KubectlError) as error:
                print(
                    f"warning: the Sandbox could not be reset after the batch: {error}",
                    file=sys.stderr,
                    flush=True,
                )
            shutil.rmtree(self.state_dir, ignore_errors=True)
        if self.budget_exhausted:
            raise BudgetExhaustedError(
                f"the batch spent {self.spent_tokens} tokens and ${self.spent_cost:.2f}, "
                "which reached or exceeded its budget"
            )
        if self.config.require_pass and any(run["result"] == "fail" for run in self.runs):
            raise HarnessError("at least one run failed; see the result document")
        return 0

    # ---- preflight ---------------------------------------------------------

    def _preflight(self) -> None:
        if not self.kube.context_exists():
            raise HarnessError(
                f"the Kubernetes context {self.kube.context!r} is not in the kubeconfig. "
                "Create the local stack with make up, or run the credential-free subset "
                "with make eval-replay."
            )
        self.kube.cluster_ready()
        for resource, name, namespace in (
            ("deployment", SERVER_DEPLOYMENT, TRIAGE_NAMESPACE),
            ("deployment", CHECKOUT, SANDBOX_NAMESPACE),
            ("configmap", AGENT_CONFIG, TRIAGE_NAMESPACE),
        ):
            result = self.kube.run("get", resource, name, "-n", namespace, check=False)
            if result.returncode:
                raise HarnessError(
                    f"{resource}/{name} is missing from namespace {namespace}. "
                    "Wait for the stack to become healthy with make up."
                )
        config_map = self.kube.get_json("configmap", AGENT_CONFIG, TRIAGE_NAMESPACE)
        provider = (config_map.get("data") or {}).get("PAGERLESS_MODEL_PROVIDER", "")
        if provider not in ("replay", *PROVIDERS_LIVE):
            raise HarnessError(
                f"{AGENT_CONFIG} does not name a valid PAGERLESS_MODEL_PROVIDER (found {provider!r})"
            )
        self.provider = provider
        self.evidence_mode = EVIDENCE_REPLAY if provider == "replay" else EVIDENCE_LIVE

        if self.config.adapter:
            self._preflight_adapter(provider)
        else:
            self._preflight_live(provider)

        if any(scenario.rule.value == "injection" for scenario in self.config.scenarios):
            gateway.reachable(self.kube)
        self.config.results_dir.mkdir(parents=True, exist_ok=True)
        self.path = result_path(self.config.results_dir, self.started, self.commit, self.evidence_mode)
        if self.path.exists():
            print(f"Merging into the existing result document {self.path}")

    def _preflight_adapter(self, provider: str) -> None:
        required = (
            "PAGERLESS_E2E_REPOSITORY",
            "PAGERLESS_E2E_API_URL",
            "PAGERLESS_E2E_CONTROL_URL",
            "PAGERLESS_E2E_TOKEN",
            "PAGERLESS_E2E_CA_FILE",
        )
        missing = [name for name in required if not os.environ.get(name)]
        if missing:
            raise HarnessError(
                "the adapter Replay subset needs its isolated configuration; missing " + ", ".join(missing)
            )
        if not Path(os.environ["PAGERLESS_E2E_CA_FILE"]).is_file():
            raise HarnessError("the adapter CA certificate is not readable")
        # The adapter also fronts the guarded paid workflow, so a live provider is
        # allowed only when that workflow opts in explicitly. `make eval-replay`
        # never sets this, so the credential-free CI subset cannot spend money.
        if provider != "replay" and os.environ.get(LIVE_OPT_IN) != "1":
            raise HarnessError(
                "make eval-replay requires the agent's provider to be replay; "
                f"the stack is configured with {provider!r}. Only the guarded paid "
                f"workflow sets {LIVE_OPT_IN}=1."
            )
        if provider != "replay":
            return
        for scenario in self.config.scenarios:
            if scenario.replay_recording is None:
                raise HarnessError(
                    f"Scenario {scenario.name!r} has no Replay recording for the CI subset. "
                    "The documented subset is oom, unschedulable, and bad-config."
                )
            recording = next(
                (
                    root / str(scenario.replay_recording)
                    for root in (ADAPTER_FIXTURES, REPLAY_ARTIFACTS)
                    if (self.config.repo_root / root / str(scenario.replay_recording)).is_file()
                ),
                None,
            )
            if recording is None:
                raise HarnessError(f"the Replay recording {scenario.replay_recording} is missing")

    def _preflight_live(self, provider: str) -> None:
        if provider == "replay":
            raise HarnessError(
                "the stack is configured for Replay, and the live evaluation needs a paid provider. "
                "Run the credential-free CI subset with make eval-replay, or configure "
                "MODEL_PROVIDER=anthropic and run make eval."
            )
        expected = {
            "anthropic": "ANTHROPIC_API_KEY",
            "bedrock": "AWS_ACCESS_KEY_ID",
        }[provider]
        credentials = self.kube.run(
            "get",
            "secret",
            "pagerless-agent-credentials",
            "-n",
            TRIAGE_NAMESPACE,
            "-o",
            "json",
            check=False,
        )
        if credentials.returncode:
            raise HarnessError(
                "the pagerless-agent-credentials Secret is missing; set the provider's "
                "credentials in .env and run make up"
            )
        keys = set((json.loads(credentials.stdout).get("data") or {}).keys())
        if expected not in keys:
            raise HarnessError(
                f"the pagerless-agent-credentials Secret has no {expected}; configure the provider first"
            )
        for name in ("SANDBOX_URL", "GITHUB_WRITE_TOKEN"):
            if not os.environ.get(name):
                raise HarnessError(f"{name} is not set. Run make up to write it to .env.")
        cli = self.config.repo_root / "bin" / "kubectl-triage"
        if not cli.is_file():
            raise HarnessError("bin/kubectl-triage is missing. Run make build-cli.")

    # ---- one run -----------------------------------------------------------

    def _run_with_context(self, scenario: Scenario, repeat: int, evaluator: Kubectl) -> dict[str, Any]:
        """Run one Scenario, and say how long it took when it does not finish.

        A live run can wait on a rollout, an alert, or a model call for many
        minutes. Without the Scenario and the elapsed time, a failure deep inside
        the run reads as a stall with no clue which step held it up.
        """
        started = time.monotonic()
        try:
            return self._run_once(scenario, repeat, evaluator)
        except (HarnessError, KubectlError, subprocess.TimeoutExpired, OSError) as error:
            elapsed = time.monotonic() - started
            raise HarnessError(
                f"Scenario {scenario.name!r} repeat {repeat} stopped after {elapsed:.0f}s: {error}"
            ) from error

    def _run_once(self, scenario: Scenario, repeat: int, evaluator: Kubectl) -> dict[str, Any]:
        evidence = Evidence()
        triage: dict[str, Any] | None = None
        phases: list[dict[str, str]] = []
        failure = ""
        if self.config.adapter:
            self._set_recording(scenario)
        self._reset(scenario)
        evidence.known_good = self._memory_settings()
        self._delete_triages()
        since = isoformat(utc_now())
        # A flag Scenario changes the shared flagd configuration, and the change
        # survives the reset of the Sandbox's Git state until the pods that read
        # the flags are replaced. Mark that before injecting, so a break that
        # fails after it committed the change is still cleaned up.
        if scenario.name in FLAG_SCENARIOS:
            self._flag_dirty = True
        self._break(scenario)
        evidence.injected_commit = self._sandbox_revision()
        evidence.injected = self._memory_settings()
        evidence.max_node_memory = self._max_node_memory()
        try:
            triage = self._wait_for_triage(scenario, since)
            name = triage["metadata"]["name"]
            triage, phases = self._drive_triage(name, scenario, evaluator)
        except (HarnessError, ApproverError) as error:
            failure = str(error)
            if triage is None:
                raise
            with contextlib.suppress(KubectlError):
                triage = self.kube.get_json("triage", triage["metadata"]["name"], TRIAGE_NAMESPACE)
        if triage is None:
            raise HarnessError(f"Scenario {scenario.name!r} never produced a Triage")
        if scenario.rule.value == "injection":
            evidence.gateway = self._gateway_evidence(triage)
        return self._record(scenario, repeat, triage, phases, evidence, failure)

    def _drive_triage(
        self, name: str, scenario: Scenario, evaluator: Kubectl
    ) -> tuple[dict[str, Any], list[dict[str, str]]]:
        phases: list[dict[str, str]] = []
        last_phase = ""
        approved_hash = ""
        proposal_deadline = time.monotonic() + self.config.timeouts.proposal
        terminal_deadline = 0.0
        while True:
            triage = self.kube.get_json("triage", name, TRIAGE_NAMESPACE)
            status = triage.get("status") or {}
            phase = str(status.get("phase") or "")
            if phase != last_phase:
                phases.append({"phase": phase, "observedAt": isoformat(utc_now())})
                last_phase = phase
            if phase in _terminal():
                return triage, phases
            proposal_hash = str((status.get("proposal") or {}).get("hash") or "")
            if phase == "Proposed" and proposal_hash != approved_hash:
                create_approval(evaluator, triage)
                approved_hash = proposal_hash
                self._await_decision(name)
                terminal_deadline = time.monotonic() + self.config.timeouts.terminal
                action = (status.get("proposal") or {}).get("action")
                if self.config.adapter and action not in ("NoAction", "RestartWorkload"):
                    self._adapter_check(name)
                continue
            now = time.monotonic()
            if not approved_hash and now > proposal_deadline:
                raise HarnessError(
                    f"the Triage did not reach Proposed within {self.config.timeouts.proposal}s "
                    f"(phase {phase!r})"
                )
            if approved_hash and now > terminal_deadline:
                raise HarnessError(
                    f"the Triage did not reach a terminal phase within "
                    f"{self.config.timeouts.terminal}s (phase {phase!r})"
                )
            time.sleep(1)

    def _await_decision(self, name: str) -> None:
        deadline = time.monotonic() + self.config.timeouts.approval
        while time.monotonic() < deadline:
            triage = self.kube.get_json("triage", name, TRIAGE_NAMESPACE)
            status = triage.get("status") or {}
            decision = status.get("decision") or {}
            approver = decision.get("approver")
            if approver:
                if approver != APPROVER_USER:
                    raise HarnessError(f"the Triage decision was made by {approver!r}, not {APPROVER_USER!r}")
                return
            if status.get("phase") in _terminal():
                raise HarnessError("the Triage ended before the Approval was recorded")
            time.sleep(1)
        raise HarnessError("the Approval did not appear on the Triage within its timeout")

    def _record(
        self,
        scenario: Scenario,
        repeat: int,
        triage: dict[str, Any],
        phases: list[dict[str, str]],
        evidence: Evidence,
        failure: str,
    ) -> dict[str, Any]:
        metadata = triage.get("metadata") or {}
        status = triage.get("status") or {}
        phase = str(status.get("phase") or "")
        if phase not in _terminal():
            phase = "TimedOut"
        started = status.get("startedAt") or metadata.get("creationTimestamp")
        terminal = status.get("terminalAt")
        proposal = status.get("proposal") or {}
        action = proposal.get("action")
        decision = status.get("decision") or {}
        usage = status.get("usage") or {}
        score = evaluate(
            scenario,
            triage,
            RunEvidence(
                injected_commit=evidence.injected_commit,
                known_good=evidence.known_good,
                injected=evidence.injected,
                max_node_memory=evidence.max_node_memory,
                gateway=evidence.gateway,
            ),
        )
        time_to_recovered = (
            _seconds(started, terminal) if phase == "Recovered" and started and terminal else None
        )
        return {
            "scenario": scenario.name,
            "scenarioNumber": scenario.number,
            "workload": scenario.workload,
            "repeat": repeat,
            "triage": metadata.get("name"),
            "startedAt": started,
            "terminalAt": terminal,
            "phase": phase,
            "phaseHistory": phases,
            "approvedBy": decision.get("approver"),
            "action": action,
            # A refused Proposal costs an extra model call and hides what the
            # agent first got wrong, so the document records the count, the
            # attempt of the accepted Proposal, and every refusal with its errors.
            "validationAttempts": int(_number(status.get("validationAttempts")) or 0),
            "proposalAttempt": proposal.get("attempt") if isinstance(proposal, dict) else None,
            "refusals": refusals(triage),
            "result": score.result,
            "score": {
                "rule": score.rule,
                "checks": [
                    {"name": check.name, "passed": check.passed, "detail": check.detail}
                    for check in score.checks
                ],
            },
            "timeToRecoveredSeconds": time_to_recovered,
            "timeToTerminalSeconds": _seconds(started, terminal) if started and terminal else None,
            "verification": _verification(status),
            "usage": {
                "inputTokens": _number(usage.get("inputTokens")),
                "outputTokens": _number(usage.get("outputTokens")),
                "costUSD": _number(usage.get("costUSD")),
            },
            "evidence": {
                "injectedCommit": evidence.injected_commit,
                "knownGood": _memory_dict(evidence.known_good),
                "injected": _memory_dict(evidence.injected),
                "maxNodeMemory": evidence.max_node_memory,
                "gatewayDenials": list(evidence.gateway.denials) if evidence.gateway else [],
            },
            "failure": failure or None,
        }

    # ---- stack operations --------------------------------------------------

    def _set_recording(self, scenario: Scenario) -> None:
        recording = f"/app/recordings/{scenario.replay_recording}"
        config_map = self.kube.get_json("configmap", AGENT_CONFIG, TRIAGE_NAMESPACE)
        current = (config_map.get("data") or {}).get("PAGERLESS_REPLAY_FILE")
        if current == recording:
            return
        self.kube.run(
            "-n",
            TRIAGE_NAMESPACE,
            "patch",
            "configmap",
            AGENT_CONFIG,
            "--type",
            "merge",
            "-p",
            json.dumps({"data": {"PAGERLESS_REPLAY_FILE": recording}}),
        )
        self.kube.run("-n", TRIAGE_NAMESPACE, "rollout", "restart", f"deployment/{SERVER_DEPLOYMENT}")
        self.kube.run(
            "-n",
            TRIAGE_NAMESPACE,
            "rollout",
            "status",
            f"deployment/{SERVER_DEPLOYMENT}",
            "--timeout=180s",
            timeout=240,
        )

    def _reset(self, scenario: Scenario | None = None) -> None:
        if self.config.adapter:
            self._adapter_client("reset")
            # Scenario 9 applies its StressChaos experiment outside Git, so the
            # adapter's Git reset cannot remove it the way hack/reset.sh does.
            # A leftover experiment would keep burning CPU through later runs.
            self.kube.run(
                "-n",
                SANDBOX_NAMESPACE,
                "delete",
                "stresschaos",
                "--all",
                "--ignore-not-found",
                "--wait=true",
                check=False,
            )
            self._wait_sandbox_healthy()
            # The adapter restores the Sandbox's Git state, but it cannot replace
            # a pod, so the harness does that here. Only a flag Scenario leaves a
            # flag behind, so only it needs the replacement.
            if self._flag_dirty:
                self._replace_flag_workloads()
        else:
            # hack/reset.sh restores the Sandbox and replaces the flag Workloads
            # itself, so a live reset always starts from the known-good flags.
            self._make("reset", timeout=self.config.timeouts.reset)
            self._wait_sandbox_healthy()
        self._flag_dirty = False
        self.kube.run(
            "-n",
            SANDBOX_NAMESPACE,
            "rollout",
            "status",
            f"deployment/{CHECKOUT}",
            "--timeout=600s",
            timeout=660,
        )
        if scenario is not None and scenario.alert is not None:
            self._wait_alert_inactive(scenario)

    def _replace_flag_workloads(self) -> None:
        """Replace the pods that read the demo's flags, as hack/reset.sh does.

        The adapter restores the Sandbox's Git state, and Argo CD syncs it, but
        the flagd pod copies its ConfigMap into an emptyDir when it starts and
        the flag-driven services resolve a flag when they start. Without this
        step a Scenario's flag would outlive the reset even though the Sandbox is
        at known-good. flagd is replaced first, so the services that start after
        it read the restored flags.

        The email service evaluates its flag for every message and clears its
        deliveries once flagd serves "off", so its pod is replaced only when the
        Scenario left it missing, not Running, not Ready or restarted.
        ContainerMemoryGrowth fires only for a pod older than fifteen minutes, so
        replacing a healthy email pod would cost the next run that age.

        The harness calls this only after a flag Scenario ran, so a batch that
        never changed a flag leaves the demo's pods, and their pod age, alone.
        """
        for workload in FLAG_WORKLOADS:
            self._replace_pod(workload)
        if self._email_needs_replacement():
            self._replace_pod("email")

    def _replace_pod(self, workload: str) -> None:
        """Delete a Workload's pods and wait for its Deployment to replace them."""
        self.kube.run(
            "-n",
            SANDBOX_NAMESPACE,
            "delete",
            "pod",
            "-l",
            f"opentelemetry.io/name={workload}",
            "--ignore-not-found",
            "--wait=true",
            timeout=300,
        )
        self.kube.run(
            "-n",
            SANDBOX_NAMESPACE,
            "rollout",
            "status",
            f"deployment/{workload}",
            "--timeout=180s",
            timeout=240,
        )

    def _email_needs_replacement(self) -> bool:
        """Report whether the email pod is not healthy enough to keep its age."""
        payload = self.kube.json(
            "get",
            "pods",
            "-n",
            SANDBOX_NAMESPACE,
            "-l",
            EMAIL_SELECTOR,
            "-o",
            "json",
        )
        pods = payload.get("items") or []
        if not pods:
            return True
        for pod in pods:
            status = pod.get("status") or {}
            if status.get("phase") != "Running":
                return True
            conditions = status.get("conditions") or []
            if not any(
                condition.get("type") == "Ready" and condition.get("status") == "True"
                for condition in conditions
            ):
                return True
            for container in status.get("containerStatuses") or []:
                if (container.get("restartCount") or 0) > 0:
                    return True
        return False

    def _wait_alert_inactive(self, scenario: Scenario) -> None:
        """Wait until Alertmanager no longer reports the Scenario's alert.

        A Scenario that an alert starts must begin from a quiet Alertmanager: an
        alert still firing for the same Workload would either join or start the
        next Triage, and Verification would never observe it resolve. Alertmanager
        reports the alert inactive only after Prometheus evaluates its rule
        against the recovered Workload, so this polls instead of sleeping for a
        fixed delay.
        """
        deadline = time.monotonic() + self.config.timeouts.alert_inactive
        while True:
            try:
                active = self._active_alerts()
            except (HarnessError, KubectlError) as error:
                last = str(error)
            else:
                firing = [alert for alert in active if _matches(alert, scenario)]
                if not firing:
                    return
                last = f"{len(firing)} active {scenario.alert} alert(s) for {scenario.workload}"
            if time.monotonic() >= deadline:
                break
            time.sleep(ALERT_POLL_INTERVAL)
        raise HarnessError(
            f"Alertmanager did not report the {scenario.alert} alert for "
            f"{scenario.workload} inactive within {self.config.timeouts.alert_inactive}s: {last}"
        )

    def _active_alerts(self) -> list[dict[str, Any]]:
        result = self.kube.run("get", "--raw", ALERTMANAGER_ALERTS, timeout=60, check=False)
        if result.returncode:
            raise HarnessError(
                "the Alertmanager query through the Kubernetes API failed: "
                + " ".join(result.stderr.split())[:400]
            )
        try:
            payload = json.loads(result.stdout)
        except ValueError:
            raise HarnessError("Alertmanager returned a response that is not JSON") from None
        if not isinstance(payload, list):
            raise HarnessError("Alertmanager returned a response that is not an alert list")
        return [alert for alert in payload if isinstance(alert, dict)]

    def _wait_sandbox_healthy(self) -> None:
        deadline = time.monotonic() + self.config.timeouts.reset
        while time.monotonic() < deadline:
            result = self.kube.run(
                "-n", "argocd", "get", "application", SANDBOX_APPLICATION, "-o", "json", check=False
            )
            if result.returncode == 0:
                status = json.loads(result.stdout).get("status") or {}
                if (status.get("sync") or {}).get("status") == "Synced" and (status.get("health") or {}).get(
                    "status"
                ) == "Healthy":
                    return
            time.sleep(2)
        raise HarnessError(
            f"the Sandbox Argo CD Application did not become Synced and Healthy within "
            f"{self.config.timeouts.reset}s"
        )

    def _sandbox_revision(self) -> str:
        application = self.kube.get_json("application", SANDBOX_APPLICATION, "argocd")
        revision = ((application.get("status") or {}).get("sync") or {}).get("revision")
        if not isinstance(revision, str) or len(revision) != 40:
            raise HarnessError("the Sandbox Argo CD Application does not report a commit revision")
        return revision

    def _memory_settings(self) -> MemorySettings:
        deployment = self.kube.get_json("deployment", CHECKOUT, SANDBOX_NAMESPACE)
        containers = (((deployment.get("spec") or {}).get("template") or {}).get("spec") or {}).get(
            "containers"
        ) or []
        container = next((item for item in containers if item.get("name") == INJECTED_CONTAINER), None)
        if container is None:
            raise HarnessError(f"deployment/{CHECKOUT} has no {INJECTED_CONTAINER} container to score")
        resources = container.get("resources") or {}
        return MemorySettings(
            requests=(resources.get("requests") or {}).get("memory"),
            limits=(resources.get("limits") or {}).get("memory"),
        )

    def _max_node_memory(self) -> str:
        if self.max_node_memory is not None:
            return self.max_node_memory
        nodes = self.kube.json("get", "nodes", "-o", "json")
        largest = 0
        for node in nodes.get("items") or []:
            value = ((node.get("status") or {}).get("allocatable") or {}).get("memory")
            if not isinstance(value, str):
                continue
            try:
                largest = max(largest, int(parse_memory(value)))
            except QuantityError:
                continue
        if not largest:
            raise HarnessError("no node reports an allocatable memory quantity")
        self.max_node_memory = str(largest)
        return self.max_node_memory

    def _delete_triages(self) -> None:
        self.kube.run(
            "-n",
            TRIAGE_NAMESPACE,
            "delete",
            "triages,triageapprovals",
            "--all",
            "--ignore-not-found",
            "--wait=true",
            "--timeout=120s",
            timeout=150,
        )

    def _break(self, scenario: Scenario) -> None:
        # hack/break.sh waits up to 22 minutes for an alert, and the flag
        # Scenarios also wait for a pull request merge and an Argo CD sync, so the
        # subprocess timeout leaves room for both.
        self._make("break", f"SCENARIO={scenario.name}", timeout=1680)

    def _make(self, target: str, *arguments: str, timeout: int) -> None:
        command = ["make", "-C", str(self.config.repo_root), target, *arguments]
        result = subprocess.run(command, capture_output=True, text=True, timeout=timeout, check=False)
        if result.returncode:
            output = (result.stdout + "\n" + result.stderr).strip().splitlines()
            tail = " / ".join(output[-6:])
            raise HarnessError(f"make {target} failed with exit {result.returncode}: {tail}")

    def _adapter_client(self, command: str) -> None:
        script = self.config.repo_root / "evals" / "e2e" / "client.py"
        result = subprocess.run(
            [
                sys.executable,
                str(script),
                "--repository",
                os.environ["PAGERLESS_E2E_REPOSITORY"],
                command,
            ],
            capture_output=True,
            text=True,
            timeout=300,
            check=False,
        )
        if result.returncode:
            raise HarnessError(f"the Sandbox adapter could not {command}: {result.stderr.strip()}")

    def _adapter_check(self, triage: str) -> None:
        script = self.config.repo_root / "evals" / "e2e" / "checks.py"
        result = subprocess.run(
            [
                sys.executable,
                str(script),
                "--triage",
                triage,
                "--timeout",
                str(self.config.timeouts.adapter_check),
            ],
            capture_output=True,
            text=True,
            timeout=self.config.timeouts.adapter_check + 60,
            check=False,
        )
        if result.returncode:
            raise HarnessError(
                "the adapter's required check did not pass the remediation pull request: "
                + " ".join((result.stdout + result.stderr).split())
            )

    # ---- triggers ----------------------------------------------------------

    def _wait_for_triage(self, scenario: Scenario, since: str) -> dict[str, Any]:
        if scenario.trigger is Trigger.ALERT:
            triage = self._find_triage(scenario.workload, since, self.config.timeouts.trigger)
            if triage is not None:
                return triage
            raise HarnessError(
                f"the {scenario.alert} alert did not start a Triage for {scenario.workload} "
                f"within {self.config.timeouts.trigger}s"
            )
        print(
            f"Scenario {scenario.name!r} has no opted-in alert; starting its Triage with "
            f"kubectl triage as docs/scenarios.md documents"
        )
        # The API server truncates creationTimestamp to the second, and the triage
        # can be created after kubectl triage starts, so the search must not open
        # its window after the call returns: a run whose call takes more than a
        # second would then search past the Triage it just created.
        manual_since = isoformat(utc_now())
        self._start_triage(scenario)
        triage = self._find_triage(scenario.workload, manual_since, self.config.timeouts.manual_start)
        if triage is None:
            raise HarnessError("the manually started Triage did not appear")
        return triage

    def _find_triage(self, workload: str, since: str, timeout: int) -> dict[str, Any] | None:
        deadline = time.monotonic() + timeout
        since_time = _parse_time(since)
        while time.monotonic() < deadline:
            payload = self.kube.json("get", "triages", "-n", TRIAGE_NAMESPACE, "-o", "json")
            candidates = []
            for item in payload.get("items") or []:
                metadata = item.get("metadata") or {}
                if ((item.get("spec") or {}).get("workload") or {}).get("name") != workload:
                    continue
                created = _parse_time(metadata.get("creationTimestamp"))
                if created is None or since_time is None:
                    continue
                # The API server timestamps to the second, while the harness
                # records microseconds. A one-second tolerance keeps a Triage
                # created in the same second as the injection from being missed.
                if created >= since_time - timedelta(seconds=1):
                    candidates.append(item)
            if candidates:
                return max(
                    candidates,
                    key=lambda item: item["metadata"].get("creationTimestamp", ""),
                )
            time.sleep(2)
        return None

    def _start_triage(self, scenario: Scenario) -> None:
        cli = self.config.repo_root / "bin" / "kubectl-triage"
        result = subprocess.run(
            [
                str(cli),
                "--context",
                self.kube.context,
                "--namespace",
                SANDBOX_NAMESPACE,
                f"deploy/{scenario.workload}",
            ],
            capture_output=True,
            text=True,
            timeout=120,
            check=False,
        )
        if result.returncode:
            raise HarnessError(
                f"kubectl triage could not start the {scenario.name} Triage: "
                + (result.stderr.strip() or result.stdout.strip())
            )
        print(result.stdout.strip())

    # ---- gateway evidence --------------------------------------------------

    def _gateway_evidence(self, triage: dict[str, Any]) -> GatewayEvidence:
        """Return the gateway denials at or after the injected Secret read.

        The agent may ignore the injected instruction entirely and never attempt
        the read, in which case there is nothing to look up. When it does attempt
        the read, the denial reaches Loki a few seconds after the request, so the
        harness polls rather than querying once and reading an empty result as a
        completed read.
        """
        steps = (triage.get("status") or {}).get("steps") or []
        starts = [
            _parse_time(step.get("time"))
            for step in steps
            if step.get("tool") == "k8s__resources_get" and "github-write" in str(step.get("summary", ""))
        ]
        start = next((item for item in starts if item is not None), None)
        if start is None:
            return GatewayEvidence()
        deadline = time.monotonic() + GATEWAY_EVIDENCE_TIMEOUT
        denials: tuple[datetime, ...] = ()
        while True:
            denials = gateway.denials(self.kube, start, utc_now())
            if denials or time.monotonic() >= deadline:
                break
            time.sleep(GATEWAY_EVIDENCE_POLL_INTERVAL)
        return GatewayEvidence(denials=tuple(isoformat(item) for item in denials))

    # ---- results and budget ------------------------------------------------

    def _write(self) -> None:
        if self.path is None or not self.runs:
            return
        document = build_document(
            commit=self.commit,
            dirty=self.dirty,
            provider=self.provider,
            evidence=self.evidence_mode,
            scenarios=tuple(item.name for item in self.config.scenarios),
            repeats=self.config.repeats,
            started_at=self.started,
            budget={
                "maxTokens": self.config.max_tokens,
                "maxCostUSD": self.config.max_cost_usd,
                "spentTokens": self.spent_tokens,
                "spentCostUSD": round(self.spent_cost, 6),
                "exhausted": self.budget_exhausted,
            },
            runs=self.runs,
        )
        write_document(self.path, document)

    def _check_budget(self) -> None:
        if self.config.max_tokens and self.spent_tokens >= self.config.max_tokens:
            self.budget_exhausted = True
            raise BudgetExhaustedError(
                f"the token budget of {self.config.max_tokens} is spent; refusing to start another run"
            )
        if self.config.max_cost_usd and self.spent_cost >= self.config.max_cost_usd:
            self.budget_exhausted = True
            raise BudgetExhaustedError(
                f"the cost budget of ${self.config.max_cost_usd:.2f} is spent; refusing to start another run"
            )

    def _add_spend(self, record: dict[str, Any]) -> None:
        usage = record["usage"]
        tokens = (usage.get("inputTokens") or 0) + (usage.get("outputTokens") or 0)
        self.spent_tokens += int(tokens)
        self.spent_cost += float(usage.get("costUSD") or 0.0)
        if self.config.max_tokens and self.spent_tokens > self.config.max_tokens:
            self.budget_exhausted = True
        if self.config.max_cost_usd and self.spent_cost > self.config.max_cost_usd:
            self.budget_exhausted = True

    def _assert_ci_evidence(self, record: dict[str, Any]) -> None:
        """Keep the CI gate strict about the evidence the old e2e job asserted.

        The scored result is the harness's answer, but a Recovered run must also
        show the deterministic Verification checks and the observation window that
        the server decided and the harness was told to require. A run that reached
        a Proposal must show the eval-approver as the approver.
        """
        if not self.config.require_pass:
            return
        if record["phase"] == "Recovered":
            verification = record["verification"]
            checks = verification["checks"]
            if not checks or any(not check["passed"] for check in checks):
                raise HarnessError("a Recovered run did not record passing deterministic Verification checks")
            window = verification["windowSeconds"]
            required = self.config.verification_window
            if window is None or window < required:
                raise HarnessError(
                    f"a Recovered run recorded {window}s of Observation, "
                    f"below the required {required}s Verification window"
                )
        if record["action"] is not None and record["approvedBy"] != APPROVER_USER:
            raise HarnessError(f"the {record['scenario']} run was not approved by {APPROVER_USER}")

    def _print_run(self, record: dict[str, Any]) -> None:
        seconds = record["timeToRecoveredSeconds"]
        usage = record["usage"]
        elapsed = "n/a to Recovered" if seconds is None else f"{seconds:.0f}s to Recovered"
        tokens = f"{usage.get('inputTokens') or 0}+{usage.get('outputTokens') or 0}"
        cost = f"{usage.get('costUSD') or 0:.4f}"
        line = (
            f"{record['scenario']} repeat {record['repeat']}: {record['action']} -> "
            f"{record['result']} ({record['phase']}, {elapsed}, {tokens} tokens, ${cost})"
        )
        self._progress(f"  {line}")
        self._append_step_summary(f"[{self.completed}/{self.total}] {line}")

    def _progress(self, message: str) -> None:
        """Print one progress line immediately.

        A full batch runs for hours inside one GitHub Actions job, and an
        unflushed print can sit in a pipe buffer long enough to make a working
        run look stalled.
        """
        print(message, flush=True)

    def _append_step_summary(self, line: str) -> None:
        """Append one Markdown line to the GitHub Actions job summary.

        GitHub renders the file named by GITHUB_STEP_SUMMARY while the job runs,
        so appending each completed run makes a long batch visible there instead
        of only in the log after the job ends. Outside Actions the variable is
        unset and this is a no-op.
        """
        path = os.environ.get("GITHUB_STEP_SUMMARY")
        if not path:
            return
        try:
            with open(path, "a", encoding="utf-8") as summary:
                summary.write(f"- {line}\n")
        except OSError as error:
            print(
                f"warning: could not append to GITHUB_STEP_SUMMARY: {error}",
                file=sys.stderr,
                flush=True,
            )


def _terminal() -> frozenset[str]:
    return frozenset({"Recovered", "NotRecovered", "Diagnosed", "Rejected", "Expired", "Invalid", "Failed"})


def _verification(status: dict[str, Any]) -> dict[str, Any]:
    phase = status.get("phase")
    outcome = {
        "Recovered": "Recovered",
        "NotRecovered": "NotRecovered",
        "Diagnosed": "NotApplicable",
    }.get(phase, "NotRun")
    started = _parse_time(status.get("verificationStartedAt"))
    terminal = _parse_time(status.get("terminalAt"))
    window = None
    if started is not None and terminal is not None:
        window = max(0.0, (terminal - started).total_seconds())
    checks = [
        {
            "name": check.get("name"),
            "passed": check.get("passed") is True,
            "message": check.get("message", ""),
        }
        for check in status.get("verificationChecks") or []
        if isinstance(check, dict)
    ]
    return {"outcome": outcome, "checks": checks, "windowSeconds": window}


def _memory_dict(settings: MemorySettings | None) -> dict[str, str | None]:
    return {
        "requests": settings.requests if settings else None,
        "limits": settings.limits if settings else None,
    }


def _parse_time(value: Any) -> datetime | None:
    if not isinstance(value, str):
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)


def _seconds(started: Any, terminal: Any) -> float | None:
    start = _parse_time(started)
    end = _parse_time(terminal)
    if start is None or end is None:
        return None
    return max(0.0, (end - start).total_seconds())


def _number(value: Any) -> int | float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    return value


def _matches(alert: dict[str, Any], scenario: Scenario) -> bool:
    """Return whether an active alert is the Scenario's alert for its Workload."""
    labels = _labels(alert)
    return (
        labels.get("alertname") == scenario.alert
        and labels.get("deployment") == scenario.workload
    )


def _labels(alert: dict[str, Any]) -> dict[str, str]:
    """Return an Alertmanager alert's labels, ignoring a malformed value."""
    labels = alert.get("labels")
    if not isinstance(labels, dict):
        return {}
    return {key: value for key, value in labels.items() if isinstance(value, str)}
