"""Normalization of volatile identifiers for Replay.

A recording must replay against a different run of the same Scenario, in which
every pod name, timestamp, and commit SHA is new. When a model response is
recorded, each volatile identifier is replaced by a placeholder. When the
recording is replayed, each placeholder is resolved from the current Context
Bundle. Both directions are driven by an `IdentifierMap` built from a bundle.

Placeholders have the form `{{kind:argument}}`:

- `{{pod:N}}` is the Nth pod of the bundle in creation order, and
  `{{pod:trigger}}` is the pod that raised the Trigger.
- `{{replicaset:N}}` is the Nth ReplicaSet that owns the bundle's pods, in the
  order of first appearance.
- `{{commit:N}}` is the Nth Sandbox commit of the bundle, newest first, and
  `{{commit:N:L}}` is its first L characters.
- `{{ts:OFFSET:SHAPE:ZONE}}` is the instant that lies OFFSET seconds after the
  bundle's `gathered_at`, written with SHAPE and ZONE. SHAPE is the date and
  time separator (`T`, `t`, or `_` for a space) followed by the number of
  fractional digits. ZONE is `Z`, `z`, a numeric offset such as `+01:00`, or
  `-` for none.
- `{{lbrace}}` is a literal `{{` that occurred in the original text, so that
  recorded text can never be mistaken for a placeholder.

Only identifiers that the Kubernetes naming scheme generates are normalized.
Anything volatile that normalization cannot resolve from a bundle is reported
by `find_residuals`, so that a recording that would not replay is refused when
it is written and not discovered when it is replayed.
"""

from __future__ import annotations

import re
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from decimal import Decimal
from typing import Any

from pagerless_agent.bundle import ContextBundle


class ReplayError(Exception):
    """A recording cannot be recorded, loaded, or replayed."""


_NS = 1_000_000_000
_EPOCH = datetime(1970, 1, 1)

# Kubernetes generates object name suffixes from this alphabet, which has no vowels or look-alike digits.
_GENERATED = "bcdfghjklmnpqrstvwxz2456789"
_POD_NAME = re.compile(rf"^(?P<owner>.+)-(?P<suffix>[{_GENERATED}]{{5}})$")
_REPLICASET_POD = re.compile(rf"^(?P<replicaset>.+-[{_GENERATED}]{{6,10}})-[{_GENERATED}]{{5}}$")

_TIMESTAMP = re.compile(
    r"(?<![0-9])(?P<year>\d{4})-(?P<month>\d{2})-(?P<day>\d{2})(?P<sep>[Tt ])"
    r"(?P<hour>\d{2}):(?P<minute>\d{2}):(?P<second>\d{2})(?:\.(?P<frac>\d{1,9}))?"
    r"(?P<zone>[Zz]|[+-]\d{2}(?::?\d{2})?)?(?![0-9])"
)
_PLACEHOLDER = re.compile(r"\{\{(?P<kind>[a-z]+)(?::(?P<arg>[^{}]*))?\}\}")
_FULL_SHA = re.compile(r"(?<![0-9A-Za-z])[0-9a-f]{40}(?![0-9A-Za-z])")
_OFFSET = re.compile(r"^-?\d+(?:\.\d{1,9})?$")
_SHAPE = re.compile(r"^[Tt_][0-9]$")
_ZONE = re.compile(r"^(?:[Zz-]|[+-]\d{2}(?::?\d{2})?)$")
_INDEX = re.compile(r"^\d+$")
_COMMIT_ARG = re.compile(r"^\d+(?::(?:[7-9]|[1-5]\d|6[0-4]))?$")


@dataclass(frozen=True)
class _Instant:
    """A parsed timestamp: nanoseconds since the epoch and how it was written."""

    ns: int
    sep: str
    digits: int
    zone: str


def _parse_zone(zone: str) -> int:
    """Return the zone's offset from UTC in nanoseconds."""
    if zone in ("", "-", "Z", "z"):
        return 0
    sign = -1 if zone[0] == "-" else 1
    digits = zone[1:].replace(":", "")
    hours, minutes = int(digits[:2]), int(digits[2:4] or "0")
    return sign * (hours * 3600 + minutes * 60) * _NS


def _parse_timestamp(m: re.Match[str]) -> _Instant | None:
    """Parse a timestamp match, or return None when it is not a real date and time."""
    try:
        stamp = datetime(
            int(m["year"]), int(m["month"]), int(m["day"]), int(m["hour"]), int(m["minute"]), int(m["second"])
        )
    except ValueError:
        return None
    frac = m["frac"] or ""
    zone = m["zone"] or "-"
    elapsed = stamp - _EPOCH
    local_ns = (elapsed.days * 86400 + elapsed.seconds) * _NS + int(frac.ljust(9, "0") or "0")
    return _Instant(ns=local_ns - _parse_zone(zone), sep=m["sep"], digits=len(frac), zone=zone)


