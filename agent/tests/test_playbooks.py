"""Playbooks are optional, stable and advisory."""

from __future__ import annotations

import pytest

from pagerless_agent import playbooks


def test_catalogue_is_stable_and_complete() -> None:
    names = [item.name for item in playbooks.catalogue()]
    assert names == sorted(names)
    assert len(names) >= 3
    assert len(set(names)) == len(names)
    for playbook in playbooks.catalogue():
        assert playbook.summary
        assert len(playbook.body) > 100
        assert playbooks.get(playbook.name) is playbook


def test_unknown_playbook_is_none() -> None:
    assert playbooks.get("not-a-playbook") is None
    with pytest.raises((AttributeError, KeyError)):
        _ = playbooks.PLAYBOOKS["not-a-playbook"]


def test_playbooks_describe_techniques_not_fixed_diagnoses() -> None:
    for playbook in playbooks.catalogue():
        body = playbook.body.lower()
        assert "diagnosis is" not in body
        assert "root cause is" not in body
