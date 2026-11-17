"""Run one agent Job with live read-only evidence and a selected model provider."""

from __future__ import annotations

import asyncio
import logging
import os
import tempfile
from collections.abc import Callable, Mapping
from contextlib import AbstractAsyncContextManager, AsyncExitStack
from pathlib import Path

import httpx
from mcp import Client

from pagerless_agent.agent import Agent, Outcome
from pagerless_agent.identifiers import ReplayError
from pagerless_agent.keycloak import FederatedTokenSource, TokenError
from pagerless_agent.live_context import MCPContextSource
from pagerless_agent.mcp import MCPServerConfig, MCPTools, connect
from pagerless_agent.model import ModelProvider
from pagerless_agent.replay import Recording, RecordingProvider, ReplayProvider
from pagerless_agent.server import ServerClient
from pagerless_agent.settings import (
    ENV_TASK,
    TASK_INVESTIGATE,
    TASK_NARRATIVE,
    ConfigError,
    RuntimeSettings,
)

log = logging.getLogger(__name__)


def create_provider(provider: str, replay_file: Path) -> ModelProvider:
    """Select Replay by default; live providers use the SDK's environment credential discovery."""
    if provider == "replay":
        return ReplayProvider(Recording.load(replay_file))
    from pagerless_agent.providers import AnthropicProvider, BedrockProvider

    if provider == "anthropic":
        if not os.environ.get("ANTHROPIC_API_KEY"):
            raise ConfigError("ANTHROPIC_API_KEY is required for the anthropic provider")
        return AnthropicProvider()
    return BedrockProvider(aws_region=os.environ.get("AWS_REGION"), aws_profile=os.environ.get("AWS_PROFILE"))


async def run(
    settings: RuntimeSettings,
    *,
    client_factory: Callable[[MCPServerConfig], AbstractAsyncContextManager[Client]] = connect,
    server_http: httpx.AsyncClient | None = None,
    provider: ModelProvider | None = None,
    token_source: FederatedTokenSource | None = None,
) -> Outcome:
    """Investigate, report, and close every connection; Replay still executes MCP reads."""
    selected = provider or create_provider(settings.provider, settings.replay_file)
    recorder = None
    if settings.recording_file is not None:
        if settings.provider == "replay":
            raise ConfigError("Replay cannot produce a live recording")
        from pagerless_agent.providers import ANTHROPIC_MODEL, BEDROCK_MODEL

        model = ANTHROPIC_MODEL if settings.provider == "anthropic" else BEDROCK_MODEL
        recorder = RecordingProvider(selected, scenario="oom", model=model)
    investigation = settings.investigation
    gateway = token_source or FederatedTokenSource(
        settings.keycloak.token_url, settings.keycloak.client_id, settings.keycloak.token_file
    )
    try:
        async with AsyncExitStack() as stack:
            if token_source is None:
                stack.push_async_callback(gateway.aclose)
            server = ServerClient(
                investigation.server_url,
                investigation.target.triage,
                investigation.token_file,
                http=server_http,
            )
            stack.push_async_callback(server.aclose)
            tools = await stack.enter_async_context(
                MCPTools(
                    settings.servers(gateway),
                    namespace=investigation.target.namespace,
                    owner=settings.sandbox_owner,
                    repository=settings.sandbox_repository,
                    application=settings.application,
                    client_factory=client_factory,
                )
            )
            outcome = await Agent(
                target=investigation.target,
                provider=recorder or selected,
                tools=tools,
                source=MCPContextSource(tools),
                server=server,
            ).run()
            if recorder is not None and outcome.kind == "reported":
                save_recording(recorder.recording(), settings.recording_file)
            return outcome
    finally:
        close = getattr(selected, "aclose", None)
        if close is not None:
            await close()


def save_recording(recording: Recording, destination: Path | None) -> None:
    if destination is None:
        return
    # A partial or non-normalizable recording must never replace a valid artifact.
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(
            dir=destination.parent, prefix=".pagerless-recording-", delete=False
        ) as file:
            temporary = Path(file.name)
        recording.save(temporary)
        temporary.replace(destination)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def main(env: Mapping[str, str] | None = None) -> int:
    """Run the Job that the environment selects, and return its exit code."""
    environment = os.environ if env is None else env
    task = environment.get(ENV_TASK, TASK_INVESTIGATE)
    if task == TASK_NARRATIVE:
        from pagerless_agent.narrative import main as narrative_main

        return narrative_main(environment)
    if task != TASK_INVESTIGATE:
        log.error("invalid agent configuration: %s must be investigate or narrative", ENV_TASK)
        return 1
    try:
        settings = RuntimeSettings.from_env(environment)
        outcome = asyncio.run(run(settings))
    except ConfigError as error:
        log.error("invalid agent configuration: %s", error)
        return 1
    except ReplayError:
        log.error(
            "cannot read or validate the Replay recording; set PAGERLESS_REPLAY_FILE to a captured recording"
        )
        return 1
    except TokenError as error:
        log.error("cannot authenticate to the MCP gateway through Keycloak: %s", error)
        return 1
    except Exception as error:
        log.error("agent startup or recording failed (%s)", type(error).__name__)
        return 1
    log.info("agent finished: %s", outcome.kind)
    return outcome.exit_code
