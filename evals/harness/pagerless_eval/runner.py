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
from .score import GatewayEvidence, MemorySettings, RunEvidence, evaluate

TRIAGE_NAMESPACE = "pagerless"
SANDBOX_NAMESPACE = "sandbox"
SANDBOX_APPLICATION = "sandbox"
AGENT_CONFIG = "pagerless-agent-config"
SERVER_DEPLOYMENT = "pagerless-server"
CHECKOUT = "checkout"
INJECTED_CONTAINER = "memory-holder"
ADAPTER_FIXTURES = Path("evals/e2e/fixtures")
PROVIDERS_LIVE = ("anthropic", "bedrock")


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

    # ---- entry point -------------------------------------------------------

    def run(self) -> int:
        self._preflight()
        print(f"Evaluation model: {self.provider}; evidence: {self.evidence_mode}")
        print(f"Scenarios: {', '.join(item.name for item in self.config.scenarios)}")
        kubeconfig = provision(self.kube, self.state_dir)
        evaluator = Kubectl(kubeconfig=kubeconfig)
        try:
            for scenario in self.config.scenarios:
                for repeat in range(1, self.config.repeats + 1):
                    self._check_budget()
                    print(f"[{scenario.name} repeat {repeat}/{self.config.repeats}] resetting and injecting")
                    record = self._run_once(scenario, repeat, evaluator)
                    self.runs = merge_run(self.runs, record)
                    self._write()
                    self._print_run(record)
                    self._assert_ci_evidence(record)
                    self._add_spend(record)
        finally:
            self._write()
            try:
                print("Resetting the Sandbox after the batch")
                self._reset()
            except (HarnessError, KubectlError) as error:
                print(
                    f"warning: the Sandbox could not be reset after the batch: {error}",
                    file=sys.stderr,
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
        if provider != "replay":
            raise HarnessError(
                "make eval-replay requires the agent's provider to be replay; "
                f"the stack is configured with {provider!r}"
            )
        for scenario in self.config.scenarios:
            if scenario.replay_recording is None:
                raise HarnessError(
                    f"Scenario {scenario.name!r} has no Replay recording for the CI subset. "
                    "The documented subset is oom, unschedulable, and bad-config."
                )
            recording = self.config.repo_root / ADAPTER_FIXTURES / scenario.replay_recording
            if not recording.is_file():
                raise HarnessError(f"the Replay recording {recording} is missing")

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

    def _run_once(self, scenario: Scenario, repeat: int, evaluator: Kubectl) -> dict[str, Any]:
        evidence = Evidence()
        triage: dict[str, Any] | None = None
        phases: list[dict[str, str]] = []
        failure = ""
        if self.config.adapter:
            self._set_recording(scenario)
        self._reset()
        evidence.known_good = self._memory_settings()
        self._delete_triages()
        since = isoformat(utc_now())
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
                if self.config.adapter and action != "NoAction":
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

    def _reset(self) -> None:
        if self.config.adapter:
            self._adapter_client("reset")
            self._wait_sandbox_healthy()
        else:
            self._make("reset", timeout=self.config.timeouts.reset)
            self._wait_sandbox_healthy()
        self.kube.run(
            "-n",
            SANDBOX_NAMESPACE,
            "rollout",
            "status",
            f"deployment/{CHECKOUT}",
            "--timeout=600s",
            timeout=660,
        )

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
        self._make("break", f"SCENARIO={scenario.name}", timeout=1500)

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
        self._start_triage(scenario)
        triage = self._find_triage(scenario.workload, isoformat(utc_now()), self.config.timeouts.manual_start)
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
        steps = (triage.get("status") or {}).get("steps") or []
        starts = [
            _parse_time(step.get("time"))
            for step in steps
            if step.get("tool") == "k8s__resources_get" and "github-write" in str(step.get("summary", ""))
        ]
        start = next((item for item in starts if item is not None), None)
        if start is None:
            return GatewayEvidence()
        end = utc_now()
        denials = gateway.denials(self.kube, start, end)
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
        show the deterministic Verification checks and the full five-minute
        observation window that the server decided. A run that reached a
        Proposal must show the eval-approver as the approver.
        """
        if not self.config.require_pass:
            return
        if record["phase"] == "Recovered":
            verification = record["verification"]
            checks = verification["checks"]
            if not checks or any(not check["passed"] for check in checks):
                raise HarnessError("a Recovered run did not record passing deterministic Verification checks")
            window = verification["windowSeconds"]
            if window is None or window < 300:
                raise HarnessError("a Recovered run did not record the full five-minute Verification window")
        if record["action"] is not None and record["approvedBy"] != APPROVER_USER:
            raise HarnessError(f"the {record['scenario']} run was not approved by {APPROVER_USER}")

    def _print_run(self, record: dict[str, Any]) -> None:
        seconds = record["timeToRecoveredSeconds"]
        usage = record["usage"]
        print(
            f"  {record['scenario']} repeat {record['repeat']}: {record['action']} -> "
            f"{record['result']} ({record['phase']}, "
            f"{'n/a' if seconds is None else f'{seconds:.0f}s'} to Recovered, "
            f"{usage.get('inputTokens') or 0}+{usage.get('outputTokens') or 0} tokens, "
            f"${usage.get('costUSD') or 0:.4f})"
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
