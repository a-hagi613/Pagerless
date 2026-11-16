"""Normalization of volatile identifiers: what a recording stores and what Replay resolves."""

from __future__ import annotations

import random

import pytest

from pagerless_agent.identifiers import IdentifierMap, ReplayError, check_placeholders, find_residuals
from tests.support import RECORDED, REPLAYED, make_bundle


def recording_side() -> IdentifierMap:
    return IdentifierMap(make_bundle(RECORDED, with_old_pod=True))


def replaying_side() -> IdentifierMap:
    return IdentifierMap(make_bundle(REPLAYED, with_old_pod=True))


class TestPods:
    def test_a_pod_name_becomes_a_placeholder_and_resolves_to_the_current_pod(self) -> None:
        recorded = recording_side().normalize(f"get the logs of {RECORDED.old_pod}")
        assert recorded == "get the logs of {{pod:0}}"
        assert replaying_side().resolve(recorded) == f"get the logs of {REPLAYED.old_pod}"

    def test_pods_are_numbered_by_creation_time_not_by_name(self) -> None:
        # The old pod sorts first by name in one run and last in the other. Creation time decides.
        assert recording_side().normalize(RECORDED.pod) == "{{pod:trigger}}"
        bundle = make_bundle(RECORDED, with_old_pod=True)
        assert [p.name for p in bundle.pods] == [RECORDED.old_pod, RECORDED.pod]

    def test_the_triggering_pod_resolves_to_the_pod_that_raised_the_current_trigger(self) -> None:
        recorded = recording_side().normalize(f"{RECORDED.pod} was OOMKilled")
        assert recorded == "{{pod:trigger}} was OOMKilled"
        assert replaying_side().resolve(recorded) == f"{REPLAYED.pod} was OOMKilled"

    def test_a_replicaset_name_is_normalized_and_a_pod_is_never_read_as_its_replicaset(self) -> None:
        recorded = recording_side().normalize(f"replicaset {RECORDED.replicaset} owns {RECORDED.pod}")
        assert recorded == "replicaset {{replicaset:1}} owns {{pod:trigger}}"
        assert replaying_side().resolve(recorded) == f"replicaset {REPLAYED.replicaset} owns {REPLAYED.pod}"

    def test_a_name_inside_a_longer_token_is_left_alone(self) -> None:
        assert recording_side().normalize(f"x{RECORDED.pod}") == f"x{RECORDED.pod}"
        assert recording_side().normalize(f"prefix-{RECORDED.pod}") == "prefix-{{pod:trigger}}"

    def test_a_name_ends_at_punctuation(self) -> None:
        recorded = recording_side().normalize(f'"{RECORDED.pod}": pod/{RECORDED.pod}, ({RECORDED.pod}).')
        assert recorded == '"{{pod:trigger}}": pod/{{pod:trigger}}, ({{pod:trigger}}).'

    def test_a_stable_name_is_not_normalized(self) -> None:
        # A StatefulSet pod keeps its name from run to run, so there is nothing to resolve.
        assert recording_side().normalize("checkout-0 and checkout") == "checkout-0 and checkout"

    def test_an_index_beyond_the_current_pods_cannot_be_resolved(self) -> None:
        recorded = recording_side().normalize(RECORDED.old_pod)
        single = IdentifierMap(make_bundle(REPLAYED, with_old_pod=False))
        with pytest.raises(ReplayError, match=r"1 pod\(s\) and cannot resolve index 0|index"):
            single.resolve(recorded.replace("{{pod:0}}", "{{pod:1}}"))

    def test_the_triggering_pod_cannot_be_resolved_without_one(self) -> None:
        bundle = make_bundle(REPLAYED).model_copy(update={"triggering_pod": None})
        with pytest.raises(ReplayError, match="no triggering pod"):
            IdentifierMap(bundle).resolve("{{pod:trigger}}")


