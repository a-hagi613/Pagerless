"""The wire contract: the Proposal variants, the path policy, and the citations."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from pagerless_agent.contract import (
    PROPOSAL,
    Capabilities,
    Diagnosis,
    DiagnosisOnly,
    FileEdit,
    FilePatch,
    Finding,
    Investigation,
    Page,
    Report,
    StepEvent,
    Target,
    TargetRepository,
    Usage,
    citation_problems,
    diagnosis_schema,
    encode,
    normalize_proposal,
    proposal_schema,
)
from pagerless_agent.structured import strictify
from tests.support import manifest

VALID_CONTENT = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: checkout\n"


def file_edit(**overrides: object) -> FileEdit:
    fields: dict[str, object] = {
        "path": "apps/shop/checkout.yaml",
        "updated_content": VALID_CONTENT,
        "rationale": "Raise the memory limit.",
    }
    fields.update(overrides)
    return FileEdit(**fields)  # type: ignore[arg-type]


def test_the_glossary_identifiers_exist() -> None:
    assert Page.__name__ == "Page"
    assert Target.__name__ == "Target"
    assert Finding.__name__ == "Finding"
    assert Diagnosis.__name__ == "Diagnosis"
    assert TargetRepository.__name__ == "TargetRepository"
    assert {FilePatch.__name__, DiagnosisOnly.__name__} == {"FilePatch", "DiagnosisOnly"}


def test_a_file_edit_normalises_line_endings_and_one_trailing_newline() -> None:
    edit = file_edit(updated_content="apiVersion: v1\nkind: Pod\n\n\n")

    assert edit.updated_content.endswith("\n")
    assert not edit.updated_content.endswith("\n\n")


@pytest.mark.parametrize(
    "path",
    [
        "/etc/passwd",
        "apps/../secrets.yaml",
        ".github/workflows/ci.yaml",
        "apps/shop/checkout.json",
        "apps\\shop\\checkout.yaml",
        ".git/config.yaml",
        "apps//shop/checkout.yaml",
        "apps/shop/checkout.yaml ",
    ],
)
def test_the_path_policy_refuses_a_path_a_patch_may_not_touch(path: str) -> None:
    with pytest.raises(ValidationError):
        file_edit(path=path)


def test_content_with_carriage_returns_or_nul_is_refused() -> None:
    with pytest.raises(ValidationError):
        file_edit(updated_content="kind: Pod\r\n")
    with pytest.raises(ValidationError):
        file_edit(updated_content="kind: Pod\x00\n")


def test_a_proposal_changes_one_to_five_files() -> None:
    one = FilePatch(kind="FilePatch", files=[file_edit()])
    assert len(one.files) == 1

    five = [file_edit(path=f"apps/shop/file-{index}.yaml") for index in range(5)]
    assert len(FilePatch(kind="FilePatch", files=five).files) == 5

    with pytest.raises(ValidationError):
        FilePatch(kind="FilePatch", files=[])
    with pytest.raises(ValidationError):
        FilePatch(kind="FilePatch", files=[file_edit(path=f"apps/f{index}.yaml") for index in range(6)])


def test_diagnosis_only_carries_a_documented_reason_and_a_recommendation() -> None:
    proposal = DiagnosisOnly(
        kind="DiagnosisOnly",
        reason="external-cause",
        recommendation="Stop the StressChaos experiment that targets the Target.",
    )

    assert proposal.reason == "external-cause"
    with pytest.raises(ValidationError):
        DiagnosisOnly(kind="DiagnosisOnly", reason="because", recommendation="Do something.")  # type: ignore[arg-type]


def test_normalize_proposal_removes_the_other_variants_nullable_fields() -> None:
    raw = {
        "kind": "DiagnosisOnly",
        "files": None,
        "confidence": None,
        "reason": "unmanaged-resource",
        "recommendation": "Attach the Deployment to GitOps.",
    }

    validated = PROPOSAL.validate_python(normalize_proposal(raw))

    assert isinstance(validated, DiagnosisOnly)
    assert normalize_proposal({"kind": "FilePatch", "files": [], "reason": None, "confidence": "exact"}) == {
        "kind": "FilePatch",
        "files": [],
        "confidence": "exact",
    }


def test_the_diagnosis_requires_at_least_one_finding() -> None:
    finding = Finding(
        kind="Log",
        source="pods_logs shop/checkout (previous)",
        summary="The process allocated 512 MiB and was killed.",
        tool_call_id="srv-1",
        pointer="/data/lines/0",
    )

    diagnosis = Diagnosis(diagnosis="The container exceeds its memory limit.", findings=[finding])
    assert diagnosis.findings[0].tool_call_id == "srv-1"

    with pytest.raises(ValidationError):
        Diagnosis(diagnosis="No evidence.", findings=[])


def test_a_finding_citation_must_name_a_recorded_call_and_an_existing_pointer() -> None:
    results = {"srv-1": {"ok": True, "data": {"lines": ["first", "second"]}}}
    good = Finding(
        kind="Log",
        source="pods_logs",
        summary="The first line.",
        tool_call_id="srv-1",
        pointer="/data/lines/1",
    )
    unknown = good.model_copy(update={"tool_call_id": "srv-9"})
    wrong = good.model_copy(update={"pointer": "/data/lines/7"})

    assert citation_problems([good], results) == []
    assert "no tool call named 'srv-9'" in citation_problems([unknown], results)[0]
    assert "does not exist" in citation_problems([wrong], results)[0]


def test_step_events_validate_their_payload_against_their_kind() -> None:
    finding = Finding(
        kind="Event",
        source="events_list",
        summary="BackOff",
        tool_call_id="srv-2",
        pointer="/data/events/0",
    )

    assert StepEvent(kind="Finding", summary="recorded", finding=finding).finding is finding
    with pytest.raises(ValidationError):
        StepEvent(kind="Finding", summary="missing")
    with pytest.raises(ValidationError):
        StepEvent(kind="Progress", summary="wrong", finding=finding)


def test_encode_uses_the_servers_camel_case_names() -> None:
    report = Report(
        page=Page(name="checkout-7f2c", uid="uid-1", namespace="pagerless"),
        diagnosis="diagnosis",
        findings=[
            Finding(
                kind="Log",
                source="pods_logs",
                summary="summary",
                tool_call_id="srv-1",
                pointer="/data/lines/0",
            )
        ],
        proposal=DiagnosisOnly(kind="DiagnosisOnly", reason="infrastructure", recommendation="Drain."),
        usage=Usage(input_tokens=10, output_tokens=5, model_calls=2),
        capabilities=Capabilities(structured_output={"propose": "tool"}),
    )

    encoded = encode(report)

    assert encoded["page"] == {"name": "checkout-7f2c", "uid": "uid-1", "namespace": "pagerless"}
    assert encoded["usage"] == {"inputTokens": 10, "outputTokens": 5, "modelCalls": 2, "estimated": False}
    assert encoded["capabilities"] == {
        "structuredOutput": {"propose": "tool"},
        "usageSource": "provider",
    }
    assert encoded["findings"][0]["toolCallId"] == "srv-1"


def test_the_model_schemas_survive_the_strict_projection() -> None:
    for schema in (diagnosis_schema(), proposal_schema()):
        projected = strictify(schema)
        assert projected["additionalProperties"] is False
        assert set(projected["properties"]) == set(projected["required"])


def test_the_manifest_parses_into_typed_models() -> None:
    investigation = Investigation.model_validate(manifest(context={"gatheredAt": "now"}))

    assert investigation.page.name
    assert investigation.target.label == "Deployment shop/checkout"
    assert investigation.scope.namespaces == ("shop",)
    assert investigation.scope.repositories[0].slug == "acme/gitops"
    assert investigation.limits.tool_calls == 15
    assert investigation.context == {"gatheredAt": "now"}
