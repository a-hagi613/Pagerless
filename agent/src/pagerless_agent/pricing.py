"""The published model prices used to estimate one Triage's cost.

Claude Haiku 5.5 prices a request by its prompt length: up to 100,000 input
tokens costs $0.10 per million input tokens and $0.50 per million output tokens;
a longer prompt costs $0.50 and $2.50. Source:
https://platform.claude.com/docs/en/about-claude/pricing, verified on 2026-11-17.

Amazon Nova 2 Lite, reached through the `us.` geo cross-Region inference
profile, costs $0.33 per million input and $2.75 per million output tokens with
no long-context tier. Source: https://aws.amazon.com/bedrock/pricing, Standard
tier, verified on 2026-11-17.

`Usage.input_tokens` counts every input token, including cache reads and cache
writes, which are billed at their own rates: cache reads below the base input
rate and cache writes at or above it. Pricing every input token at the base rate
therefore overstates cached reads and can understate cached writes. The figure is
an estimate for the Triage record and the dashboard, not an invoice.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Prices:
    """The published rates of one model, in United States dollars per million tokens."""

    input_usd_per_mtok: float
    output_usd_per_mtok: float
    long_context_input_usd_per_mtok: float | None = None
    long_context_output_usd_per_mtok: float | None = None
    long_context_threshold_tokens: int | None = None


HAIKU_5_5_PRICES = Prices(
    input_usd_per_mtok=0.10,
    output_usd_per_mtok=0.50,
    long_context_input_usd_per_mtok=0.50,
    long_context_output_usd_per_mtok=2.50,
    long_context_threshold_tokens=100_000,
)

NOVA_2_LITE_PRICES = Prices(input_usd_per_mtok=0.33, output_usd_per_mtok=2.75)


def cost_usd(input_tokens: int, output_tokens: int, *, prices: Prices = HAIKU_5_5_PRICES) -> float:
    """Return the estimated cost of one model call in United States dollars."""
    threshold = prices.long_context_threshold_tokens
    if (
        threshold is not None
        and input_tokens > threshold
        and prices.long_context_input_usd_per_mtok is not None
        and prices.long_context_output_usd_per_mtok is not None
    ):
        return (
            input_tokens * prices.long_context_input_usd_per_mtok
            + output_tokens * prices.long_context_output_usd_per_mtok
        ) / 1_000_000
    return (input_tokens * prices.input_usd_per_mtok + output_tokens * prices.output_usd_per_mtok) / 1_000_000