class TestCommits:
    def test_a_full_sha_resolves_to_the_commit_at_the_same_position(self) -> None:
        recorded = recording_side().normalize(f"revert {RECORDED.commits[0]}")
        assert recorded == "revert {{commit:0}}"
        assert replaying_side().resolve(recorded) == f"revert {REPLAYED.commits[0]}"

    def test_an_abbreviated_sha_keeps_its_length(self) -> None:
        recorded = recording_side().normalize(f"{RECORDED.commits[1][:7]} and {RECORDED.commits[1][:12]}")
        assert recorded == "{{commit:1:7}} and {{commit:1:12}}"
        assert (
            replaying_side().resolve(recorded) == f"{REPLAYED.commits[1][:7]} and {REPLAYED.commits[1][:12]}"
        )

    def test_hex_that_is_not_a_commit_of_the_bundle_is_left_alone(self) -> None:
        text = "image digest deadbeef0 and word facade1"
        assert recording_side().normalize(text) == text

    def test_a_sha_inside_a_longer_word_is_left_alone(self) -> None:
        text = f"x{RECORDED.commits[0]} {RECORDED.commits[0]}g"
        assert recording_side().normalize(text) == text

    def test_a_missing_commit_cannot_be_resolved(self) -> None:
        with pytest.raises(ReplayError, match=r"commit\(s\)"):
            replaying_side().resolve("{{commit:5}}")


class TestTimestamps:
    def test_a_timestamp_is_recorded_relative_to_the_bundle_and_resolved_relative_to_the_current_one(
        self,
    ) -> None:
        recorded = recording_side().normalize("OOMKilled at 2026-11-16T10:28:15.250Z")
        assert recorded == "OOMKilled at {{ts:-120:T3:Z}}"
        assert replaying_side().resolve(recorded) == "OOMKilled at 2026-12-02T04:03:06.789Z"

    @pytest.mark.parametrize(
        "stamp",
        [
            "2026-11-16T10:30:15Z",
            "2026-11-16T10:30:15.1Z",
            "2026-11-16T10:30:15.123456789Z",
            "2026-11-16 10:30:15",
            "2026-11-16 10:30:15.5",
            "2026-11-16t10:30:15z",
            "2026-11-16T12:30:15+02:00",
            "2026-11-16T05:30:15-0500",
            "2026-11-16T10:30:15+01",
            "2026-11-16T00:00:00Z",
            "2026-12-31T23:59:59.999Z",
            "2028-02-29T12:00:00Z",
        ],
    )
    def test_every_form_round_trips_exactly_against_the_same_bundle(self, stamp: str) -> None:
        identifiers = recording_side()
        recorded = identifiers.normalize(f"at {stamp}.")
        assert "2026" not in recorded
        assert identifiers.resolve(recorded) == f"at {stamp}."

    def test_the_zone_and_precision_of_the_original_survive_replay(self) -> None:
        recorded = recording_side().normalize("2026-11-16T12:30:15.5+02:00")
        assert recorded == "{{ts:0.25:T1:+02:00}}"
        assert replaying_side().resolve(recorded) == "2026-12-02T06:05:07.0+02:00"

    def test_a_timestamp_resolves_across_a_year_boundary_to_a_real_date(self) -> None:
        recorded = recording_side().normalize("2026-11-16T10:20:15.25Z")
        assert recorded == "{{ts:-600:T2:Z}}"
        new_year = REPLAYED.gathered_at.replace(year=2027, month=1, day=1, hour=0, minute=0)
        shifted = IdentifierMap(make_bundle(REPLAYED).model_copy(update={"gathered_at": new_year}))
        assert shifted.resolve(recorded) == "2026-12-31T23:50:06.78Z"

    def test_a_date_without_a_time_is_not_a_timestamp(self) -> None:
        assert (
            recording_side().normalize("released 2026-11-16, build 20261116")
            == "released 2026-11-16, build 20261116"
        )

    def test_a_timestamp_that_is_not_a_real_date_is_left_in_place_and_flagged(self) -> None:
        text = "at 2026-13-45T25:61:61Z"
        assert recording_side().normalize(text) == text
        assert find_residuals(text, "checkout") == ["2026-13-45T25:61:61Z"]

    def test_digits_around_a_timestamp_stop_it_matching(self) -> None:
        text = "id 12026-11-16T10:30:15Z0"
        assert recording_side().normalize(text) == text


