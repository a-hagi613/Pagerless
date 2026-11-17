"""The wire contract: the shapes the server decodes, and agreement with the Go source that defines them."""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any, get_args

import pytest
from pydantic import ValidationError

from pagerless_agent import contract, settings
from pagerless_agent.contract import (
    PROPOSED_ACTION,
    Finding,
    Report,
    StepEvent,
    encode,
    validation_errors,
)

REPO = Path(__file__).resolve().parents[2]


def go_source(path: str) -> str:
    return (REPO / path).read_text(encoding="utf-8")


class TestAgreementWithTheServer:
    """These tests read the Go packages, so that a change on either side of the contract fails here."""

    def test_the_limits_match_the_servers(self) -> None:
        check = go_source("internal/agentapi/check.go")
        limits = {m[1]: int(m[2]) for m in re.finditer(r"^\s+(Max\w+)\s*=\s*(\d+)$", check, re.MULTILINE)}
        assert limits == {
            "MaxDiagnosisLength": contract.MAX_DIAGNOSIS_LENGTH,
            "MaxFindingSourceLen": contract.MAX_FINDING_SOURCE_LENGTH,
            "MaxFindingSummaryLen": contract.MAX_FINDING_SUMMARY_LENGTH,
            "MaxStepSummaryLength": contract.MAX_STEP_SUMMARY_LENGTH,
            "MaxToolNameLength": contract.MAX_TOOL_NAME_LENGTH,
        }
        types = go_source("api/v1alpha1/triage_types.go")
        assert re.search(rf"const MaxFindings = {contract.MAX_FINDINGS}\b", types)

    def test_the_vocabularies_match_the_servers(self) -> None:
        types = go_source("api/v1alpha1/triage_types.go")
        proposal = go_source("api/v1alpha1/proposal_types.go")
        finding_kinds = re.findall(r'FindingKind\w+ FindingKind = "(\w+)"', types)
        step_kinds = re.findall(r'StepKind\w+ StepKind = "(\w+)"', types)
        actions = re.findall(r'Action\w+ Action = "(\w+)"', proposal)
        assert finding_kinds == list(get_args(contract.FindingKind))
        assert step_kinds == list(get_args(contract.StepKind))
        assert actions == list(contract.ACTIONS)

    def test_the_environment_matches_the_one_the_server_sets_on_the_job(self) -> None:
        names = set(re.findall(r'Env\w+ += "(PAGERLESS_\w+)"', go_source("internal/agentapi/contract.go")))
        mine = {value for key, value in vars(settings).items() if key.startswith("ENV_")}
        assert mine == names

    def test_the_audience_and_paths_match(self) -> None:
        source = go_source("internal/agentapi/contract.go")
        assert 'Audience = "pagerless-server"' in source
        assert '"/v1/triages/" + url.PathEscape(triage) + "/events"' in source
        assert '"/v1/triages/" + url.PathEscape(triage) + "/report"' in source


class TestFindingAndStep:
    def test_a_finding_needs_a_source_and_a_summary_that_are_not_blank(self) -> None:
        for bad in (
            {"kind": "Log", "source": " ", "summary": "x"},
            {"kind": "Log", "source": "x", "summary": ""},
            {"kind": "Rumour", "source": "x", "summary": "x"},
            {"kind": "Log", "source": "x" * 501, "summary": "x"},
            {"kind": "Log", "source": "x", "summary": "x" * 1001},
            {"kind": "Log", "source": "x", "summary": "x", "extra": 1},
        ):
            with pytest.raises(ValidationError):
                Finding.model_validate(bad)

    def test_a_finding_step_carries_its_finding_and_no_other_step_does(self) -> None:
        finding = Finding(kind="Log", source="s", summary="m")
        StepEvent(kind="Finding", summary="x", finding=finding)
        with pytest.raises(ValidationError, match="must carry the finding"):
            StepEvent(kind="Finding", summary="x")
        with pytest.raises(ValidationError, match="only a Finding step"):
            StepEvent(kind="Progress", summary="x", finding=finding)

    def test_a_step_is_encoded_without_its_unset_fields(self) -> None:
        assert encode(StepEvent(kind="Progress", summary="Investigating.")) == {
            "kind": "Progress",
            "summary": "Investigating.",
        }
        assert encode(StepEvent(kind="ToolCall", tool="pods_log", summary="pods_log {}")) == {
            "kind": "ToolCall",
            "tool": "pods_log",
            "summary": "pods_log {}",
        }

    def test_a_step_summary_longer_than_the_servers_limit_is_refused_locally(self) -> None:
        with pytest.raises(ValidationError):
            StepEvent(kind="Progress", summary="x" * 301)


VALID_PROPOSALS: list[dict[str, Any]] = [
    {"action": "NoAction"},
    {"action": "RestartWorkload"},
    {"action": "Scale", "parameters": {"scale": {"replicas": 3}}},
    {"action": "Scale", "parameters": {"scale": {"replicas": 0}}},
    {
        "action": "PatchResources",
        "parameters": {"patchResources": {"container": "app", "limits": {"memory": "512Mi"}}},
    },
    {
        "action": "PatchResources",
        "parameters": {
            "patchResources": {
                "container": "app",
                "requests": {"cpu": "250m", "memory": "256Mi"},
                "limits": {"cpu": "1", "memory": "1Gi", "ephemeral-storage": "1e3"},
            }
        },
    },
    {"action": "Rollback", "parameters": {"rollback": {"commit": "3f2a9c1"}}},
]


