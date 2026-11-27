"""The version of the packaged agent service."""

from importlib.metadata import PackageNotFoundError, version

try:
    __version__ = version("pagerless-agent")
except PackageNotFoundError:  # pragma: no cover - only when running from an uninstalled tree
    __version__ = "0.0.0+unknown"
