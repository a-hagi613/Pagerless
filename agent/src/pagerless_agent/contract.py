"""The wire contract between the agent and pagerless-server.

These models mirror the v2 Page API and the investigation endpoints the server exposes.
The server decodes strictly and rejects unknown fields, so every model forbids extras.

Vocabulary is the v2 glossary, exactly: a `Page` is one investigation, a `Target` is the
object under investigation, a `Finding` cites one recorded tool call, a `Diagnosis` is
the root cause with its Findings, and a `Proposal` is either a `FilePatch` of one to five
whole files against the Page's `Target Repository`, or a `DiagnosisOnly` conclusion.
"""

from __future__ import annotations

from typing import Annotated, Any, Literal

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    StringConstraints,
    TypeAdapter,
    ValidationError,
    field_validator,
    model_validator,
)

from pagerless_agent.model import ToolSpec
from pagerless_agent.structured import PointerError, resolve_pointer

# Limits the server enforces, repeated here so a malformed answer is caught before the report.
MAX_DIAGNOSIS_LENGTH = 4000
MAX_NARRATIVE_LENGTH = 4000
MAX_FINDING_SOURCE_LENGTH = 300
MAX_FINDING_SUMMARY_LENGTH = 500
MAX_STEP_SUMMARY_LENGTH = 200
MAX_TOOL_NAME_LENGTH = 64
# The number of Findings a Page keeps (`v1alpha1.MaxFindings`).
MAX_FINDINGS = 20
MAX_PROPOSAL_FILES = 5
MAX_FILE_PATH_LENGTH = 500
MAX_FILE_CONTENT_CHARS = 64 * 1024
MAX_RATIONALE_LENGTH = 1000
MAX_RECOMMENDATION_LENGTH = 2000
MAX_INPUT_TOKENS = 10_000_000
MAX_OUTPUT_TOKENS = 1_000_000

FindingKind = Literal["Log", "Metric", "Event", "Commit", "Object"]
StepKind = Literal["ToolCall", "Finding", "Progress", "SuspiciousOutput"]
ProposalKind = Literal["FilePatch", "DiagnosisOnly"]
DiagnosisOnlyReason = Literal[
    "external-cause",
    "unmanaged-resource",
    "infrastructure",
    "no-git-repository",
    "insufficient-evidence",
    "operator-action-required",
]
Confidence = Literal["exact", "configured", "identity", "name-only"]

_MANIFEST_SUFFIXES = (".yaml", ".yml")
_ALLOWED_PATH_CHARACTERS = frozenset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-/")


class _Frozen(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)


def _to_camel(name: str) -> str:
    head, *rest = name.split("_")
    return head + "".join(part[:1].upper() + part[1:] for part in rest)


class _Wire(_Frozen):
    """A model whose JSON names are the server's camelCase names."""

    model_config = ConfigDict(
        extra="forbid",
        frozen=True,
        alias_generator=_to_camel,
        populate_by_name=True,
    )


def _not_blank(value: str) -> str:
    if not value.strip():
        raise ValueError("must not be blank")
    return value


class Page(_Wire):
    """The Page under investigation."""

    name: Annotated[str, StringConstraints(min_length=1, max_length=253)]
    uid: Annotated[str, StringConstraints(min_length=1, max_length=128)]
    namespace: Annotated[str, StringConstraints(min_length=1, max_length=63)]


