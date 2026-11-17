"""The Narrative: one short descriptive model call after Verification (ADR-0006).

`pagerless-server` starts a Narrative Job once a Triage reaches a terminal
Verification outcome. The Job reads the Triage record from the server, makes one
model call, and posts the Narrative back. It never writes a phase, a check, or a
Finding, so a Narrative that fails or times out leaves the outcome as it was.

The Narrative has no Context Bundle, and Replay resolves placeholders only from
one. Its recording therefore carries no placeholders: the prompt asks for prose
without volatile identifiers, and the recording format refuses a response that
still holds a pod name, a full commit SHA, or a timestamp.
"""

from __future__ import annotations

import asyncio
import logging
import os
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Literal

import httpx

from pagerless_agent import prompts
from pagerless_agent.contract import NarrativeRecord
from pagerless_agent.identifiers import ReplayError
from pagerless_agent.model import Message, ModelProvider, ModelRequest, Stage
from pagerless_agent.replay import RecordingProvider
from pagerless_agent.runtime import create_provider, save_recording
from pagerless_agent.server import ServerClient, ServerError, ServerRefusedError
from pagerless_agent.settings import ConfigError, NarrativeSettings
from pagerless_agent.tools import new_nonce, present

log = logging.getLogger(__name__)

# The one call's output cap, which bounds the cost of the Narrative (ADR-0010).
NARRATIVE_MAX_OUTPUT_TOKENS = 2000
# The Triage record is rendered within this many characters, which bounds the prompt.
_MAX_RECORD_CHARS = 12_000


@dataclass(frozen=True)
class NarrativeOutcome:
    """How the Narrative Job ended."""

    kind: Literal["written", "failed"]
    detail: str

    @property
    def exit_code(self) -> int:
        """A Job that writes no Narrative exits non-zero and leaves the Triage's status alone."""
        return 0 if self.kind == "written" else 1


async def run_narrative(
    settings: NarrativeSettings,
    *,
    server_http: httpx.AsyncClient | None = None,
    provider: ModelProvider | None = None,
) -> NarrativeOutcome:
    """Read the Triage record, make one model call, and post the Narrative."""
    selected = provider or create_provider(settings.provider, settings.replay_file)
    server = ServerClient(settings.server_url, settings.triage, settings.token_file, http=server_http)
    recorder = None
    try:
        record = await server.narrative_record()
        recorder = _recorder(settings, selected, record)
        request = ModelRequest(
            stage=Stage.NARRATIVE,
            system=prompts.narrative_system_prompt(),
            messages=(
                Message(
                    role="user",
                    content=prompts.narrative_task_prompt(
                        _workload_label(record),
                        present(new_nonce(), "triage record", record.text, limit=_MAX_RECORD_CHARS),
                    ),
                ),
            ),
            tools=(),
            max_output_tokens=NARRATIVE_MAX_OUTPUT_TOKENS,
        )
        response = await (recorder or selected).complete(request)
        narrative = " ".join(response.text.split())
        if not narrative:
            return NarrativeOutcome("failed", "the model returned no Narrative")
        await server.post_narrative(narrative)
        if recorder is not None:
            save_recording(recorder.recording(), settings.recording_file)
        return NarrativeOutcome("written", "the Narrative was recorded")
    finally:
        await server.aclose()
        close = getattr(selected, "aclose", None)
        if close is not None:
            await close()


def _recorder(
    settings: NarrativeSettings, provider: ModelProvider, record: NarrativeRecord
) -> RecordingProvider | None:
    """Wrap a live provider when this Job was asked to capture a recording."""
    if settings.recording_file is None:
        return None
    from pagerless_agent.providers import ANTHROPIC_MODEL, BEDROCK_MODEL

    model = ANTHROPIC_MODEL if settings.provider == "anthropic" else BEDROCK_MODEL
    return RecordingProvider(provider, scenario="narrative", model=model, workload=record.workload.name)


def _workload_label(record: NarrativeRecord) -> str:
    workload = record.workload
    return f"{workload.kind} {workload.namespace}/{workload.name}"


def main(env: Mapping[str, str] | None = None) -> int:
    """Run the Narrative Job and return its exit code without printing credentials."""
    try:
        settings = NarrativeSettings.from_env(os.environ if env is None else env)
        outcome = asyncio.run(run_narrative(settings))
    except ConfigError as error:
        log.error("invalid Narrative configuration: %s", error)
        return 1
    except ReplayError:
        log.error(
            "cannot read or validate the Narrative Replay recording; set "
            "PAGERLESS_NARRATIVE_REPLAY_FILE to a captured recording"
        )
        return 1
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