def _format_offset(ns: int) -> str:
    """Write a signed nanosecond count as decimal seconds with no trailing zeros."""
    sign = "-" if ns < 0 else ""
    whole, frac = divmod(abs(ns), _NS)
    text = f"{whole}.{frac:09d}".rstrip("0").rstrip(".")
    return sign + text


def _parse_offset(text: str) -> int:
    return int(Decimal(text) * _NS)


def _format_timestamp(ns: int, sep: str, digits: int, zone: str) -> str:
    local_ns = ns + _parse_zone(zone)
    seconds, frac_ns = divmod(local_ns, _NS)
    stamp = _EPOCH + timedelta(seconds=seconds)
    separator = " " if sep == "_" else sep
    text = (
        f"{stamp.year:04d}-{stamp.month:02d}-{stamp.day:02d}{separator}"
        f"{stamp.hour:02d}:{stamp.minute:02d}:{stamp.second:02d}"
    )
    if digits:
        text += "." + f"{frac_ns:09d}"[:digits]
    return text + ("" if zone == "-" else zone)


class IdentifierMap:
    """Maps the volatile identifiers of one Context Bundle to placeholders and back."""

    def __init__(self, bundle: ContextBundle) -> None:
        self._reference_ns = _datetime_ns(bundle.gathered_at)
        self._workload = bundle.workload.name
        self._pods = [p.name for p in bundle.pods]
        self._trigger = bundle.triggering_pod
        self._commits = [c.sha for c in bundle.commits]
        self._replicasets: list[str] = []
        for name in self._pods:
            m = _REPLICASET_POD.match(name)
            if m and m["replicaset"] not in self._replicasets:
                self._replicasets.append(m["replicaset"])

        # Names to placeholders. The longest name is tried first, so that a pod
        # is never read as its ReplicaSet.
        names: dict[str, str] = {}
        if self._trigger and _POD_NAME.match(self._trigger):
            names[self._trigger] = "{{pod:trigger}}"
        for index, name in enumerate(self._pods):
            if _POD_NAME.match(name):
                names.setdefault(name, f"{{{{pod:{index}}}}}")
        for index, name in enumerate(self._replicasets):
            names.setdefault(name, f"{{{{replicaset:{index}}}}}")
        self._names = names
        ordered = sorted(names, key=len, reverse=True)
        name_alternatives = "|".join(re.escape(n) for n in ordered) or r"(?!)"
        self._normalizer = re.compile(
            rf"(?P<brace>\{{\{{)|(?P<ts>{_TIMESTAMP.pattern})"
            rf"|(?<![0-9A-Za-z])(?P<name>{name_alternatives})(?![0-9A-Za-z])"
            r"|(?P<hex>(?<![0-9A-Za-z])[0-9a-f]{7,64}(?![0-9A-Za-z]))"
        )

    # Recording.

    def normalize(self, text: str) -> str:
        """Replace the volatile identifiers in text with placeholders."""

        def replace(m: re.Match[str]) -> str:
            if m["brace"]:
                return "{{lbrace}}"
            if m["ts"]:
                return self._normalize_timestamp(m["ts"])
            if m["name"]:
                return self._names[m["name"]]
            return self._normalize_hex(m["hex"])

        return self._normalizer.sub(replace, text)

    def _normalize_timestamp(self, text: str) -> str:
        found = _TIMESTAMP.fullmatch(text)
        instant = _parse_timestamp(found) if found else None
        if instant is None:
            return text
        offset = _format_offset(instant.ns - self._reference_ns)
        return f"{{{{ts:{offset}:{instant.sep.replace(' ', '_')}{instant.digits}:{instant.zone}}}}}"

    def _normalize_hex(self, text: str) -> str:
        for index, sha in enumerate(self._commits):
            if sha.startswith(text):
                return f"{{{{commit:{index}}}}}" if text == sha else f"{{{{commit:{index}:{len(text)}}}}}"
        return text

    def normalize_json(self, value: Any) -> Any:
        """Normalize every string, and every key, in a JSON value."""
        return _map_strings(value, self.normalize)

    # Replaying.

    def resolve(self, text: str) -> str:
        """Replace the placeholders in text with the identifiers of this bundle."""
        found = list(_PLACEHOLDER.finditer(text))
        if text.count("{{") != len(found):
            raise ReplayError(f"malformed placeholder in recorded text: {_excerpt(text)}")

        def replace(m: re.Match[str]) -> str:
            return self._resolve_one(m["kind"], m["arg"], m.group(0))

        return _PLACEHOLDER.sub(replace, text)

    def _resolve_one(self, kind: str, arg: str | None, source: str) -> str:
        if kind == "lbrace" and arg is None:
            return "{{"
        if kind == "pod" and arg == "trigger":
            if not self._trigger:
                raise ReplayError(f"{source}: the Context Bundle has no triggering pod")
            return self._trigger
        if kind in ("pod", "replicaset") and arg is not None and _INDEX.match(arg):
            items = self._pods if kind == "pod" else self._replicasets
            return self._at(items, int(arg), kind, source)
        if kind == "commit" and arg is not None:
            index, _, length = arg.partition(":")
            if _INDEX.match(index) and (not length or (_INDEX.match(length) and int(length) >= 7)):
                sha = self._at(self._commits, int(index), "commit", source)
                return sha[: int(length)] if length else sha
        if kind == "ts" and arg is not None:
            return self._resolve_timestamp(arg, source)
        raise ReplayError(f"unknown placeholder {source}")

    def _resolve_timestamp(self, arg: str, source: str) -> str:
        parts = arg.split(":", 2)
        if len(parts) == 3 and _OFFSET.match(parts[0]) and _SHAPE.match(parts[1]) and _ZONE.match(parts[2]):
            return _format_timestamp(
                self._reference_ns + _parse_offset(parts[0]), parts[1][0], int(parts[1][1]), parts[2]
            )
        raise ReplayError(f"unknown placeholder {source}")

    @staticmethod
    def _at(items: list[str], index: int, what: str, source: str) -> str:
        if index >= len(items):
            raise ReplayError(
                f"{source}: the Context Bundle has {len(items)} {what}(s) and cannot resolve index {index}"
            )
        return items[index]

    def resolve_json(self, value: Any) -> Any:
        """Resolve every string, and every key, in a JSON value."""
        return _map_strings(value, self.resolve)