class Target(_Wire):
    """The object under investigation. A Node Target is cluster-scoped and has no namespace."""

    kind: Literal[
        "Deployment",
        "StatefulSet",
        "DaemonSet",
        "Job",
        "CronJob",
        "Pod",
        "PersistentVolumeClaim",
        "Node",
    ]
    name: Annotated[str, StringConstraints(min_length=1, max_length=253)]
    namespace: Annotated[str, StringConstraints(min_length=1, max_length=63)] | None = None
    uid: Annotated[str, StringConstraints(min_length=1, max_length=128)] | None = None
    generation: Annotated[int, Field(ge=0)] | None = None

    @model_validator(mode="after")
    def _namespace_matches_kind(self) -> Target:
        if self.kind == "Node" and self.namespace is not None:
            raise ValueError("a Node Target is cluster-scoped and has no namespace")
        if self.kind != "Node" and self.namespace is None:
            raise ValueError("a namespaced Target requires a namespace")
        return self

    @property
    def label(self) -> str:
        """Return the Target as the prompts name it."""
        if self.namespace is None:
            return f"{self.kind} {self.name}"
        return f"{self.kind} {self.namespace}/{self.name}"


class TargetRepository(_Wire):
    """The one repository a Proposal may change."""

    owner: Annotated[str, StringConstraints(pattern=r"^[A-Za-z0-9_.-]+$")]
    name: Annotated[str, StringConstraints(pattern=r"^[A-Za-z0-9_.-]+$")]
    ref: Annotated[str, StringConstraints(min_length=1, max_length=255)]

    @property
    def slug(self) -> str:
        """Return `owner/name`, the form GitHub uses."""
        return f"{self.owner}/{self.name}"


class Scope(_Wire):
    """What the server will allow: namespaces, repositories, connectors, and kinds."""

    namespaces: tuple[str, ...] = ()
    repositories: tuple[TargetRepository, ...] = ()
    connectors: tuple[str, ...] = ()
    kinds: tuple[str, ...] = ()


class PageLimits(_Wire):
    """The budgets the server enforces, read from the investigation manifest."""

    tool_calls: Annotated[int, Field(ge=1, le=1000)] = 15
    model_calls: Annotated[int, Field(ge=1, le=1000)] = 12
    input_tokens: Annotated[int, Field(ge=1, le=100_000_000)] = 300_000
    seconds: Annotated[float, Field(gt=0, le=86_400)] = 300.0


class Trigger(_Wire):
    """What opened the Page: a watcher signal, an alert, or a human request."""

    kind: Annotated[str, StringConstraints(min_length=1, max_length=64)]
    reason: Annotated[str, StringConstraints(max_length=200)] | None = None
    pod: Annotated[str, StringConstraints(max_length=253)] | None = None


class Investigation(_Wire):
    """One response that replaces discovery, bundle gathering, and every MCP handshake.

    The manifest is written by the server, not by the model, so an unknown field is
    ignored rather than fatal: a newer server may add one without breaking an older agent.
    Everything the model produces stays strict.
    """

    model_config = ConfigDict(
        extra="ignore",
        frozen=True,
        alias_generator=_to_camel,
        populate_by_name=True,
    )

    page: Page
    target: Target
    trigger: Trigger | None = None
    scope: Scope
    context: dict[str, Any]
    tools: tuple[ToolSpec, ...]
    limits: PageLimits
    hint: str | None = None
    capabilities: dict[str, Any] | None = None


class Finding(_Wire):
    """One cited piece of evidence.

    `tool_call_id` names a tool call the Page recorded, and `pointer` names the JSON Pointer
    inside that call's stored result. The server verifies both; a fabricated Finding is refused.
    """

    kind: FindingKind
    source: Annotated[str, StringConstraints(min_length=1, max_length=MAX_FINDING_SOURCE_LENGTH)]
    summary: Annotated[str, StringConstraints(min_length=1, max_length=MAX_FINDING_SUMMARY_LENGTH)]
    tool_call_id: Annotated[str, StringConstraints(pattern=r"^[A-Za-z0-9_.:-]{1,128}$")]
    pointer: Annotated[str, StringConstraints(pattern=r"^/(data|meta)(/[^/\s]+)*$")]


class Diagnosis(_Wire):
    """The root cause and the Findings that support it."""

    diagnosis: Annotated[str, StringConstraints(min_length=1, max_length=MAX_DIAGNOSIS_LENGTH)]
    findings: Annotated[list[Finding], Field(min_length=1, max_length=MAX_FINDINGS)]


