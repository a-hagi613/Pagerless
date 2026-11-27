"""The private wire contract between the Go service and the Python agent.

The Go service is the only client. Request models tolerate unknown fields so a newer Go
release can add one; response models are built here and serialized with exactly the names
the contract documents.

All JSON names are camelCase.
"""

from __future__ import annotations

import hashlib
import json
from typing import Annotated, Any, Literal

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    StringConstraints,
    ValidationError,
    field_validator,
    model_validator,
)

# Bounds the service enforces before anything reaches the model.
MAX_MESSAGES = 100
MAX_MESSAGE_CHARS = 32_768
MAX_CONVERSATION_CHARS = 200_000
MAX_TOOLS = 64
MAX_TOOL_SCHEMA_CHARS = 32_768
MAX_TOOL_DESCRIPTION_CHARS = 4_000
MAX_ANSWER_CHARS = 8_000
MAX_CITATIONS = 50
MAX_POINTER_CHARS = 512
MAX_PROPOSAL_FILES = 5
MAX_FILE_PATH_CHARS = 500
MAX_FILE_CONTENT_CHARS = 64 * 1024
MAX_ERROR_CHARS = 500
MAX_CALLBACK_URL_CHARS = 2_048

# Tools the agent implements itself. A forwarded MCP tool may not shadow one.
RESERVED_TOOLS = frozenset({"submit_answer", "read_playbook"})

_IDENTIFIER = r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$"
_TOOL_NAME = r"^[A-Za-z][A-Za-z0-9_.-]{0,63}$"
_NAMESPACE = r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
_KIND = r"^[A-Za-z][A-Za-z0-9.]{0,63}$"
_OBJECT_NAME = r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$"
_BASE_SHA = r"^[0-9a-fA-F]{7,64}$"


def to_camel(name: str) -> str:
    """Return the contract's JSON name for a Python field name."""
    head, *rest = name.split("_")
    return head + "".join(part[:1].upper() + part[1:] for part in rest)


class _Request(BaseModel):
    """A Go request: unknown fields are ignored for forward compatibility."""

    model_config = ConfigDict(
        extra="ignore",
        frozen=True,
        alias_generator=to_camel,
        populate_by_name=True,
    )


class _Response(BaseModel):
    """A Python response: every field is deliberate."""

    model_config = ConfigDict(
        extra="forbid",
        frozen=True,
        alias_generator=to_camel,
        populate_by_name=True,
    )


class TurnMessage(_Request):
    """One message of the Page conversation."""

    role: Literal["user", "assistant"]
    content: Annotated[str, StringConstraints(max_length=MAX_MESSAGE_CHARS)]
    turn_id: Annotated[str, StringConstraints(pattern=_IDENTIFIER)] | None = None


class TargetHint(_Request):
    """The optional Kubernetes object the operator named."""

    kind: Annotated[str, StringConstraints(pattern=_KIND)]
    name: Annotated[str, StringConstraints(pattern=_OBJECT_NAME)]
    namespace: Annotated[str, StringConstraints(pattern=_NAMESPACE)] | None = None


class ToolDefinition(_Request):
    """One read-only tool from the Go tool policy."""

    name: Annotated[str, StringConstraints(pattern=_TOOL_NAME)]
    description: Annotated[str, StringConstraints(max_length=MAX_TOOL_DESCRIPTION_CHARS)] = ""
    input_schema: dict[str, Any] = Field(default_factory=dict, alias="inputSchema")

    @field_validator("name")
    @classmethod
    def _not_reserved(cls, value: str) -> str:
        if value in RESERVED_TOOLS:
            raise ValueError("is reserved by the agent")
        return value

    @field_validator("input_schema")
    @classmethod
    def _bounded_schema(cls, value: dict[str, Any]) -> dict[str, Any]:
        try:
            encoded = json.dumps(value, sort_keys=True, separators=(",", ":"))
        except (TypeError, ValueError) as error:
            raise ValueError("must be a JSON object") from error
        if len(encoded) > MAX_TOOL_SCHEMA_CHARS:
            raise ValueError(f"must be at most {MAX_TOOL_SCHEMA_CHARS} characters of JSON")
        return value


class TurnLimits(_Request):
    """Optional lower ceilings for one turn. Values above the service ceiling are refused."""

    tool_calls: Annotated[int, Field(ge=1, le=1_000)] | None = None
    model_calls: Annotated[int, Field(ge=1, le=1_000)] | None = None
    input_tokens: Annotated[int, Field(ge=1, le=100_000_000)] | None = None
    seconds: Annotated[float, Field(ge=0.1, le=86_400)] | None = None


