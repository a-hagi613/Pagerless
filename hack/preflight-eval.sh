#!/usr/bin/env bash
# Fast sub-30s preflight gate for evaluation and Replay capture runs.
#
# By default, runs offline unit, lint, shell syntax, and manifest checks in under
# 30 seconds without a cluster or paid model calls. With --live, also sends one
# compact structured Proposal turn through pagerless-agent's live provider and
# RecordingProvider and validates the recorded artifact with evals/replay/capture.py.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

live_mode=0
for arg in "$@"; do
	case "$arg" in
	--live)
		live_mode=1
		;;
	*)
		die "usage: hack/preflight-eval.sh [--live]"
		;;
	esac
done

start_ns="$(date +%s%N)"

echo "[1/4] Running Python contract, prompt, Replay, and capture normalization unit tests..."
(
	cd "$REPO_ROOT/agent" &&
		uv run --locked pytest -c pyproject.toml -q \
			tests/test_prompts.py \
			tests/test_contract.py \
			tests/test_replay.py \
			../evals/replay/test_capture.py
)

echo "[2/4] Running Python static lint checks on prompts, contract, and Replay capture..."
(
	cd "$REPO_ROOT/agent" &&
		uv run --locked ruff check \
			src/pagerless_agent/prompts.py \
			src/pagerless_agent/contract.py \
			../evals/replay
)

echo "[3/4] Running Go validation/rendering unit tests and evaluation harness unit tests..."
(
	cd "$REPO_ROOT" &&
		go test \
			./internal/patchresources/... \
			./internal/rollback/... \
			./internal/scale/... \
			./internal/proposal/... \
			./internal/agentapi/...
)
make -C "$REPO_ROOT" test-eval

