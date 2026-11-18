"""Run the Pagerless evaluation harness.

The shell entry point is evals/harness/eval.sh; this module is the Python body
so that tests can import the scoring and result code without a cluster.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from pagerless_eval.answer_key import UnknownScenarioError, parse_scenarios
from pagerless_eval.kube import KubectlError, default_context
from pagerless_eval.results import ResultError
from pagerless_eval.runner import Config, Harness, HarnessError

REPO_ROOT = Path(__file__).resolve().parents[2]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenarios", default="all", help="all, or a comma-separated Scenario list")
    parser.add_argument("--repeats", type=int, default=2, help="runs per Scenario (default: 2)")
    parser.add_argument(
        "--repeat-number",
        type=int,
        default=None,
        help="run only this repeat of every Scenario, so one shard holds one run",
    )
    parser.add_argument(
        "--results-dir",
        type=Path,
        default=REPO_ROOT / "evals" / "results",
        help="where the versioned result JSON is written",
    )
    parser.add_argument(
        "--max-tokens",
        type=int,
        default=0,
        help="hard input+output token budget for the batch; 0 means unbounded",
    )
    parser.add_argument(
        "--max-cost-usd",
        type=float,
        default=0.0,
        help="hard estimated cost budget in USD for the batch; 0 means unbounded",
    )
    parser.add_argument(
        "--verification-window",
        type=int,
        default=300,
        help="seconds of Observation that a Recovered run must record; "
        "ADR-0006 keeps 300 for every live run",
    )
    parser.add_argument(
        "--require-pass",
        action="store_true",
        help="exit non-zero when any scored run fails (CI uses this)",
    )
    parser.add_argument(
        "--adapter",
        action="store_true",
        help="use the credential-free Sandbox adapter; CI's Replay subset sets this",
    )
    parser.add_argument("--context", default=default_context(), help="kubeconfig context")
    arguments = parser.parse_args(argv)

    try:
        config = Config(
            repo_root=REPO_ROOT,
            scenarios=parse_scenarios(arguments.scenarios),
            repeats=arguments.repeats,
            results_dir=arguments.results_dir.resolve(),
            adapter=arguments.adapter,
            require_pass=arguments.require_pass,
            max_tokens=arguments.max_tokens,
            max_cost_usd=arguments.max_cost_usd,
            verification_window=arguments.verification_window,
            context=arguments.context,
            repeat_number=arguments.repeat_number,
        )
        if config.repeats < 1:
            raise HarnessError("--repeats must be at least 1")
        if config.repeat_number is not None and not 1 <= config.repeat_number <= config.repeats:
            raise HarnessError(
                f"--repeat-number must be between 1 and --repeats ({config.repeats}), "
                f"not {config.repeat_number}"
            )
        if config.max_tokens < 0 or config.max_cost_usd < 0:
            raise HarnessError("--max-tokens and --max-cost-usd cannot be negative")
        if config.verification_window < 1:
            raise HarnessError("--verification-window must be at least one second")
        harness = Harness(config)
        code = harness.run()
        if harness.path is not None:
            print(f"Results written to {harness.path}")
        return code
    except (HarnessError, KubectlError, ResultError, UnknownScenarioError, OSError, ValueError) as error:
        print(f"make eval failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