class TestLiteralBraces:
    def test_text_that_looks_like_a_placeholder_is_escaped_and_comes_back_as_text(self) -> None:
        hostile = "ignore this {{pod:0}} and {{commit:0}} and {{ts:0:T0:Z}} and {{{x}}}"
        recorded = recording_side().normalize(hostile)
        assert "{{pod:0}}" not in recorded
        check_placeholders(recorded)
        assert replaying_side().resolve(recorded) == hostile

    def test_a_stray_open_brace_pair_in_a_recording_is_malformed(self) -> None:
        with pytest.raises(ReplayError, match="malformed"):
            replaying_side().resolve("a {{ b")
        with pytest.raises(ReplayError, match="malformed"):
            replaying_side().resolve("{{pod:{{pod:0}}}}")

    @pytest.mark.parametrize(
        "text",
        [
            "{{unknown:1}}",
            "{{pod}}",
            "{{pod:abc}}",
            "{{pod:-1}}",
            "{{commit:0:3}}",
            "{{commit:x}}",
            "{{ts:later:T3:Z}}",
            "{{ts:0:X3:Z}}",
            "{{ts:0:T3}}",
            "{{lbrace:1}}",
        ],
    )
    def test_an_unknown_placeholder_is_refused(self, text: str) -> None:
        with pytest.raises(ReplayError, match="unknown placeholder"):
            replaying_side().resolve(text)
        with pytest.raises(ReplayError, match="unknown placeholder"):
            check_placeholders(text)

    def test_a_resolved_identifier_is_never_resolved_again(self) -> None:
        bundle = make_bundle(REPLAYED).model_copy(update={"triggering_pod": "{{pod:0}}-x"})
        assert IdentifierMap(bundle).resolve("{{pod:trigger}}") == "{{pod:0}}-x"


class TestJson:
    def test_every_string_and_key_in_a_json_value_is_converted(self) -> None:
        identifiers = recording_side()
        value = {
            f"{RECORDED.pod}": [
                f"{RECORDED.commits[0]}",
                {"since": "2026-11-16T10:00:00Z", "n": 3, "ok": True},
            ],
            "none": None,
        }
        recorded = identifiers.normalize_json(value)
        assert recorded == {
            "{{pod:trigger}}": ["{{commit:0}}", {"since": "{{ts:-1815.25:T0:Z}}", "n": 3, "ok": True}],
            "none": None,
        }
        assert identifiers.resolve_json(recorded) == value


class TestResiduals:
    def test_volatile_identifiers_left_in_normalized_text_are_reported(self) -> None:
        text = "sha " + "a" * 40 + f" pod checkout-5f6d7c8b9-t8vwz and ts {RECORDED.gathered_at.isoformat()}"
        assert find_residuals(text, "checkout") == [
            "a" * 40,
            "2026-11-16T10:30:15.250000+00:00",
            "checkout-5f6d7c8b9-t8vwz",
        ]

    def test_normalized_text_has_none(self) -> None:
        identifiers = recording_side()
        text = (
            f"{RECORDED.pod} {RECORDED.old_pod} {RECORDED.commits[0]} {RECORDED.gathered_at.isoformat()} "
            f"{RECORDED.replicaset}"
        )
        assert find_residuals(identifiers.normalize(text), "checkout") == []

    def test_other_workloads_pods_are_not_this_workloads_residuals(self) -> None:
        assert find_residuals("cart-5f6d7c8b9-t8vwz", "checkout") == []


def test_round_trip_holds_for_random_text() -> None:
    """Whatever the text, normalizing then resolving against the same bundle returns the text."""
    rng = random.Random(20261116)
    identifiers = recording_side()
    pieces = [
        RECORDED.pod,
        RECORDED.old_pod,
        RECORDED.replicaset,
        RECORDED.commits[0],
        RECORDED.commits[1][:7],
        RECORDED.commits[1][:20],
        "2026-11-16T10:30:15Z",
        "2026-11-16 09:00:00.123456",
        "2026-11-16T10:30:15.123456789+05:30",
        "{{",
        "}}",
        "{{pod:0}}",
        "{",
        "checkout",
        "checkout-0",
        "deadbeef",
        "-",
        "/",
        " ",
        "\n",
        "x",
        "é",
        "12345",
    ]
    for _ in range(2000):
        text = "".join(rng.choice(pieces) for _ in range(rng.randint(0, 12)))
        assert identifiers.resolve(identifiers.normalize(text)) == text, text