def find_residuals(text: str, workload: str) -> list[str]:
    """Return the volatile identifiers that remain in already normalized text.

    These are full Git SHAs, timestamps that could not be parsed, and the names
    of the Workload's pods. A recording that contains one cannot be replayed
    faithfully, because nothing resolves it from the current bundle.
    """
    residuals = [m.group(0) for m in _FULL_SHA.finditer(text)]
    residuals += [m.group(0) for m in _TIMESTAMP.finditer(text)]
    pod_like = re.compile(
        rf"(?<![0-9A-Za-z]){re.escape(workload)}-[{_GENERATED}]{{6,10}}-[{_GENERATED}]{{5}}(?![0-9A-Za-z])"
    )
    residuals += [m.group(0) for m in pod_like.finditer(text)]
    return residuals


def _datetime_ns(value: datetime) -> int:
    """Return an aware datetime as nanoseconds since the epoch."""
    delta = value.astimezone(UTC).replace(tzinfo=None) - _EPOCH
    return (delta.days * 86400 + delta.seconds) * _NS + delta.microseconds * 1000


def _map_strings(value: Any, fn: Callable[[str], str]) -> Any:
    if isinstance(value, str):
        return fn(value)
    if isinstance(value, list):
        return [_map_strings(item, fn) for item in value]
    if isinstance(value, dict):
        return {fn(key): _map_strings(item, fn) for key, item in value.items()}
    return value


def _excerpt(text: str, limit: int = 80) -> str:
    return text if len(text) <= limit else text[:limit] + "..."


def check_placeholders(text: str) -> None:
    """Raise ReplayError if text has a malformed placeholder or a stray `{{`."""
    found = list(_PLACEHOLDER.finditer(text))
    if text.count("{{") != len(found):
        raise ReplayError(f"malformed placeholder in recorded text: {_excerpt(text)}")
    for m in found:
        kind, arg = m["kind"], m["arg"]
        valid = (
            (kind == "lbrace" and arg is None)
            or (kind == "pod" and arg is not None and (arg == "trigger" or bool(_INDEX.match(arg))))
            or (kind == "replicaset" and arg is not None and bool(_INDEX.match(arg)))
            or (kind == "commit" and arg is not None and bool(_COMMIT_ARG.match(arg)))
            or (kind == "ts" and arg is not None and _valid_timestamp_arg(arg))
        )
        if not valid:
            raise ReplayError(f"unknown placeholder {m.group(0)}")


def _valid_timestamp_arg(arg: str) -> bool:
    parts = arg.split(":", 2)
    return len(parts) == 3 and bool(
        _OFFSET.match(parts[0]) and _SHAPE.match(parts[1]) and _ZONE.match(parts[2])
    )
