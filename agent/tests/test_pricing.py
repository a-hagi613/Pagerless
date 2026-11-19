"""The published prices that turn one model call's usage into the Page's cost estimate."""

from __future__ import annotations

import pytest

from pagerless_agent.pricing import HAIKU_5_5_PRICES, NOVA_2_LITE_PRICES, cost_usd


def test_haiku_5_5_prices_a_short_prompt_at_the_base_rate() -> None:
    assert cost_usd(5000, 500, prices=HAIKU_5_5_PRICES) == pytest.approx(0.00075)


def test_haiku_5_5_prices_a_prompt_above_the_threshold_at_the_long_context_rate() -> None:
    assert cost_usd(100_001, 100, prices=HAIKU_5_5_PRICES) == pytest.approx(0.0502505)


def test_haiku_5_5_keeps_the_base_rate_up_to_and_including_the_threshold() -> None:
    assert cost_usd(100_000, 0, prices=HAIKU_5_5_PRICES) == pytest.approx(0.01)


def test_nova_2_lite_has_one_rate_and_no_long_context_tier() -> None:
    assert cost_usd(5000, 500, prices=NOVA_2_LITE_PRICES) == pytest.approx(0.003025)
    assert cost_usd(200_000, 500, prices=NOVA_2_LITE_PRICES) == pytest.approx(0.067375)