def _validate_path(value: str) -> str:
    if not value.strip():
        raise ValueError("must not be blank")
    if len(value) > MAX_FILE_PATH_LENGTH:
        raise ValueError(f"must be at most {MAX_FILE_PATH_LENGTH} characters")
    if value.startswith("/") or value.startswith("\\"):
        raise ValueError("must be a relative path")
    if "\\" in value:
        raise ValueError("must use forward slashes")
    if any(character not in _ALLOWED_PATH_CHARACTERS for character in value):
        raise ValueError("must contain only letters, digits, dots, dashes, and slashes")
    segments = value.split("/")
    if any(segment in {"", ".", ".."} for segment in segments):
        raise ValueError("must be a clean path without empty, '.' or '..' segments")
    if segments[0] == ".git" or ".git" in segments:
        raise ValueError("must not touch the repository's .git directory")
    if segments[0] == ".github":
        raise ValueError("must not touch .github, which carries CI and workflow configuration")
    if not value.endswith(_MANIFEST_SUFFIXES):
        raise ValueError("must be a YAML manifest (.yaml or .yml)")
    return value


def _validate_content(value: str) -> str:
    if not value.strip():
        raise ValueError("must not be blank")
    if "\x00" in value:
        raise ValueError("must not contain a NUL byte")
    if "\r" in value:
        raise ValueError("must use LF line endings")
    if len(value) > MAX_FILE_CONTENT_CHARS:
        raise ValueError(f"must be at most {MAX_FILE_CONTENT_CHARS} characters")
    # The contract requires LF-terminated content; one trailing newline is deterministic.
    return value.rstrip("\n") + "\n"


class FileEdit(_Wire):
    """One whole-file replacement in a Proposal."""

    path: Annotated[str, StringConstraints(min_length=1, max_length=MAX_FILE_PATH_LENGTH)]
    updated_content: Annotated[str, StringConstraints(min_length=1, max_length=MAX_FILE_CONTENT_CHARS)]
    rationale: Annotated[str, StringConstraints(min_length=1, max_length=MAX_RATIONALE_LENGTH)]

    _path_policy = field_validator("path")(_validate_path)
    _content_policy = field_validator("updated_content")(_validate_content)
    _rationale_given = field_validator("rationale")(_not_blank)


class FilePatch(_Wire):
    """A Proposal that changes one to five files against the Target Repository."""

    kind: Literal["FilePatch"]
    files: Annotated[list[FileEdit], Field(min_length=1, max_length=MAX_PROPOSAL_FILES)]
    confidence: Confidence | None = None


class DiagnosisOnly(_Wire):
    """A Proposal that concludes that no repository change is the correct remediation."""

    kind: Literal["DiagnosisOnly"]
    reason: DiagnosisOnlyReason
    recommendation: Annotated[str, StringConstraints(min_length=1, max_length=MAX_RECOMMENDATION_LENGTH)]

    _recommendation_given = field_validator("recommendation")(_not_blank)


Proposal = Annotated[FilePatch | DiagnosisOnly, Field(discriminator="kind")]
PROPOSAL: TypeAdapter[Proposal] = TypeAdapter(Proposal)


def normalize_proposal(value: object) -> object:
    """Drop the fields that belong to the other Proposal variant, and every JSON null.

    Strict structured outputs require every property to be present, so a model emits
    `files: null` on a `DiagnosisOnly` and `reason: null` on a `FilePatch`. Those nulls
    carry no information; removing them lets the strict Pydantic variant, with
    `extra="forbid"`, remain the authority on what each variant may contain.
    """
    if not isinstance(value, dict):
        return value
    cleaned = {key: item for key, item in value.items() if item is not None}
    kind = cleaned.get("kind")
    if kind == "FilePatch":
        for key in ("reason", "recommendation"):
            cleaned.pop(key, None)
    elif kind == "DiagnosisOnly":
        for key in ("files", "confidence"):
            cleaned.pop(key, None)
    return cleaned


