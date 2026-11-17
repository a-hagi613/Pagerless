"""Replay: the recording format, the provider that plays it back, and the one that records it.

A recording is a JSON document that holds the model responses of one run, in
order, with their volatile identifiers normalized to placeholders (see
`pagerless_agent.identifiers`). Replay plays the responses back in sequence and
resolves the placeholders from the current Context Bundle. Tool calls still
execute for real, so replayed tool arguments must name the current pods.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, ValidationError, model_validator

from pagerless_agent.identifiers import IdentifierMap, ReplayError, check_placeholders, find_residuals
from pagerless_agent.model import ModelProvider, ModelRequest, ModelResponse, Stage

FORMAT = "pagerless.dev/replay"
VERSION = 1


class RecordedTurn(BaseModel):
    """One recorded model call: the stage that made it and the normalized response."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    stage: Stage
    response: ModelResponse


class Recording(BaseModel):
    """The model responses of one run of a Scenario.

    `workload` is the name of the Workload the run investigated. It lets the
    recording be checked for pod names that normalization missed.
    """

    model_config = ConfigDict(extra="forbid", frozen=True)

    format: Literal["pagerless.dev/replay"] = "pagerless.dev/replay"
    version: Literal[1] = 1
    scenario: str
    model: str
    workload: str
    turns: tuple[RecordedTurn, ...]

    @model_validator(mode="after")
    def _is_replayable(self) -> Recording:
        """Refuse a recording with a malformed placeholder or a volatile identifier left in it."""
        problems: list[str] = []
        for number, turn in enumerate(self.turns, start=1):
            payload = turn.response.model_dump(mode="json")
            for text in _strings(payload):
                try:
                    check_placeholders(text)
                except ReplayError as error:
                    problems.append(f"turn {number}: {error}")
                problems += [
                    f"turn {number}: the volatile identifier {found!r} was not normalized"
                    for found in find_residuals(text, self.workload)
                ]
        if problems:
            raise ValueError("; ".join(problems))
        return self

    @classmethod
    def load(cls, path: Path) -> Recording:
        """Read a recording, refusing one that is malformed, of another version, or not replayable."""
        try:
            raw = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise ReplayError(f"cannot read the recording {path}: {error}") from error
        if not isinstance(raw, dict) or raw.get("format") != FORMAT:
            raise ReplayError(f"{path} is not a Replay recording (expected format {FORMAT!r})")
        if raw.get("version") != VERSION:
            raise ReplayError(
                f"{path} has version {raw.get('version')!r}; this agent reads version {VERSION}"
            )
        try:
            return cls.model_validate(raw)
        except ValidationError as error:
            raise ReplayError(f"{path} is not a valid recording: {error}") from error

    def save(self, path: Path) -> None:
        """Write the recording as stable, reviewable JSON."""
        document = self.model_dump(mode="json")
        text = json.dumps(document, indent=2, ensure_ascii=False) + "\n"
        path.write_bytes(text.encode("utf-8"))


def _strings(value: Any) -> list[str]:
    """Return every string in a JSON value, keys included."""
    if isinstance(value, str):
        return [value]
    if isinstance(value, list):
        return [s for item in value for s in _strings(item)]
    if isinstance(value, dict):
        return [s for key, item in value.items() for s in (_strings(key) + _strings(item))]
    return []


class ReplayProvider:
    """Plays a recording back, one response per request, in order."""

    def __init__(self, recording: Recording) -> None:
        self._recording = recording
        self._next = 0

    @property
    def remaining(self) -> int:
        """The number of recorded responses not yet played."""
        return len(self._recording.turns) - self._next

    async def complete(self, request: ModelRequest) -> ModelResponse:
        """Return the next recorded response, resolved against the request's Context Bundle."""
        if self._next >= len(self._recording.turns):
            raise ReplayError(
                f"the recording of scenario {self._recording.scenario!r} is exhausted after "
                f"{len(self._recording.turns)} responses, but the agent asked for another ({request.stage})"
            )
        turn = self._recording.turns[self._next]
        if turn.stage != request.stage:
            raise ReplayError(
                f"the agent diverged from the recording of scenario {self._recording.scenario!r}: "
                f"response {self._next + 1} was recorded for the {turn.stage} stage, "
                f"but the agent asked for the {request.stage} stage"
            )
        self._next += 1
        identifiers = IdentifierMap(request.bundle)
        payload = identifiers.resolve_json(turn.response.model_dump(mode="json"))
        return ModelResponse.model_validate(payload)


class RecordingProvider:
    """Wraps a live provider and records its responses, normalized, for Replay."""

    def __init__(self, inner: ModelProvider, *, scenario: str, model: str) -> None:
        self._inner = inner
        self._scenario = scenario
        self._model = model
        self._turns: list[RecordedTurn] = []
        self._workload: str | None = None
        self._call_ids = 0

    async def complete(self, request: ModelRequest) -> ModelResponse:
        """Answer the request with the wrapped provider, and record the answer."""
        response = await self._inner.complete(request)
        identifiers = IdentifierMap(request.bundle)
        payload = identifiers.normalize_json(response.model_dump(mode="json"))
        calls = []
        for call in payload["tool_calls"]:
            # Provider call ids are random. Sequential ids make a recording reproducible.
            self._call_ids += 1
            calls.append({**call, "id": f"call_{self._call_ids}"})
        payload["tool_calls"] = calls
        self._turns.append(RecordedTurn(stage=request.stage, response=ModelResponse.model_validate(payload)))
        self._workload = request.bundle.workload.name
        return response

    def recording(self) -> Recording:
        """Return what was recorded. It raises ReplayError if it could not be replayed."""
        if self._workload is None:
            raise ReplayError("nothing was recorded")
        try:
            return Recording(
                scenario=self._scenario, model=self._model, workload=self._workload, turns=tuple(self._turns)
            )
        except ValidationError as error:
            raise ReplayError(f"the run cannot be recorded for Replay: {error}") from error