class TurnRequest(_Request):
    """The body of POST /turn."""

    page_id: Annotated[str, StringConstraints(pattern=_IDENTIFIER)]
    turn_id: Annotated[str, StringConstraints(pattern=_IDENTIFIER)]
    messages: Annotated[list[TurnMessage], Field(min_length=1, max_length=MAX_MESSAGES)]
    namespace: Annotated[str, StringConstraints(pattern=_NAMESPACE)] | None = None
    target: TargetHint | None = None
    tools: Annotated[list[ToolDefinition], Field(max_length=MAX_TOOLS)] = Field(default_factory=list)
    callback_url: Annotated[str, StringConstraints(min_length=1, max_length=MAX_CALLBACK_URL_CHARS)]
    limits: TurnLimits | None = None

    @model_validator(mode="after")
    def _consistent(self) -> TurnRequest:
        if self.messages[-1].role != "user" or not self.messages[-1].content.strip():
            raise ValueError("the last message must be a non-blank user message")
        total = sum(len(message.content) for message in self.messages)
        if total > MAX_CONVERSATION_CHARS:
            raise ValueError(f"the conversation must be at most {MAX_CONVERSATION_CHARS} characters")
        names = [tool.name for tool in self.tools]
        if len(set(names)) != len(names):
            raise ValueError("two tools share a name")
        if (
            self.target is not None
            and self.namespace is not None
            and self.target.namespace is not None
            and self.target.namespace != self.namespace
        ):
            raise ValueError("namespace conflicts with target.namespace")
        return self

    def effective_namespace(self) -> str | None:
        """Return the operator's namespace hint, whichever field carried it."""
        if self.namespace is not None:
            return self.namespace
        return self.target.namespace if self.target is not None else None

    def fingerprint(self) -> str:
        """Return a stable digest of the request, used for replay and conflict detection."""
        encoded = json.dumps(
            self.model_dump(mode="json", by_alias=True, exclude_none=False),
            sort_keys=True,
            separators=(",", ":"),
        )
        return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


class Citation(_Response):
    """A reference to one tool result the agent actually received this turn."""

    call_id: Annotated[str, StringConstraints(min_length=1, max_length=128)]
    pointer: Annotated[str, StringConstraints(min_length=1, max_length=MAX_POINTER_CHARS)] | None = None


class ProposalFile(_Response):
    """One whole-file change in a source proposal."""

    path: Annotated[str, StringConstraints(min_length=1, max_length=MAX_FILE_PATH_CHARS)]
    content: Annotated[str, StringConstraints(min_length=1, max_length=MAX_FILE_CONTENT_CHARS)]


class Proposal(_Response):
    """A source change the Go service may validate and publish as a draft pull request."""

    base_sha: Annotated[str, StringConstraints(pattern=_BASE_SHA)]
    files: Annotated[list[ProposalFile], Field(min_length=1, max_length=MAX_PROPOSAL_FILES)]


class TurnUsage(_Response):
    """What the turn spent, including transport and dialect retries."""

    input_tokens: Annotated[int, Field(ge=0)] = 0
    output_tokens: Annotated[int, Field(ge=0)] = 0
    model_calls: Annotated[int, Field(ge=0)] = 0
    tool_calls: Annotated[int, Field(ge=0)] = 0


class TurnError(_Response):
    """A stable machine code and a short, sanitized explanation."""

    code: Annotated[str, StringConstraints(pattern=r"^[a-z][a-z0-9_]{0,63}$")]
    message: Annotated[str, StringConstraints(min_length=1, max_length=MAX_ERROR_CHARS)]


class TurnResult(_Response):
    """The terminal answer to POST /turn."""

    status: Literal["completed", "stopped", "failed"]
    content: Annotated[str, StringConstraints(max_length=MAX_ANSWER_CHARS)] = ""
    citations: Annotated[list[Citation], Field(max_length=MAX_CITATIONS)] = Field(default_factory=list)
    proposal: Proposal | None = None
    error: TurnError | None = None
    usage: TurnUsage = Field(default_factory=TurnUsage)

    @model_validator(mode="after")
    def _status_consistent(self) -> TurnResult:
        if self.status == "failed" and self.error is None:
            raise ValueError("a failed result requires an error")
        if self.status != "failed" and self.error is not None:
            raise ValueError("only a failed result may carry an error")
        if self.status == "failed" and self.proposal is not None:
            raise ValueError("a failed result cannot carry a proposal")
        return self


class AnswerPayload(_Response):
    """The arguments of the model's `submit_answer` tool call."""

    content: Annotated[str, StringConstraints(min_length=1, max_length=MAX_ANSWER_CHARS)]
    citations: Annotated[list[Citation], Field(max_length=MAX_CITATIONS)] = Field(default_factory=list)
    proposal: Proposal | None = None


class CallbackResponse(_Response):
    """The Go service's answer to one tool callback.

    Unknown fields are ignored: Go may echo the callId or add a bounded detail without
    breaking this agent.
    """

    model_config = ConfigDict(
        extra="ignore",
        frozen=True,
        alias_generator=to_camel,
        populate_by_name=True,
    )

    status: Literal["ok", "error", "denied", "truncated", "timed_out", "budget_exhausted"]
    result: Any = None
    redacted: bool = False
    truncated: bool = False


class StopResult(_Response):
    """The body of POST /turns/{turnId}/stop. Repeated calls are safe."""

    status: Literal["stopped", "finished", "unknown"]


class ReadyCheck(_Response):
    """One readiness check. Python owns only the model check."""

    name: str
    ok: bool
    reason: str


class ReadyResult(_Response):
    """The body of GET /readyz. HTTP 200 when ready, 503 when degraded."""

    status: Literal["ready", "degraded"]
    checks: list[ReadyCheck]


def encode(model: BaseModel, *, exclude_none: bool = True) -> dict[str, Any]:
    """Return the contract JSON body for a model."""
    return model.model_dump(mode="json", by_alias=True, exclude_none=exclude_none)


def validation_message(error: ValidationError) -> str:
    """Return a bounded, non-secret summary of a request validation failure."""
    parts: list[str] = []
    for item in error.errors()[:3]:
        location = ".".join(str(part) for part in item["loc"]) or "body"
        parts.append(f"{location}: {item['msg']}")
    return "; ".join(parts)[:MAX_ERROR_CHARS]