class Usage(_Wire):
    """The model usage of one investigation, and whether any figure was estimated."""

    input_tokens: Annotated[int, Field(ge=0, le=MAX_INPUT_TOKENS)]
    output_tokens: Annotated[int, Field(ge=0, le=MAX_OUTPUT_TOKENS)]
    model_calls: Annotated[int, Field(ge=0, le=1000)] = 0
    estimated: bool = False


class Capabilities(_Wire):
    """What the endpoint actually supported, so a degraded installation is visible."""

    structured_output: dict[str, str] = Field(default_factory=dict)
    usage_source: Literal["provider", "estimated", "mixed"] = "provider"


class Report(_Wire):
    """The agent's final output: a Diagnosis, its Findings, one Proposal, and how it was produced."""

    page: Page
    diagnosis: str
    findings: list[Finding]
    proposal: Proposal
    usage: Usage
    capabilities: Capabilities


class StepEvent(_Wire):
    """One step of the investigation, posted to the Page's events endpoint."""

    kind: StepKind
    tool: Annotated[str, StringConstraints(max_length=MAX_TOOL_NAME_LENGTH)] | None = None
    summary: Annotated[str, StringConstraints(min_length=1, max_length=MAX_STEP_SUMMARY_LENGTH)]
    finding: Finding | None = None
    tool_call_id: Annotated[str, StringConstraints(max_length=128)] | None = None

    @model_validator(mode="after")
    def _payload_matches_kind(self) -> StepEvent:
        if self.kind == "Finding" and self.finding is None:
            raise ValueError("a Finding step must carry the finding")
        if self.kind != "Finding" and self.finding is not None:
            raise ValueError("only a Finding step may carry a finding")
        return self


class ReportError(_Wire):
    """One reason the server refused a report (`agentapi.ValidationError`)."""

    field: str
    message: str


class NarrativeReport(_Wire):
    """The Narrative Job's answer: the short account of a finished Page."""

    narrative: Annotated[str, StringConstraints(min_length=1, max_length=MAX_NARRATIVE_LENGTH)]


class NarrativeRecord(_Wire):
    """What the server gives a Narrative Job: the Page rendered as text, and its Target."""

    target: Target
    text: Annotated[str, StringConstraints(min_length=1)]


def encode(model: BaseModel) -> dict[str, Any]:
    """Return the JSON body for a wire model, using the server's names and omitting unset fields."""
    return model.model_dump(mode="json", by_alias=True, exclude_none=True)


def validation_errors(error: ValidationError, prefix: str) -> list[ReportError]:
    """Convert a local schema failure into the shape the server uses for its own errors."""
    errors: list[ReportError] = []
    for item in error.errors():
        loc = [str(part) for part in item["loc"]]
        # A discriminated union puts the variant's tag first; the server's paths omit it.
        if loc and loc[0] in {"FilePatch", "DiagnosisOnly"}:
            loc = loc[1:]
        errors.append(ReportError(field=".".join([prefix, *loc]), message=item["msg"]))
    return errors


def citation_problems(findings: list[Finding], results: dict[str, Any]) -> list[str]:
    """Check that every Finding cites a recorded tool call and a pointer that resolves.

    The server performs the authoritative check against its own evidence store. This is the
    local half: it stops a run from reporting a citation it can already see is fabricated.
    """
    problems: list[str] = []
    for index, finding in enumerate(findings):
        result = results.get(finding.tool_call_id)
        if result is None:
            problems.append(
                f"findings[{index}].toolCallId: no tool call named {finding.tool_call_id!r} was made"
            )
            continue
        try:
            resolve_pointer(result, finding.pointer)
        except PointerError as error:
            problems.append(f"findings[{index}].pointer: {error}")
    return problems


