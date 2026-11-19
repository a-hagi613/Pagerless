"""Run one agent Job with a scoped server client and one OpenAI-compatible provider.

This module is the only place where an HTTP client is constructed. There are two, and
only two: the tool broker client for `PAGERLESS_SERVER_URL`, and the model client for
`PAGERLESS_MODEL_BASE_URL`. No Kubernetes, connector, or GitHub client exists in this
package, because the agent holds no credential for any of them.
"""

from __future__ import annotations

import asyncio
import logging
import os
from collections.abc import Mapping

from pagerless_agent.agent import Agent, Outcome
from pagerless_agent.config import TASK_NARRATIVE, ConfigError, Settings
from pagerless_agent.model import ModelProvider
from pagerless_agent.openai_compat import (
    OpenAICompatibleProvider,
    ProviderAuthError,
    ProviderRejectedError,
    ProviderTruncatedError,
)
from pagerless_agent.toolclient import ServerError, ToolClient

log = logging.getLogger(__name__)


def create_provider(settings: Settings) -> OpenAICompatibleProvider:
    """Return the one provider this agent uses: any OpenAI-compatible endpoint."""
    return OpenAICompatibleProvider(settings.model)


async def run(
    settings: Settings,
    *,
    provider: ModelProvider | None = None,
    client: ToolClient | None = None,
) -> Outcome:
    """Investigate one Page and report the outcome, closing only what this function created."""
    selected = provider or create_provider(settings)
    tools = client or ToolClient(
        settings.server_url,
        settings.page,
        settings.page_uid,
        settings.token_file,
    )
    try:
        return await Agent(settings=settings, provider=selected, client=tools).run()
    finally:
        if client is None:
            await tools.aclose()
        close = getattr(selected, "aclose", None)
        if provider is None and close is not None:
            await close()


def main(env: Mapping[str, str] | None = None) -> int:
    """Run the Job the environment selects, and return its exit code without printing a secret."""
    environment = os.environ if env is None else env
    try:
        settings = Settings.from_env(environment)
    except ConfigError as error:
        log.error("invalid agent configuration: %s", error)
        return 1
    if settings.task == TASK_NARRATIVE:
        from pagerless_agent.narrative import main as narrative_main

        return narrative_main(environment)
    try:
        outcome = asyncio.run(run(settings))
    except ProviderAuthError as error:
        log.error("the model endpoint refused the credentials: %s", error)
        return 5
    except (ProviderRejectedError, ProviderTruncatedError) as error:
        log.error("the model endpoint rejected the request: %s", error)
        return 6
    except ServerError as error:
        log.error("pagerless-server is unavailable: %s", error)
        return 1
    except Exception as error:
        log.error("agent startup failed (%s)", type(error).__name__)
        return 1
    log.info("agent finished: %s", outcome.kind)
    return outcome.exit_code
