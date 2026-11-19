"""The Narrative: one short descriptive model call after Verification.

`pagerless-server` starts a Narrative Job once a Page reaches a terminal Verification
outcome. The Job reads the Page's record, makes one model call, and posts the Narrative
back. It never writes a phase, a check, or a Finding, so a Narrative that fails or times
out leaves the outcome exactly as it was.

The Narrative has no Context Bundle and no tools. Its single message carries the Page
record inside the same nonce-delimited envelope the investigation uses, because the
record contains text an attacker may have written.
"""

from __future__ import annotations

import asyncio
import logging
import os
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Literal

from pagerless_agent import prompts
from pagerless_agent.config import TASK_NARRATIVE, ConfigError, Settings
from pagerless_agent.model import Message, ModelProvider, ModelRequest, Stage
from pagerless_agent.runtime import create_provider
from pagerless_agent.toolclient import ServerError, ServerRefusedError, ToolClient
from pagerless_agent.tools import new_nonce, present

log = logging.getLogger(__name__)

# The one call's output cap, which bounds the cost of the Narrative.
NARRATIVE_MAX_OUTPUT_TOKENS = 2000
# The Page record is rendered within this many characters, which bounds the prompt.
_MAX_RECORD_CHARS = 12_000


@dataclass(frozen=True)
class NarrativeOutcome:
    """How the Narrative Job ended."""

    kind: Literal["written", "failed"]
    detail: str

    @property
    def exit_code(self) -> int:
        """A Job that writes no Narrative exits non-zero and leaves the Page's status alone."""
        return 0 if self.kind == "written" else 1


async def run_narrative(
    settings: Settings,
    *,
    provider: ModelProvider | None = None,
    client: ToolClient | None = None,
) -> NarrativeOutcome:
    """Read the Page record, make one model call, and post the Narrative."""
    selected = provider or create_provider(settings)
    tools = client or ToolClient(
        settings.server_url,
        settings.page,
        settings.page_uid,
        settings.token_file,
    )
    try:
        record = await tools.narrative_record()
        request = ModelRequest(
            stage=Stage.NARRATIVE,
            system=prompts.narrative_system_prompt(),
            messages=(
                Message(
                    role="user",
                    content=prompts.narrative_task_prompt(
                        record.target.label,
                        present(new_nonce(), "page record", {"text": record.text}, limit=_MAX_RECORD_CHARS),
                    ),
                ),
            ),
            tools=(),
            max_output_tokens=NARRATIVE_MAX_OUTPUT_TOKENS,
        )
        response = await selected.complete(request)
        narrative = " ".join(response.text.split())
        if not narrative:
            return NarrativeOutcome("failed", "the model returned no Narrative")
        await tools.post_narrative(narrative)
        return NarrativeOutcome("written", "the Narrative was recorded")
    finally:
        if client is None:
            await tools.aclose()
        close = getattr(selected, "aclose", None)
        if provider is None and close is not None:
            await close()


def main(env: Mapping[str, str] | None = None) -> int:
    """Run the Narrative Job and return its exit code without printing a credential."""
    environment = os.environ if env is None else env
    try:
        settings = Settings.from_env(environment)
    except ConfigError as error:
        log.error("invalid Narrative configuration: %s", error)
        return 1
    if settings.task != TASK_NARRATIVE:
        log.error("the Narrative Job requires %s=%s", "PAGERLESS_TASK", TASK_NARRATIVE)
        return 1
    try:
        outcome = asyncio.run(run_narrative(settings))
    except ServerRefusedError as error:
        log.error("pagerless-server refused the Narrative Job: %s", error)
        return 1
    except ServerError as error:
        log.error("pagerless-server is unavailable: %s", error)
        return 1
    except Exception as error:
        log.error("the Narrative failed (%s)", type(error).__name__)
        return 1
    log.info("narrative finished: %s", outcome.kind)
    return outcome.exit_code