def diagnosis_schema() -> dict[str, Any]:
    """Return the JSON Schema the Diagnosis stage asks for, rich in descriptions for weak models."""
    return {
        "type": "object",
        "additionalProperties": False,
        "required": ["diagnosis", "findings"],
        "properties": {
            "diagnosis": {
                "type": "string",
                "maxLength": MAX_DIAGNOSIS_LENGTH,
                "description": (
                    "The root cause of the failure in at most a few sentences. State what failed, "
                    "why, and which evidence proves it."
                ),
            },
            "findings": {
                "type": "array",
                "minItems": 1,
                "maxItems": MAX_FINDINGS,
                "description": "Evidence that supports the Diagnosis. At least one Finding is required.",
                "items": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["kind", "source", "summary", "toolCallId", "pointer"],
                    "properties": {
                        "kind": {
                            "type": "string",
                            "enum": ["Log", "Metric", "Event", "Commit", "Object"],
                        },
                        "source": {
                            "type": "string",
                            "maxLength": MAX_FINDING_SOURCE_LENGTH,
                            "description": "The tool and query that produced the evidence.",
                        },
                        "summary": {
                            "type": "string",
                            "maxLength": MAX_FINDING_SUMMARY_LENGTH,
                            "description": "What the evidence shows, in one sentence.",
                        },
                        "toolCallId": {
                            "type": "string",
                            "description": "The meta.callId of the tool result, exactly as returned.",
                        },
                        "pointer": {
                            "type": "string",
                            "description": (
                                "A JSON Pointer into that tool result, for example /data/lines/12."
                            ),
                        },
                    },
                },
            },
        },
    }


def proposal_schema() -> dict[str, Any]:
    """Return the JSON Schema the Proposal stage asks for.

    It is one flat object with a `kind` discriminator rather than a top-level union, because
    several providers reject a union in `response_format`, and a flat object repairs better
    on a weak model. Pydantic validates the exact variant afterwards.
    """
    return {
        "type": "object",
        "additionalProperties": False,
        "required": ["kind"],
        "properties": {
            "kind": {
                "type": "string",
                "enum": ["FilePatch", "DiagnosisOnly"],
                "description": (
                    "FilePatch when a change to the Target Repository fixes the cause; "
                    "DiagnosisOnly when no repository change is the correct remediation."
                ),
            },
            "files": {
                "type": "array",
                "minItems": 1,
                "maxItems": MAX_PROPOSAL_FILES,
                "description": "FilePatch: one to five whole YAML files, each with its complete new content.",
                "items": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["path", "updatedContent", "rationale"],
                    "properties": {
                        "path": {
                            "type": "string",
                            "description": (
                                "Repository-relative path of a YAML manifest that already exists, "
                                "for example apps/shop/overlays/prod/checkout.yaml."
                            ),
                        },
                        "updatedContent": {
                            "type": "string",
                            "description": (
                                "The complete new content of the file, LF-terminated. Not a diff and "
                                "not a fragment."
                            ),
                        },
                        "rationale": {
                            "type": "string",
                            "maxLength": MAX_RATIONALE_LENGTH,
                            "description": "Why this file changes this way.",
                        },
                    },
                },
            },
            "confidence": {
                "type": "string",
                "enum": ["exact", "configured", "identity", "name-only"],
                "description": "How certain you are that this path is the one that defines the Target.",
            },
            "reason": {
                "type": "string",
                "enum": [
                    "external-cause",
                    "unmanaged-resource",
                    "infrastructure",
                    "no-git-repository",
                    "insufficient-evidence",
                    "operator-action-required",
                ],
                "description": "DiagnosisOnly: why no repository change is the correct remediation.",
            },
            "recommendation": {
                "type": "string",
                "maxLength": MAX_RECOMMENDATION_LENGTH,
                "description": "DiagnosisOnly: the action a human should take.",
            },
        },
    }
