"""Tolerant repair of tool-call arguments."""

from __future__ import annotations

from pagerless_agent.arguments import parse_arguments, strip_fence, strip_trailing_commas


def test_empty_arguments_are_an_empty_object() -> None:
    assert parse_arguments(None) == {}
    assert parse_arguments("") == {}
    assert parse_arguments("   ") == {}
    assert parse_arguments({}) == {}


def test_plain_and_fenced_json_objects_are_accepted() -> None:
    assert parse_arguments('{"a": 1}') == {"a": 1}
    assert parse_arguments('```json\n{"a": 1}\n```') == {"a": 1}
    assert parse_arguments('{"a": 1,}') == {"a": 1}


def test_double_encoded_object_is_unwrapped() -> None:
    assert parse_arguments('"{\\"a\\": 1}"') == {"a": 1}


def test_non_object_and_broken_values_are_refused() -> None:
    assert parse_arguments("[1, 2]") is None
    assert parse_arguments("not json") is None
    assert parse_arguments(42) is None


def test_helpers_leave_strings_alone() -> None:
    assert strip_fence("plain") == "plain"
    assert strip_fence("```\n{}\n```") == "{}"
    assert strip_trailing_commas('{"a": "x,}", "b": [1,],}') == '{"a": "x,}", "b": [1]}'
