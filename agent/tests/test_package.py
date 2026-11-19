"""The shipped package: its dependencies, its imports, and the files that left it."""

from __future__ import annotations

import importlib.util
import tomllib
from pathlib import Path

import pagerless_agent

AGENT = Path(__file__).resolve().parents[1]
REMOVED = (
    "providers",
    "mcp",
    "keycloak",
    "live_context",
    "replay",
    "identifiers",
    "pricing",
    "settings",
    "server",
    "bundle",
)


def test_the_package_imports() -> None:
    assert pagerless_agent.__version__ == "0.1.0"


def test_no_recording_or_provider_locked_module_survives() -> None:
    for name in REMOVED:
        assert importlib.util.find_spec(f"pagerless_agent.{name}") is None, name


def test_the_declared_dependencies_are_the_universal_ones() -> None:
    project = tomllib.loads((AGENT / "pyproject.toml").read_text(encoding="utf-8"))
    dependencies = {requirement.split("==")[0] for requirement in project["project"]["dependencies"]}

    assert dependencies == {"httpx", "langgraph", "openai", "pydantic", "pyyaml"}
    for forbidden in ("anthropic", "boto3", "httpx2", "mcp"):
        assert forbidden not in dependencies


def test_the_lockfile_pins_the_universal_client() -> None:
    lock = tomllib.loads((AGENT / "uv.lock").read_text(encoding="utf-8"))
    pins = {
        package["name"]: package["version"]
        for package in lock["package"]
        if package["name"] in {"openai", "langgraph", "pydantic", "pyyaml", "httpx"}
    }

    assert pins["openai"] == "3.55.1"
    assert pins["langgraph"] == "1.2.22"
    assert pins["pydantic"] == "2.14.3"
    assert pins["pyyaml"] == "6.0.3"
    assert pins["httpx"] == "0.28.1"


def test_the_image_carries_no_recording_and_starts_the_agent() -> None:
    dockerfile = (AGENT.parent / "Dockerfile.pagerless-agent").read_text(encoding="utf-8")

    assert "recordings" not in dockerfile
    assert 'ENTRYPOINT ["python", "-m", "pagerless_agent"]' in dockerfile


def test_only_the_broker_and_the_model_client_construct_an_http_client() -> None:
    sources = {
        path.name: path.read_text(encoding="utf-8")
        for path in (AGENT / "src" / "pagerless_agent").glob("*.py")
    }
    broker_clients = [name for name, text in sources.items() if "httpx.AsyncClient(" in text]
    sdk_clients = [name for name, text in sources.items() if "AsyncOpenAI(" in text]

    assert broker_clients == ["toolclient.py"]
    assert sdk_clients == ["openai_compat.py"]


def test_the_two_base_url_variables_are_the_only_ones() -> None:
    from pagerless_agent.config import BASE_URL_VARIABLES

    assert BASE_URL_VARIABLES == ("PAGERLESS_SERVER_URL", "PAGERLESS_MODEL_BASE_URL")
