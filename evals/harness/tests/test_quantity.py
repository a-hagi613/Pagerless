"""Tests for exact Kubernetes memory-quantity parsing."""

from __future__ import annotations

import unittest
from decimal import Decimal

from pagerless_eval.quantity import QuantityError, format_memory, parse_memory


class ParseMemoryTest(unittest.TestCase):
    def test_binary_and_decimal_suffixes(self) -> None:
        self.assertEqual(parse_memory("32Mi"), Decimal(32 * 1024**2))
        self.assertEqual(parse_memory("128Mi"), Decimal(128 * 1024**2))
        self.assertEqual(parse_memory("1.5Gi"), Decimal(1536 * 1024**2))
        self.assertEqual(parse_memory("256Gi"), Decimal(256 * 1024**3))
        self.assertEqual(parse_memory("500M"), Decimal(500_000_000))
        self.assertEqual(parse_memory("1024"), Decimal(1024))

    def test_exponents_and_whitespace(self) -> None:
        self.assertEqual(parse_memory(" 1e9 "), Decimal(1_000_000_000))
        self.assertEqual(parse_memory("1.5e2Ki"), Decimal(150 * 1024))

    def test_rejects_invalid_quantities(self) -> None:
        for value in ("not-a-size", "", "64m", "12Zi", "Mi", "1.2.3Mi", "128MiB"):
            with self.subTest(value=value), self.assertRaises(QuantityError):
                parse_memory(value)

    def test_format_uses_the_largest_exact_binary_suffix(self) -> None:
        self.assertEqual(format_memory(parse_memory("128Mi")), "128Mi")
        self.assertEqual(format_memory(parse_memory("1.5Gi")), "1536Mi")
        self.assertEqual(format_memory(Decimal(1536)), "1536")


if __name__ == "__main__":
    unittest.main()
