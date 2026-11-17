"""The published Claude Haiku 5.5 prices used to estimate one Triage's cost.

Source: https://platform.claude.com/docs/en/about-claude/pricing, verified on
2026-11-17. Claude Haiku 5.5 prices a request by its prompt length: up to
100,000 input tokens costs $0.10 per million input tokens and $0.50 per million
output tokens; a longer prompt costs $0.50 and $2.50.

`Usage.input_tokens` counts every input token, including cache reads and cache
writes, which are billed below the base input rate. Pricing all of them at the
base rate therefore overstates the cost slightly, never understates it. The
figure is an estimate for the Triage record and the dashboard, not an invoice.
"""

from __future__ import annotations

INPUT_USD_PER_MTOK = 0.10
OUTPUT_USD_PER_MTOK = 0.50
LONG_CONTEXT_INPUT_USD_PER_MTOK = 0.50
LONG_CONTEXT_OUTPUT_USD_PER_MTOK = 2.50
LONG_CONTEXT_THRESHOLD_TOKENS = 100_000


def cost_usd(input_tokens: int, output_tokens: int) -> float:
    """Return the estimated cost of one model call in United States dollars."""
    if input_tokens > LONG_CONTEXT_THRESHOLD_TOKENS:
        return (
            input_tokens * LONG_CONTEXT_INPUT_USD_PER_MTOK + output_tokens * LONG_CONTEXT_OUTPUT_USD_PER_MTOK
        ) / 1_000_000
    return (input_tokens * INPUT_USD_PER_MTOK + output_tokens * OUTPUT_USD_PER_MTOK) / 1_000_000
