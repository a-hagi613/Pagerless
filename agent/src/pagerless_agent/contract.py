"""The wire contract between the agent and pagerless-server.

These models mirror `internal/agentapi/contract.go` and the Proposal types in
`api/v1alpha1`. The server decodes strictly and rejects unknown fields, so every
model forbids extras and is serialized without its unset optional fields.
"""

from __future__ import annotations

from typing import Annotated, Literal

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

from pagerless_agent.bundle import WorkloadRef

# Limits that the server enforces, matching `internal/agentapi/check.go`.
MAX_DIAGNOSIS_LENGTH = 4000
MAX_NARRATIVE_LENGTH = 4000
MAX_FINDING_SOURCE_LENGTH = 300
MAX_FINDING_SUMMARY_LENGTH = 500
MAX_STEP_SUMMARY_LENGTH = 200
MAX_TOOL_NAME_LENGTH = 64
# The number of Findings a Page keeps (`v1alpha1.MaxFindings`).
MAX_FINDINGS = 20
# Bounds on the model usage one report may carry, matching `internal/agentapi/check.go`.
MAX_INPUT_TOKENS = 10_000_000
MAX_OUTPUT_TOKENS = 1_000_000
MAX_COST_USD = 100.0

FindingKind = Literal["Log", "Metric", "Trace", "Event", "Commit"]
StepKind = Literal["ToolCall", "Finding", "Progress"]
ACTIONS = ("Scale", "PatchResources", "Rollback", "NoAction")


class _Strict(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)


def _not_blank(value: str) -> str:
    if not value.strip():
        raise ValueError("must not be blank")
    return value


class Finding(_Strict):
    """One cited piece of evidence that supports a Diagnosis."""

    kind: FindingKind
    source: Annotated[str, StringConstraints(max_length=MAX_FINDING_SOURCE_LENGTH)]
    summary: Annotated[str, StringConstraints(max_length=MAX_FINDING_SUMMARY_LENGTH)]

    _source_cited = field_validator("source")(_not_blank)
    _summary_given = field_validator("summary")(_not_blank)


class StepEvent(_Strict):
    """One step of the investigation, posted to the events endpoint."""

    kind: StepKind
    tool: Annotated[str, StringConstraints(max_length=MAX_TOOL_NAME_LENGTH)] | None = None
    summary: Annotated[str, StringConstraints(max_length=MAX_STEP_SUMMARY_LENGTH)]
    finding: Finding | None = None

    _summary_given = field_validator("summary")(_not_blank)

    @model_validator(mode="after")
    def _finding_matches_kind(self) -> StepEvent:
        if self.kind == "Finding" and self.finding is None:
            raise ValueError("a Finding step must carry the finding")
        if self.kind != "Finding" and self.finding is not None:
            raise ValueError("only a Finding step may carry a finding")
        return self


# Kubernetes quantities such as 256Mi, 0.5, 500m, or 1e3.
_QUANTITY = r"^[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+|[kKMGTPE]i?|m)?$"
_RESOURCE_NAME = r"^[a-z][a-z0-9./-]*$"

Quantity = Annotated[str, StringConstraints(pattern=_QUANTITY)]
ResourceList = dict[Annotated[str, StringConstraints(pattern=_RESOURCE_NAME)], Quantity]


class ScaleSpec(_Strict):
    replicas: Annotated[int, Field(ge=0, le=2**31 - 1)]


class ScaleParameters(_Strict):
    scale: ScaleSpec


class PatchResourcesSpec(_Strict):
    container: Annotated[str, StringConstraints(min_length=1, max_length=63)]
    requests: ResourceList | None = None
    limits: ResourceList | None = None

    @model_validator(mode="after")
    def _changes_something(self) -> PatchResourcesSpec:
        if not self.requests and not self.limits:
            raise ValueError("set at least one of requests and limits")
        return self


class PatchResourcesParameters(_Strict):
    patchResources: PatchResourcesSpec  # noqa: N815 (the wire name)


class RollbackSpec(_Strict):
    commit: Annotated[str, StringConstraints(min_length=7, max_length=64, pattern=r"^[0-9a-f]+$")]


class RollbackParameters(_Strict):
    rollback: RollbackSpec


class ScaleProposal(_Strict):
    action: Literal["Scale"]
    parameters: ScaleParameters


class PatchResourcesProposal(_Strict):
    action: Literal["PatchResources"]
    parameters: PatchResourcesParameters


class RollbackProposal(_Strict):
    action: Literal["Rollback"]
    parameters: RollbackParameters


class NoActionProposal(_Strict):
    action: Literal["NoAction"]


ProposedAction = Annotated[
    ScaleProposal | PatchResourcesProposal | RollbackProposal | NoActionProposal,
    Field(discriminator="action"),
]
"""One Action with the parameters that belong to it and no others (ADR-0002)."""

PROPOSED_ACTION: TypeAdapter[ProposedAction] = TypeAdapter(ProposedAction)

_PARAMETER_WRAPPER = {"Scale": "scale", "PatchResources": "patchResources", "Rollback": "rollback"}


