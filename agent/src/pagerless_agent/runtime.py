"""Start the private investigation agent service."""

from __future__ import annotations

import logging
import os
from collections.abc import Mapping

import uvicorn

from pagerless_agent.config import ConfigError, ServiceSettings
from pagerless_agent.server import create_app

log = logging.getLogger(__name__)


def main(env: Mapping[str, str] | None = None) -> int:
    """Read the environment, start the service, and return a process exit code."""
    environment = os.environ if env is None else env
    try:
        settings = ServiceSettings.from_env(environment)
    except ConfigError as error:
        log.error("invalid agent configuration: %s", error)
        return 2
    app = create_app(settings)
    server = uvicorn.Server(
        uvicorn.Config(
            app,
            host=settings.host,
            port=settings.port,
            log_level="info",
            access_log=False,
            timeout_graceful_shutdown=max(1, int(settings.shutdown_timeout_s)),
        )
    )
    server.run()
    return 0