echo "[4/4] Checking hack/*.sh syntax and validating rendered Kubernetes manifests..."
bash -n "$REPO_ROOT"/hack/*.sh
make -C "$REPO_ROOT" validate

if [ "$live_mode" -eq 1 ]; then
	echo "[live] Running single-turn live provider + RecordingProvider + capture validation check..."
	load_env
	(
		cd "$REPO_ROOT/agent" &&
			PYTHONPATH="$REPO_ROOT/evals/replay${PYTHONPATH:+:$PYTHONPATH}" \
				uv run --locked python - <<'PY'
import asyncio
import os
import tempfile
from pathlib import Path
from typing import Any

from capture import _validation_bundle, validate_recording
from pagerless_agent import prompts
from pagerless_agent.budget import Limits
from pagerless_agent.contract import proposal_tool_schema
from pagerless_agent.model import Message, ModelProvider, ModelRequest, ModelResponse, Stage, Usage
from pagerless_agent.providers import ANTHROPIC_MODEL, BEDROCK_MODEL
from pagerless_agent.replay import RecordingProvider
from pagerless_agent.runtime import create_provider


class _HybridOOMProbeProvider:
    """Uses synthetic investigate/diagnose turns and one real live PROPOSE turn."""

    def __init__(self, live: ModelProvider, pod: str, commit: str) -> None:
        self._live = live
        self._pod = pod
        self._commit = commit
        self.live_usage: Usage | None = None

    async def complete(self, request: ModelRequest) -> ModelResponse:
        if request.stage == Stage.INVESTIGATE:
            return ModelResponse(
                text=f"Pod {self._pod} was OOMKilled in container memory-holder (32Mi limit).",
                usage=Usage(input_tokens=1, output_tokens=1),
            )
        if request.stage == Stage.DIAGNOSE:
            return ModelResponse(
                structured={
                    "diagnosis": (
                        f"Container memory-holder in pod {self._pod} of checkout was OOMKilled "
                        f"after commit {self._commit[:7]} lowered its memory limit to 32Mi."
                    ),
                    "findings": [
                        {
                            "kind": "Event",
                            "source": "context bundle",
                            "summary": f"{self._pod} terminated with OOMKilled at 32Mi limit",
                        }
                    ],
                },
                usage=Usage(input_tokens=1, output_tokens=1),
            )
        response = await self._live.complete(request)
        self.live_usage = response.usage
        return response


async def _run_live_probe() -> None:
    provider_name = os.environ.get("MODEL_PROVIDER") or os.environ.get("PAGERLESS_MODEL_PROVIDER") or ""
    if provider_name in ("", "replay"):
        if os.environ.get("ANTHROPIC_API_KEY"):
            provider_name = "anthropic"
        elif os.environ.get("AWS_PROFILE") or os.environ.get("AWS_ACCESS_KEY_ID"):
            provider_name = "bedrock"
        else:
            raise RuntimeError(
                "set MODEL_PROVIDER=anthropic (with ANTHROPIC_API_KEY) or MODEL_PROVIDER=bedrock for --live"
            )
    if provider_name not in ("anthropic", "bedrock"):
        raise RuntimeError(f"unsupported live provider {provider_name!r}; expected anthropic or bedrock")

    bundle = _validation_bundle()
    pod = bundle.pods[0].name
    commit = bundle.commits[0].sha
    live = create_provider(provider_name, Path("/nonexistent"))
    close: Any = getattr(live, "aclose", None)
    try:
        hybrid = _HybridOOMProbeProvider(live, pod=pod, commit=commit)
        # validate_recording checks the canonical Replay model header while verifying the
        # normalized turns, DiagnosisOutput, and PROPOSED_ACTION schema.
        recorder = RecordingProvider(hybrid, scenario="oom", model=ANTHROPIC_MODEL)
        system = prompts.system_prompt(Limits())
        await recorder.complete(
            ModelRequest(
                stage=Stage.INVESTIGATE,
                system=system,
                messages=(Message(role="user", content="Investigate checkout OOMKilled."),),
                tools=(),
                bundle=bundle,
            )
        )
        await recorder.complete(
            ModelRequest(
                stage=Stage.DIAGNOSE,
                system=system,
                messages=(Message(role="user", content=prompts.DIAGNOSE_PROMPT),),
                tools=(),
                bundle=bundle,
            )
        )
        propose_prompt = (
            f"Deployment sandbox/checkout pod {pod} container memory-holder was OOMKilled with "
            f"requests.memory=32Mi and limits.memory=32Mi after faulty commit {commit}. "
            "Propose PatchResources on container memory-holder raising requests.memory to 64Mi and "
            "limits.memory to 128Mi.\n\n" + prompts.PROPOSE_PROMPT
        )
        await recorder.complete(
            ModelRequest(
                stage=Stage.PROPOSE,
                system=system,
                messages=(Message(role="user", content=propose_prompt),),
                tools=(),
                output_schema=proposal_tool_schema(),
                bundle=bundle,
                max_output_tokens=256,
            )
        )
        recording = recorder.recording()
        secrets = tuple(
            value
            for key in (
                "ANTHROPIC_API_KEY",
                "AWS_SECRET_ACCESS_KEY",
                "AWS_SESSION_TOKEN",
                "GITHUB_READ_TOKEN",
                "GITHUB_WRITE_TOKEN",
            )
            if (value := os.environ.get(key, ""))
        )
        with tempfile.TemporaryDirectory() as tmp_dir:
            artifact = Path(tmp_dir) / "preflight-oom.json"
            recording.save(artifact)
            validated = validate_recording(artifact, secrets)
        model_id = ANTHROPIC_MODEL if provider_name == "anthropic" else BEDROCK_MODEL
        usage = hybrid.live_usage
        action = (validated.turns[-1].response.structured or {}).get("action", "unknown")
        print(
            f"Live preflight passed: provider={provider_name} model={model_id} "
            f"action={action} input_tokens={usage.input_tokens if usage else 0} "
            f"output_tokens={usage.output_tokens if usage else 0}"
        )
    finally:
        if close is not None:
            await close()


asyncio.run(_run_live_probe())
PY
	)
fi

end_ns="$(date +%s%N)"
elapsed_ms=$(((end_ns - start_ns) / 1000000))
printf 'preflight-eval: all checks passed in %d.%03ds\n' "$((elapsed_ms / 1000))" "$((elapsed_ms % 1000))"