def normalize_proposal(value: object) -> object:
    """Wrap the model's per-Action parameter fields in the wire contract's wrapper.

    The tool schema presents one flat ``parameters`` object because Anthropic
    rejects a top-level union, while the wire contract nests each Action's
    parameters under its own key. A model that already emits the nested shape is
    accepted unchanged.
    """
    if not isinstance(value, dict):
        return value
    action = value.get("action")
    wrapper = _PARAMETER_WRAPPER.get(action) if isinstance(action, str) else None
    parameters = value.get("parameters")
    if wrapper is None or not isinstance(parameters, dict) or wrapper in parameters:
        return value
    return {**value, "parameters": {wrapper: parameters}}


def proposal_tool_schema() -> dict[str, object]:
    """Return the Proposal tool schema that the Anthropic API accepts.

    Anthropic rejects a union at the top level of a tool's input schema, so the
    discriminated union is presented as one object with an action enum and the
    parameters for each parameterised Action. Pydantic still validates the
    exact variant, and one revision is allowed when the model picks the wrong
    parameter object.
    """
    quantity = {"type": "string", "pattern": _QUANTITY}
    resource_map = {
        "type": "object",
        "description": "Kubernetes resource name to quantity, for example memory: 128Mi.",
        "additionalProperties": quantity,
    }
    return {
        "type": "object",
        "additionalProperties": False,
        "required": ["action"],
        "properties": {
            "action": {"type": "string", "enum": list(ACTIONS)},
            "parameters": {
                "type": "object",
                "description": (
                    "The fields for the chosen Action and no others: replicas for Scale; "
                    "container, requests, and limits for PatchResources; commit for Rollback. "
                    "Omit parameters for NoAction."
                ),
                "additionalProperties": False,
                "properties": {
                    "replicas": {
                        "type": "integer",
                        "minimum": 0,
                        "maximum": 2**31 - 1,
                        "description": "Scale: the desired replica count.",
                    },
                    "container": {
                        "type": "string",
                        "minLength": 1,
                        "maxLength": 63,
                        "description": "PatchResources: the container to change.",
                    },
                    "requests": {
                        **resource_map,
                        "description": "PatchResources: the resource requests to set, "
                        "for example memory: 128Mi.",
                    },
                    "limits": {
                        **resource_map,
                        "description": "PatchResources: the resource limits to set, "
                        "for example memory: 256Mi.",
                    },
                    "commit": {
                        "type": "string",
                        "pattern": "^[0-9a-f]{7,64}$",
                        "description": "Rollback: the Sandbox commit to revert.",
                    },
                },
            },
        },
    }


class DiagnosisOutput(_Strict):
    """The structured output of the Diagnosis stage."""

    diagnosis: Annotated[str, StringConstraints(max_length=MAX_DIAGNOSIS_LENGTH)]
    findings: Annotated[list[Finding], Field(max_length=MAX_FINDINGS)]

    _diagnosis_given = field_validator("diagnosis")(_not_blank)


class Usage(_Strict):
    """The model usage of one investigation, bounded like the Page status."""

    inputTokens: Annotated[int, Field(ge=0, le=MAX_INPUT_TOKENS)]  # noqa: N815
    outputTokens: Annotated[int, Field(ge=0, le=MAX_OUTPUT_TOKENS)]  # noqa: N815
    costUSD: Annotated[float, Field(ge=0, le=MAX_COST_USD)]  # noqa: N815


class Report(_Strict):
    """The agent's final output: a Diagnosis, its Findings, one Proposal, and its model usage."""

    diagnosis: str
    findings: list[Finding]
    proposal: ProposedAction
    usage: Usage | None = None


class ReportError(_Strict):
    """One reason the server refused a report (`agentapi.ValidationError`)."""

    field: str
    message: str


class NarrativeReport(_Strict):
    """The Narrative Job's answer: the short account of a finished Page."""

    narrative: Annotated[str, StringConstraints(min_length=1, max_length=MAX_NARRATIVE_LENGTH)]

    _narrative_given = field_validator("narrative")(_not_blank)


class NarrativeRecord(_Strict):
    """What the server gives a Narrative Job: the Page rendered as text, and its Workload."""

    workload: WorkloadRef
    text: Annotated[str, StringConstraints(min_length=1)]


def encode(model: BaseModel) -> dict[str, object]:
    """Return the JSON body for a wire model, omitting unset optional fields."""
    return model.model_dump(mode="json", exclude_none=True)


def validation_errors(error: ValidationError, prefix: str) -> list[ReportError]:
    """Convert a local schema failure into the shape the server uses for its own errors."""
    errors: list[ReportError] = []
    for item in error.errors():
        loc = [str(part) for part in item["loc"]]
        # A discriminated union puts the variant's tag first. The server's field paths omit it.
        if loc and loc[0] in ACTIONS:
            loc = loc[1:]
        errors.append(ReportError(field=".".join([prefix, *loc]), message=item["msg"]))
    return errors
