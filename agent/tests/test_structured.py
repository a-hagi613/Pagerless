"""The ladder, the strict projection, the argument repair, and the pointer resolution."""

from __future__ import annotations

import pytest

from pagerless_agent.structured import (
    LadderCache,
    PointerError,
    Rung,
    extract_json,
    is_structured_parameter,
    named_parameter,
    parse_arguments,
    resolve_pointer,
    strictify,
)


def test_strictify_closes_every_object_and_requires_every_property() -> None:
    schema = {
        "type": "object",
        "additionalProperties": False,
        "required": ["kind", "nested"],
        "properties": {
            "kind": {"type": "string", "enum": ["a", "b"]},
            "count": {"type": "integer", "minimum": 1, "maximum": 5},
            "nested": {
                "type": "object",
                "properties": {"patterned": {"type": "string", "pattern": "^[a-f]+$"}},
            },
        },
    }

    projected = strictify(schema)

    assert projected["additionalProperties"] is False
    assert projected["required"] == ["kind", "nested", "count"]
    assert projected["properties"]["count"] == {"anyOf": [{"type": "integer"}, {"type": "null"}]}
    assert projected["properties"]["nested"]["additionalProperties"] is False
    assert projected["properties"]["nested"]["required"] == ["patterned"]
    # Strict mode has no `pattern`, which is why local validation is authoritative.
    assert projected["properties"]["nested"]["properties"]["patterned"] == {
        "anyOf": [{"type": "string"}, {"type": "null"}]
    }


def test_strictify_keeps_nested_definitions_and_arrays() -> None:
    schema = {
        "type": "object",
        "required": ["items"],
        "properties": {
            "items": {"type": "array", "items": {"$ref": "#/$defs/Item"}},
        },
        "$defs": {"Item": {"type": "object", "properties": {"path": {"type": "string"}}}},
    }

    projected = strictify(schema)

    assert projected["properties"]["items"]["items"] == {"$ref": "#/$defs/Item"}
    assert projected["$defs"]["Item"]["additionalProperties"] is False


@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        (None, {}),
        ("", {}),
        ("   ", {}),
        ("{}", {}),
        ('{"namespace": "shop"}', {"namespace": "shop"}),
        ('```json\n{"a": 1}\n```', {"a": 1}),
        ('{"a": 1,}', {"a": 1}),
        ('{"a": [1, 2,],}', {"a": [1, 2]}),
        ('"{\\"a\\": 1}"', {"a": 1}),
        ({"a": 1}, {"a": 1}),
        ("[1, 2]", None),
        ("42", None),
        ('"just a string"', None),
        ("not json", None),
        ('{"a": 1', None),
    ],
)
def test_parse_arguments_repairs_what_a_weak_model_emits(raw: object, expected: object) -> None:
    assert parse_arguments(raw) == expected


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ('{"a": 1}', {"a": 1}),
        ('```json\n{"a": 1}\n```', {"a": 1}),
        ('Here is the answer:\n{"a": {"b": 2}}\nThanks.', {"a": {"b": 2}}),
        ('{"a": "}"}', {"a": "}"}),
        ('[{"a": 1}]', [{"a": 1}]),
        ('{"a": "unterminated', None),
        ("no json here", None),
        ("", None),
    ],
)
def test_extract_json_finds_the_first_balanced_document(text: str, expected: object) -> None:
    assert extract_json(text) == expected


def test_resolve_pointer_follows_rfc_6901() -> None:
    document = {"data": {"lines": ["first", "second"], "a/b": 1}}
    assert resolve_pointer(document, "/data/lines/1") == "second"
    assert resolve_pointer(document, "/data/a~1b") == 1
    assert resolve_pointer(document, "") == document


@pytest.mark.parametrize("pointer", ["/data/lines/5", "/data/missing", "data/lines/0", "/data/lines/x"])
def test_resolve_pointer_refuses_a_pointer_outside_the_document(pointer: str) -> None:
    with pytest.raises(PointerError):
        resolve_pointer({"data": {"lines": ["first"]}}, pointer)


def test_the_ladder_never_goes_below_text() -> None:
    assert Rung.JSON_SCHEMA.next is Rung.JSON_OBJECT
    assert Rung.JSON_OBJECT.next is Rung.TOOL
    assert Rung.TOOL.next is Rung.TEXT
    assert Rung.TEXT.next is None


def test_ladder_cache_negotiates_once_per_endpoint_model_and_stage() -> None:
    cache = LadderCache()
    key = ("http://mock/v1", "model", "diagnose")

    assert cache.start(key, configured=Rung.JSON_SCHEMA) is Rung.JSON_SCHEMA
    cache.descend(key, Rung.JSON_OBJECT)
    assert cache.start(key, configured=Rung.JSON_SCHEMA) is Rung.JSON_OBJECT
    cache.descend(key, Rung.TOOL)
    cache.record(key, Rung.TOOL)
    assert cache.cached(key) is Rung.TOOL
    assert cache.cached(("http://mock/v1", "model", "propose")) is None


@pytest.mark.parametrize(
    ("message", "parameter"),
    [
        ("response_format is not supported", "response_format"),
        ("json_schema is not available", "json_schema"),
        ("strict mode is unsupported", "strict"),
        ("tool_choice is not supported", "tool_choice"),
        ("max_completion_tokens is required", "max_completion_tokens"),
        ("this model does not support max_tokens", "max_tokens"),
        ("temperature must be 1", "temperature"),
        ("tools are not supported", "tools"),
        ("something else entirely", None),
    ],
)
def test_named_parameter_finds_the_rejected_parameter(message: str, parameter: str | None) -> None:
    assert named_parameter(message) == parameter


def test_only_the_structured_parameters_descend_the_ladder() -> None:
    assert is_structured_parameter("response_format")
    assert is_structured_parameter("json_schema")
    assert not is_structured_parameter("tool_choice")
    assert not is_structured_parameter("max_tokens")
