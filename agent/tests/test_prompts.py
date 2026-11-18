"""Tests for the agent's prompts: flat tool-schema alignment and Validation invariants."""

from __future__ import annotations

import json
import re

from pagerless_agent.budget import Limits
from pagerless_agent.contract import (
    PROPOSED_ACTION,
    PatchResourcesProposal,
    normalize_proposal,
)
from pagerless_agent.prompts import PROPOSE_PROMPT, system_prompt
from pagerless_agent.tools import UNTRUSTED_DATA_RULE


def test_system_prompt_matches_flat_parameters_and_preserves_security_anchors() -> None:
    prompt = system_prompt(Limits())

    assert "You are the investigation agent of Pagerless" in prompt
    assert UNTRUSTED_DATA_RULE in prompt
    assert "Use only pod names, commit SHAs, and values that you read in the Context Bundle" in prompt
    assert "RestartWorkload: no parameters." in prompt
    assert "Scale: parameters.replicas, the desired replica count (1 to 10)." in prompt
    assert "Rollback: parameters.commit" in prompt
    assert "NoAction: no parameters." in prompt
    assert "parameters.scale" not in prompt
    assert "parameters.patchResources" not in prompt
    assert "parameters.rollback" not in prompt


def test_system_prompt_states_patch_resources_overlay_and_bounds_invariants() -> None:
    prompt = system_prompt(Limits())

    assert "cpu from 10m to 4" in prompt
    assert "memory from 16Mi to 8Gi" in prompt
    assert "overlays the container's existing resources" in prompt
    assert "every resulting request must not exceed its resulting limit" in prompt
    assert "when raising memory for an OOMKilled container, set limits at or above requests" in prompt
    assert (
        "when lowering an unschedulable request whose current limit exceeds 8Gi, "
        "also set limits within bounds" in prompt
    )


def test_system_prompt_and_propose_prompt_specify_reverting_faulty_commit_for_rollback() -> None:
    prompt = system_prompt(Limits())

    assert "the SHA of the faulty Sandbox commit to revert" in prompt
    assert "not the earlier healthy commit to return to" in prompt
    assert "declared dependency paths" in prompt

    assert "For Rollback, parameters are commit (the faulty commit SHA to revert" in PROPOSE_PROMPT
    assert "not the prior healthy commit" in PROPOSE_PROMPT


def test_propose_prompt_uses_neutral_container_example_with_requests_and_limits() -> None:
    assert "checkout" not in PROPOSE_PROMPT

    match = re.search(r"(\{.*\})\.", PROPOSE_PROMPT)
    assert match is not None
    example = json.loads(match.group(1))
    assert example == {
        "action": "PatchResources",
        "parameters": {
            "container": "app",
            "requests": {"memory": "64Mi"},
            "limits": {"memory": "128Mi"},
        },
    }
    proposal = PROPOSED_ACTION.validate_python(normalize_proposal(example))
    assert isinstance(proposal, PatchResourcesProposal)
