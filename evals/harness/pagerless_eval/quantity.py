"""Parse Kubernetes resource quantities for deterministic comparisons.

Only the forms that memory resources use in this repository are accepted:
decimal numbers with an optional exponent, an optional binary suffix (Ki, Mi,
Gi, Ti, Pi, Ei), or an optional decimal suffix (k, M, G, T, P, E). The result is
an exact Decimal number of bytes, so a boundary such as 1.5 times 32Mi is
compared without floating-point error.
"""

from __future__ import annotations

import re
from decimal import Decimal

# Suffixes are case-sensitive in Kubernetes: 128Mi is 128 binary mebibytes and
# 128M is 128 decimal megabytes.
_SUFFIXES: dict[str, Decimal] = {
    "": Decimal(1),
    "k": Decimal(1000),
    "M": Decimal(1000) ** 2,
    "G": Decimal(1000) ** 3,
    "T": Decimal(1000) ** 4,
    "P": Decimal(1000) ** 5,
    "E": Decimal(1000) ** 6,
    "Ki": Decimal(1024),
    "Mi": Decimal(1024) ** 2,
    "Gi": Decimal(1024) ** 3,
    "Ti": Decimal(1024) ** 4,
    "Pi": Decimal(1024) ** 5,
    "Ei": Decimal(1024) ** 6,
}

_QUANTITY = re.compile(r"^([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)([A-Za-z]*)$")


class QuantityError(ValueError):
    """A resource quantity could not be parsed."""


def parse_memory(value: str) -> Decimal:
    """Return the number of bytes that a Kubernetes memory quantity denotes."""
    text = value.strip()
    match = _QUANTITY.match(text)
    if match is None:
        raise QuantityError(f"{value!r} is not a Kubernetes resource quantity")
    number, suffix = match.groups()
    if suffix == "m":
        # A milli-byte is a valid quantity but never a memory size. Treating it
        # as 0.001 bytes would let a malformed Proposal pass a bound check.
        raise QuantityError(f"{value!r} uses the milli suffix, which is not a memory size")
    multiplier = _SUFFIXES.get(suffix)
    if multiplier is None:
        raise QuantityError(f"{value!r} has the unknown resource suffix {suffix!r}")
    return Decimal(number) * multiplier


def format_memory(value: Decimal) -> str:
    """Return a quantity for a byte count, using the largest exact binary suffix."""
    for suffix, multiplier in (
        ("Ei", _SUFFIXES["Ei"]),
        ("Pi", _SUFFIXES["Pi"]),
        ("Ti", _SUFFIXES["Ti"]),
        ("Gi", _SUFFIXES["Gi"]),
        ("Mi", _SUFFIXES["Mi"]),
        ("Ki", _SUFFIXES["Ki"]),
    ):
        if value % multiplier == 0:
            return f"{(value // multiplier):g}{suffix}"
    return f"{value:g}"