class TestProposalSchema:
    @pytest.mark.parametrize("proposal", VALID_PROPOSALS)
    def test_a_valid_proposal_round_trips_to_the_wire_form_unchanged(self, proposal: dict[str, Any]) -> None:
        parsed = PROPOSED_ACTION.validate_python(proposal)
        assert PROPOSED_ACTION.dump_python(parsed, mode="json", exclude_none=True) == proposal

    @pytest.mark.parametrize(
        ("proposal", "field"),
        [
            ({"action": "DeleteNamespace"}, "proposal"),
            ({}, "proposal"),
            ({"action": "Scale"}, "proposal.parameters"),
            (
                {"action": "Scale", "parameters": {"scale": {"replicas": -1}}},
                "proposal.parameters.scale.replicas",
            ),
            (
                {"action": "Scale", "parameters": {"scale": {"replicas": "3"}}},
                "proposal.parameters.scale.replicas",
            ),
            (
                {"action": "Scale", "parameters": {"scale": {"replicas": 2**31}}},
                "proposal.parameters.scale.replicas",
            ),
            (
                {"action": "Scale", "parameters": {"patchResources": {"container": "a"}}},
                "proposal.parameters.scale",
            ),
            ({"action": "NoAction", "parameters": {"scale": {"replicas": 1}}}, "proposal.parameters"),
            ({"action": "RestartWorkload", "parameters": {}}, "proposal.parameters"),
            (
                {"action": "PatchResources", "parameters": {"patchResources": {"container": "app"}}},
                "proposal.parameters.patchResources",
            ),
            (
                {
                    "action": "PatchResources",
                    "parameters": {"patchResources": {"container": "", "limits": {"memory": "1Gi"}}},
                },
                "proposal.parameters.patchResources.container",
            ),
            (
                {
                    "action": "PatchResources",
                    "parameters": {"patchResources": {"container": "a" * 64, "limits": {"memory": "1Gi"}}},
                },
                "proposal.parameters.patchResources.container",
            ),
            (
                {
                    "action": "PatchResources",
                    "parameters": {"patchResources": {"container": "app", "limits": {"memory": "lots"}}},
                },
                "proposal.parameters.patchResources.limits.memory",
            ),
            (
                {
                    "action": "PatchResources",
                    "parameters": {"patchResources": {"container": "app", "limits": {"memory": "-1Gi"}}},
                },
                "proposal.parameters.patchResources.limits.memory",
            ),
            (
                {
                    "action": "PatchResources",
                    "parameters": {"patchResources": {"container": "app", "limits": {"Memory": "1Gi"}}},
                },
                "proposal.parameters.patchResources.limits.Memory",
            ),
            (
                {"action": "Rollback", "parameters": {"rollback": {"commit": "abc"}}},
                "proposal.parameters.rollback.commit",
            ),
            (
                {"action": "Rollback", "parameters": {"rollback": {"commit": "ABCDEF1"}}},
                "proposal.parameters.rollback.commit",
            ),
            (
                {"action": "Rollback", "parameters": {"rollback": {"commit": "HEAD~1"}}},
                "proposal.parameters.rollback.commit",
            ),
            (
                {"action": "Rollback", "parameters": {"rollback": {"commit": "a" * 65}}},
                "proposal.parameters.rollback.commit",
            ),
            ({"action": "NoAction", "approve": True}, "proposal.approve"),
        ],
    )
    def test_a_proposal_off_the_menu_or_out_of_shape_is_refused_at_the_field(
        self, proposal: dict[str, Any], field: str
    ) -> None:
        with pytest.raises(ValidationError) as caught:
            PROPOSED_ACTION.validate_python(proposal)
        fields = [e.field for e in validation_errors(caught.value, "proposal")]
        assert any(f == field or f.startswith(field + ".") for f in fields), fields

    def test_the_schema_given_to_the_model_is_the_per_action_union(self) -> None:
        schema = PROPOSED_ACTION.json_schema()
        assert schema["discriminator"]["propertyName"] == "action"
        assert set(schema["discriminator"]["mapping"]) == set(contract.ACTIONS)


def test_a_report_is_encoded_as_the_server_decodes_it() -> None:
    report = Report.model_validate(
        {
            "diagnosis": "d",
            "findings": [{"kind": "Log", "source": "s", "summary": "m"}],
            "proposal": {"action": "NoAction"},
        }
    )
    assert encode(report) == {
        "diagnosis": "d",
        "findings": [{"kind": "Log", "source": "s", "summary": "m"}],
        "proposal": {"action": "NoAction"},
    }


def test_diagnosis_output_is_bounded_like_the_servers_report() -> None:
    finding = {"kind": "Log", "source": "s", "summary": "m"}
    contract.DiagnosisOutput.model_validate({"diagnosis": "d", "findings": [finding] * 20})
    for bad in (
        {"diagnosis": "d", "findings": [finding] * 21},
        {"diagnosis": " ", "findings": [finding]},
        {"diagnosis": "x" * 4001, "findings": [finding]},
    ):
        with pytest.raises(ValidationError):
            contract.DiagnosisOutput.model_validate(bad)
