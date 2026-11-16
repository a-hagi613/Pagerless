from importlib.metadata import version

import pagerless_agent


def test_installed_distribution_matches_package_version() -> None:
    assert version("pagerless-agent") == pagerless_agent.__version__
