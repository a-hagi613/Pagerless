"""Read, write, validate, and merge the versioned evaluation result documents.

Each run is recorded as JSON named by the date and the Pagerless commit that ran
it. A document is never published from a run that did not happen: the harness
writes a run only after it has read the terminal Triage, and a Replay document
is labelled as Replay so the README table can exclude it.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import subprocess
import sys
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from .answer_key import parse_scenarios

# Schema version 2 added validationAttempts, proposalAttempt, and refusals to each run.
SCHEMA_VERSION = 2
EVIDENCE_LIVE = "live"
EVIDENCE_REPLAY = "replay"
EVIDENCE = (EVIDENCE_LIVE, EVIDENCE_REPLAY)
RESULTS = ("pass", "mitigated", "fail")
VERIFICATION_OUTCOMES = ("Recovered", "NotRecovered", "NotApplicable", "NotRun")
TERMINAL_PHASES = (
    "Recovered",
    "NotRecovered",
    "Diagnosed",
    "Rejected",
    "Expired",
    "Invalid",
    "Failed",
    # The run did not reach a terminal Triage phase before its timeout. The
    # harness records that honestly and scores the run as a failure.
    "TimedOut",
)


class ResultError(ValueError):
    """A result document is invalid or cannot be written."""


def utc_now() -> datetime:
    return datetime.now(UTC)


def isoformat(value: datetime) -> str:
    return value.astimezone(UTC).isoformat().replace("+00:00", "Z")


def pagerless_revision(repo_root: Path) -> tuple[str, bool]:
    """Return the checked-out commit and whether the worktree is dirty."""
    commit = subprocess.run(
        ["git", "-C", str(repo_root), "rev-parse", "HEAD"],
        capture_output=True,
        text=True,
        check=False,
    )
    if commit.returncode:
        raise ResultError("the Pagerless commit could not be read from this checkout")
    status = subprocess.run(
        ["git", "-C", str(repo_root), "status", "--porcelain", "--untracked-files=no"],
        capture_output=True,
        text=True,
        check=False,
    )
    if status.returncode:
        raise ResultError("the Pagerless worktree state could not be read from this checkout")
    return commit.stdout.strip(), bool(status.stdout.strip())


def result_path(results_dir: Path, started: datetime, commit: str, evidence: str) -> Path:
    """Return the versioned path for a run batch: date, short commit, evidence."""
    if evidence not in EVIDENCE:
        raise ResultError(f"evidence must be one of {', '.join(EVIDENCE)}, not {evidence!r}")
    if len(commit) < 12 or any(character not in "0123456789abcdef" for character in commit):
        raise ResultError("the Pagerless commit must be a full hexadecimal SHA")
    return results_dir / f"{started.astimezone(UTC):%Y-%m-%d}-{commit[:12]}-{evidence}.json"


def model_label(provider: str) -> tuple[str, str]:
    """Return the model id and human label for an agent provider."""
    match provider:
        case "anthropic":
            return "claude-haiku-5-5", "Claude Haiku 5.5"
        case "bedrock":
            return "anthropic.claude-haiku-5-5", "Claude Haiku 5.5 on Bedrock"
        case "replay":
            return "replay", "Replay (synthetic recordings)"
        case _:
            raise ResultError(f"unknown model provider {provider!r}")


def build_document(
    *,
    commit: str,
    dirty: bool,
    provider: str,
    evidence: str,
    scenarios: tuple[str, ...],
    repeats: int,
    started_at: datetime,
    budget: dict[str, Any],
    runs: list[dict[str, Any]],
) -> dict[str, Any]:
    model_id, label = model_label(provider)
    parse_scenarios(",".join(scenarios))
    return {
        "schemaVersion": SCHEMA_VERSION,
        "generatedAt": isoformat(utc_now()),
        "startedAt": isoformat(started_at),
        "pagerlessCommit": commit,
        "pagerlessDirty": dirty,
        "model": {"provider": provider, "id": model_id, "label": label},
        "evidence": evidence,
        "scenarios": list(scenarios),
        "repeats": repeats,
        "budget": budget,
        "runs": runs,
    }


def merge_run(runs: list[dict[str, Any]], run: dict[str, Any]) -> list[dict[str, Any]]:
    """Replace the run with the same Scenario and repeat, then sort deterministically."""
    key = (run["scenario"], run["repeat"])
    merged = [item for item in runs if (item.get("scenario"), item.get("repeat")) != key]
    merged.append(run)
    merged.sort(key=lambda item: (item.get("scenarioNumber", 0), item.get("repeat", 0)))
    return merged


def merge_documents(
    documents: list[dict[str, Any]],
    *,
    commit: str,
    scenarios: tuple[str, ...] | None = None,
    repeats: int | None = None,
) -> dict[str, Any]:
    """Merge a batch's shard documents into the one document a batch publishes.

    A full paid batch runs on several GitHub Actions runners at once, so each
    runner writes a document that covers its own Scenarios, records its own
    spend, and starts at its own time. The published record is still one batch,
    so this merges the shards back together: it keeps one run per Scenario and
    repeat, starts at the earliest shard, and sums the spend and the budgets the
    shards were given.

    It refuses a batch that cannot be published honestly: a shard from another
    commit or evidence mode, a dirty worktree, two shards that record the same
    run, an incomplete or extended Scenario/repeat set, and any incomplete or
    errored run. A run that completed cleanly and was graded ``fail`` is a real
    measurement of the model, so it is kept and counted in the Fail column.
    """
    if not documents:
        raise ResultError("no shard documents were merged")
    for document in documents:
        validate_document(document)
        if document["pagerlessCommit"] != commit:
            raise ResultError(
                f"a shard document was written for {document['pagerlessCommit']}, not {commit}"
            )
        if document["evidence"] != EVIDENCE_LIVE:
            raise ResultError(f"a shard document records {document['evidence']!r} evidence, not live")
        if document["pagerlessDirty"]:
            raise ResultError("a shard document came from a dirty Pagerless worktree")

    model = documents[0]["model"]
    for document in documents[1:]:
        if document["model"] != model:
            raise ResultError("the shard documents were not run with the same model")

    declared = sorted({name for document in documents for name in document["scenarios"]})
    requested = declared if scenarios is None else scenarios
    expected = parse_scenarios(",".join(requested))
    undeclared = sorted({item.name for item in expected} - set(declared))
    if undeclared:
        raise ResultError(f"no shard document covers {', '.join(undeclared)}")

    observed_repeats = {document["repeats"] for document in documents}
    if len(observed_repeats) != 1:
        raise ResultError("the shard documents disagree on the repeat count")
    batch_repeats = observed_repeats.pop()
    if repeats is not None and repeats != batch_repeats:
        raise ResultError(f"the shard documents record {batch_repeats} repeats, not {repeats}")

    runs: list[dict[str, Any]] = []
    recorded: set[tuple[str, int]] = set()
    for document in documents:
        for run in document["runs"]:
            key = (run["scenario"], run["repeat"])
            if key in recorded:
                raise ResultError(
                    f"more than one shard document records {run['scenario']} repeat {run['repeat']}"
                )
            recorded.add(key)
            runs.append(run)

    expected_runs = {
        (item.name, repeat) for item in expected for repeat in range(1, batch_repeats + 1)
    }
    missing = sorted(expected_runs - recorded)
    if missing:
        raise ResultError(
            "the merged batch is missing "
            + ", ".join(f"{name} repeat {repeat}" for name, repeat in missing)
        )
    unexpected = sorted(recorded - expected_runs)
    if unexpected:
        raise ResultError(
            "the merged batch carries runs it did not declare: "
            + ", ".join(f"{name} repeat {repeat}" for name, repeat in unexpected)
        )
    incomplete = sorted(
        f"{run['scenario']} repeat {run['repeat']}"
        for run in runs
        if run["failure"] is not None or run["phase"] in ("Failed", "TimedOut")
    )
    if incomplete:
        raise ResultError(
            f"the merged batch carries incomplete or errored runs: {', '.join(incomplete)}"
        )

    budget = {
        "maxTokens": sum(document["budget"]["maxTokens"] for document in documents),
        "maxCostUSD": round(sum(document["budget"]["maxCostUSD"] for document in documents), 6),
        "spentTokens": sum(document["budget"]["spentTokens"] for document in documents),
        "spentCostUSD": round(sum(document["budget"]["spentCostUSD"] for document in documents), 6),
        "exhausted": any(document["budget"]["exhausted"] for document in documents),
    }
    if (budget["maxTokens"] and budget["spentTokens"] > budget["maxTokens"]) or (
        budget["maxCostUSD"] and budget["spentCostUSD"] > budget["maxCostUSD"]
    ):
        budget["exhausted"] = True

    merged = build_document(
        commit=commit,
        dirty=False,
        provider=model["provider"],
        evidence=EVIDENCE_LIVE,
        scenarios=tuple(item.name for item in expected),
        repeats=batch_repeats,
        started_at=min(_iso_datetime(document["startedAt"]) for document in documents),
        budget=budget,
        runs=sorted(runs, key=lambda run: (run["scenarioNumber"], run["repeat"])),
    )
    validate_document(merged)
    return merged


def write_document(path: Path, document: dict[str, Any]) -> None:
    """Validate and write a document atomically, merging runs from an existing file."""
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        existing = load_document(path)
        if (
            existing["pagerlessCommit"] != document["pagerlessCommit"]
            or existing["evidence"] != document["evidence"]
        ):
            raise ResultError(f"{path} belongs to a different commit or evidence mode")
        runs = existing["runs"]
        for run in document["runs"]:
            runs = merge_run(runs, run)
        document = {**document, "runs": runs}
    validate_document(document)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(document, indent=2, sort_keys=False) + "\n", encoding="utf-8")
    os.replace(temporary, path)


def load_document(path: Path) -> dict[str, Any]:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ResultError(f"{path} is not a readable result document: {error}") from None
    if not isinstance(document, dict):
        raise ResultError(f"{path} does not hold a result object")
    validate_document(document)
    return document


def result_files(results_dir: Path) -> list[Path]:
    if not results_dir.exists():
        return []
    return sorted(results_dir.glob("*.json"))


def load_all(results_dir: Path) -> list[dict[str, Any]]:
    return [load_document(path) for path in result_files(results_dir)]


def validate_document(document: dict[str, Any]) -> None:
    """Refuse a document that is incomplete, inconsistent, or not honest about evidence."""
    required = {
        "schemaVersion",
        "generatedAt",
        "startedAt",
        "pagerlessCommit",
        "pagerlessDirty",
        "model",
        "evidence",
        "scenarios",
        "repeats",
        "budget",
        "runs",
    }
    missing = sorted(required - document.keys())
    if missing:
        raise ResultError(f"the result document is missing {', '.join(missing)}")
    if document["schemaVersion"] != SCHEMA_VERSION:
        raise ResultError(f"unsupported result schema version {document['schemaVersion']!r}")
    if document["evidence"] not in EVIDENCE:
        raise ResultError(f"evidence must be one of {', '.join(EVIDENCE)}")
    commit = document["pagerlessCommit"]
    if not isinstance(commit, str) or len(commit) != 40 or any(c not in "0123456789abcdef" for c in commit):
        raise ResultError("pagerlessCommit must be a full hexadecimal SHA")
    model = document["model"]
    if not isinstance(model, dict) or not all(
        isinstance(model.get(key), str) and model[key] for key in ("provider", "id", "label")
    ):
        raise ResultError("model must name a provider, an id, and a label")
    if not isinstance(document["pagerlessDirty"], bool):
        raise ResultError("pagerlessDirty must be a boolean")
    if not isinstance(document["scenarios"], list) or not document["scenarios"]:
        raise ResultError("scenarios must be a non-empty list")
    parse_scenarios(",".join(str(item) for item in document["scenarios"]))
    if not isinstance(document["repeats"], int) or document["repeats"] < 1:
        raise ResultError("repeats must be a positive integer")
    _validate_iso(document["generatedAt"], "generatedAt")
    _validate_iso(document["startedAt"], "startedAt")
    budget = document["budget"]
    if not isinstance(budget, dict) or not isinstance(budget.get("exhausted"), bool):
        raise ResultError("budget must record whether it was exhausted")
    for field in ("maxTokens", "maxCostUSD", "spentTokens", "spentCostUSD"):
        value = budget.get(field)
        if not isinstance(value, (int, float)) or isinstance(value, bool) or value < 0:
            raise ResultError(f"budget.{field} must be a non-negative number")
    if not isinstance(document["runs"], list):
        raise ResultError("runs must be a list")
    for run in document["runs"]:
        validate_run(run)


def validate_run(run: dict[str, Any]) -> None:
    required = {
        "scenario",
        "scenarioNumber",
        "workload",
        "repeat",
        "triage",
        "startedAt",
        "terminalAt",
        "phase",
        "phaseHistory",
        "approvedBy",
        "action",
        "validationAttempts",
        "proposalAttempt",
        "refusals",
        "result",
        "score",
        "timeToRecoveredSeconds",
        "timeToTerminalSeconds",
        "verification",
        "usage",
        "evidence",
        "failure",
    }
    missing = sorted(required - run.keys())
    if missing:
        raise ResultError(f"a run is missing {', '.join(missing)}")
    scenario = parse_scenarios(str(run["scenario"]))
    if len(scenario) != 1:
        raise ResultError(f"a run must name one Scenario, not {run['scenario']!r}")
    if run["scenarioNumber"] != scenario[0].number:
        raise ResultError(f"scenarioNumber does not match Scenario {run['scenario']!r}")
    if run["workload"] != scenario[0].workload:
        raise ResultError(f"workload does not match Scenario {run['scenario']!r}")
    if not isinstance(run["repeat"], int) or run["repeat"] < 1:
        raise ResultError("repeat must be a positive integer")
    attempts = run["validationAttempts"]
    if not isinstance(attempts, int) or isinstance(attempts, bool) or attempts < 0:
        raise ResultError("validationAttempts must be a non-negative integer")
    proposal_attempt = run["proposalAttempt"]
    if proposal_attempt is not None and (
        not isinstance(proposal_attempt, int)
        or isinstance(proposal_attempt, bool)
        or proposal_attempt < 1
    ):
        raise ResultError("proposalAttempt must be null or a positive integer")
    if not isinstance(run["refusals"], list):
        raise ResultError("refusals must be a list")
    for refusal in run["refusals"]:
        attempt = refusal.get("attempt") if isinstance(refusal, dict) else None
        if (
            not isinstance(refusal, dict)
            or (attempt is not None and (not isinstance(attempt, int) or isinstance(attempt, bool)))
            or not isinstance(refusal.get("action"), (str, type(None)))
            or not isinstance(refusal.get("parameters"), (str, type(None)))
            or not isinstance(refusal.get("errors"), str)
        ):
            raise ResultError("every refusal must record its attempt, action, parameters, and errors")
    if run["result"] not in RESULTS:
        raise ResultError(f"result must be one of {', '.join(RESULTS)}, not {run['result']!r}")
    score = run["score"]
    if not isinstance(score, dict) or not isinstance(score.get("rule"), str):
        raise ResultError("score must record the deterministic rule that graded the run")
    checks = score.get("checks")
    if not isinstance(checks, list) or not checks:
        raise ResultError("score.checks must be a non-empty list")
    for check in checks:
        if (
            not isinstance(check, dict)
            or not isinstance(check.get("name"), str)
            or not isinstance(check.get("passed"), bool)
        ):
            raise ResultError("every score check must name itself and record a boolean")
    verification = run["verification"]
    if not isinstance(verification, dict) or verification.get("outcome") not in VERIFICATION_OUTCOMES:
        raise ResultError(f"Verification outcome must be one of {', '.join(VERIFICATION_OUTCOMES)}")
    usage = run["usage"]
    if not isinstance(usage, dict):
        raise ResultError("usage must be an object")
    for field in ("inputTokens", "outputTokens", "costUSD"):
        value = usage.get(field)
        if value is not None and (
            not isinstance(value, (int, float)) or isinstance(value, bool) or math.isnan(value) or value < 0
        ):
            raise ResultError(f"usage.{field} must be a non-negative number or null")
    if not isinstance(run["evidence"], dict):
        raise ResultError("evidence must be an object")
    if run["failure"] is not None and (not isinstance(run["failure"], str) or not run["failure"]):
        raise ResultError("failure must be null or a non-empty explanation")
    for field in ("startedAt", "terminalAt"):
        if run[field] is not None:
            _validate_iso(run[field], field)
    for field in ("timeToRecoveredSeconds", "timeToTerminalSeconds"):
        value = run[field]
        if value is not None and (
            not isinstance(value, (int, float)) or isinstance(value, bool) or value < 0
        ):
            raise ResultError(f"{field} must be null or a non-negative number")
    if run["phase"] not in TERMINAL_PHASES:
        raise ResultError(f"phase must be terminal, not {run['phase']!r}")
    if not isinstance(run["phaseHistory"], list):
        raise ResultError("phaseHistory must be a list")


def _validate_iso(value: Any, field: str) -> None:
    if not isinstance(value, str):
        raise ResultError(f"{field} must be an ISO timestamp")
    try:
        _iso_datetime(value)
    except ValueError:
        raise ResultError(f"{field} must be an ISO timestamp") from None


def _iso_datetime(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)


@dataclass(frozen=True)
class Totals:
    """The summary of the live runs in one or more documents."""

    model: str
    runs: int
    scenarios: int
    passed: int
    mitigated: int
    failed: int
    mean_seconds_to_recovered: float | None
    total_cost_usd: float


def totals(documents: list[dict[str, Any]]) -> Totals:
    """Summarise the live runs, excluding Replay and dirty-worktree batches."""
    live = [
        document
        for document in documents
        if document["evidence"] == EVIDENCE_LIVE and not document["pagerlessDirty"]
    ]
    runs = [run for document in live for run in document["runs"]]
    if not runs:
        return Totals("", 0, 0, 0, 0, 0, None, 0.0)
    labels = sorted({document["model"]["label"] for document in live})
    recovered = [
        run["timeToRecoveredSeconds"]
        for run in runs
        if run["phase"] == "Recovered" and run["timeToRecoveredSeconds"] is not None
    ]
    costs = [run["usage"]["costUSD"] for run in runs if isinstance(run["usage"].get("costUSD"), (int, float))]
    return Totals(
        model=", ".join(labels),
        runs=len(runs),
        scenarios=len({run["scenario"] for run in runs}),
        passed=sum(run["result"] == "pass" for run in runs),
        mitigated=sum(run["result"] == "mitigated" for run in runs),
        failed=sum(run["result"] == "fail" for run in runs),
        mean_seconds_to_recovered=(sum(recovered) / len(recovered)) if recovered else None,
        total_cost_usd=float(sum(costs)),
    )


def main(argv: list[str] | None = None) -> int:
    """Merge the shard documents of one paid batch into the published document."""
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    merge = commands.add_parser("merge", help="merge the shard documents under a directory tree")
    merge.add_argument(
        "--shards",
        type=Path,
        required=True,
        help="the directory tree holding the extracted shard artifacts",
    )
    merge.add_argument(
        "--out-dir",
        type=Path,
        required=True,
        help="the directory the single merged document is written to",
    )
    merge.add_argument("--commit", required=True, help="the full Pagerless commit the batch ran")
    merge.add_argument(
        "--scenarios",
        default="",
        help="all, or the comma-separated Scenarios the batch must cover; "
        "empty uses the Scenarios the shards declare",
    )
    merge.add_argument(
        "--summary",
        type=Path,
        default=None,
        help="append the rendered result table to this file, as GITHUB_STEP_SUMMARY expects",
    )
    arguments = parser.parse_args(argv)

    try:
        paths = sorted(arguments.shards.rglob("*.json"))
        if not paths:
            raise ResultError(f"no shard documents were found under {arguments.shards}")
        requested = (
            tuple(item.name for item in parse_scenarios(arguments.scenarios))
            if arguments.scenarios.strip()
            else None
        )
        merged = merge_documents(
            [load_document(path) for path in paths],
            commit=arguments.commit,
            scenarios=requested,
        )
        path = result_path(
            arguments.out_dir,
            _iso_datetime(merged["startedAt"]),
            merged["pagerlessCommit"],
            merged["evidence"],
        )
        write_document(path, merged)
        if arguments.summary is not None:
            # table.py imports this module, so the renderer is imported here.
            from .table import render

            arguments.summary.parent.mkdir(parents=True, exist_ok=True)
            with arguments.summary.open("a", encoding="utf-8") as summary:
                summary.write(render([merged]) + "\n")
    except (OSError, ValueError) as error:
        print(f"merging the evaluation shards failed: {error}", file=sys.stderr)
        return 1

    print(
        f"Merged {len(merged['runs'])} runs from {len(paths)} shard documents into {path}",
        flush=True,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
