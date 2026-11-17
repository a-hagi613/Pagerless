"""The entry point of the short-lived investigation Job."""

import logging

from pagerless_agent.runtime import main

if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
    raise SystemExit(main())
