#!/usr/bin/env python3
"""Verify that a Roost price book's declared hash is its canonical content hash.

The marketing site (roost-website/scripts/generate-prices.mjs) hashes a price
book by sorting object keys at every level, removing the top-level "hash"
field, serializing with no whitespace and hashing the UTF-8 bytes. The Console
must produce the same value, otherwise a page generated from /v1/pricing/book
shows a different hash than the API's ETag.

This script is read-only: it never writes the book or the page. It is meant to
run in CI and before a page regeneration.

Examples

    # Verify a local fixture
    python3 scripts/check-price-hash.py --book ../../../worktrees/fixtures/pricing-book.v1.json

    # Compare the live public book with the page embedded hash (staging only)
    python3 scripts/check-price-hash.py \
        --book https://console.example/v1/pricing/book \
        --page ../../../roost-website/relay.html

    # Prove the price *values* are in sync while the shape/hash differs
    python3 scripts/check-price-hash.py \
        --book https://console.example/v1/pricing/book \
        --compare ../../../worktrees/fixtures/pricing-book.v1.json

Exit codes: 0 = aligned, 1 = mismatch, 2 = usage/read/JSON error.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import urllib.request
from pathlib import Path


def canonical_json(value) -> str:
    # Same rule as generate-prices.mjs canonicalJson: sorted keys recursively,
    # arrays keep order, no whitespace, UTF-8.
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def content_hash(book: dict) -> str:
    body = {key: value for key, value in book.items() if key != "hash"}
    return "sha256:" + hashlib.sha256(canonical_json(body).encode("utf-8")).hexdigest()


def insertion_order_json(book: dict) -> str:
    """Serialize with the order the JSON document arrived in.

    Go's encoding/json writes struct fields in declaration order and maps in
    sorted-key order. For an API response, preserving the parsed document order
    therefore reproduces the legacy (pre-canonicalization) Console hash. That
    lets the script say whether a mismatch is pure canonicalization or the
    content itself.
    """
    body = {key: value for key, value in book.items() if key != "hash"}
    return json.dumps(body, separators=(",", ":"), ensure_ascii=False)


def legacy_hash(book: dict) -> str:
    return "sha256:" + hashlib.sha256(insertion_order_json(book).encode("utf-8")).hexdigest()


def read_book(ref: str) -> dict:
    if re.match(r"^https?://", ref, re.IGNORECASE):
        request = urllib.request.Request(ref, headers={"Accept": "application/json"})
        with urllib.request.urlopen(request, timeout=30) as response:
            raw = response.read().decode("utf-8")
    else:
        raw = Path(ref).expanduser().read_text(encoding="utf-8")
    book = json.loads(raw)
    if not isinstance(book, dict):
        raise ValueError(f"{ref}: price book must be a JSON object")
    if not isinstance(book.get("models"), list) or not book["models"]:
        raise ValueError(f"{ref}: price book has no models[]")
    return book


def read_page_hash(page: str) -> str | None:
    text = Path(page).expanduser().read_text(encoding="utf-8")
    match = re.search(r'data-price-hash="(sha256:[0-9a-f]{64})"', text)
    return match.group(1) if match else None


def price_map(book: dict) -> dict[tuple[str, int, str], int]:
    """(model, tier_max_input, component) -> retail minor per million."""
    out: dict[tuple[str, int, str], int] = {}
    for model in book.get("models", []):
        name = str(model.get("model", ""))
        for tier in model.get("tiers", []):
            max_input = int(tier.get("max_input", 0))
            for component, price in (tier.get("prices") or {}).items():
                out[(name, max_input, str(component))] = int(price)
    return out


def compare_books(left: dict, right: dict) -> list[str]:
    left_prices = price_map(left)
    right_prices = price_map(right)
    problems: list[str] = []
    for key in sorted(left_prices.keys() - right_prices.keys()):
        problems.append(f"only in first book: {key[0]} tier={key[1]} {key[2]}={left_prices[key]}")
    for key in sorted(right_prices.keys() - left_prices.keys()):
        problems.append(f"only in second book: {key[0]} tier={key[1]} {key[2]}={right_prices[key]}")
    for key in sorted(left_prices.keys() & right_prices.keys()):
        if left_prices[key] != right_prices[key]:
            problems.append(
                f"{key[0]} tier={key[1]} {key[2]}: {left_prices[key]} != {right_prices[key]}"
            )
    return problems


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description="Verify a Roost price book against the website's canonical hash rule.",
    )
    parser.add_argument("--book", required=True, help="price book path or URL")
    parser.add_argument("--page", help="optional relay.html to read data-price-hash from")
    parser.add_argument(
        "--compare",
        help="optional second book; compares only model/tier/component retail prices",
    )
    parser.add_argument("--json", action="store_true", help="machine-readable output")
    args = parser.parse_args(argv)

    try:
        book = read_book(args.book)
    except Exception as error:  # noqa: BLE001 - CLI must report the reason
        print(f"check-price-hash: cannot read {args.book}: {error}", file=sys.stderr)
        return 2

    computed = content_hash(book)
    declared = book.get("hash")
    legacy = legacy_hash(book)
    problems: list[str] = []

    if declared is not None and declared != computed:
        problems.append(
            f"declared hash {declared} != canonical hash {computed}"
        )
        if declared == legacy:
            problems.append(
                "  diagnosis: the declared hash uses the legacy insertion-order serialization "
                "(Go struct field order). Canonicalize sorted keys at every level."
            )
        else:
            problems.append(
                "  diagnosis: neither canonical nor legacy order matches; the content itself "
                "differs from what produced the declared hash (stale fixture/seed)."
            )

    page_hash = None
    if args.page:
        try:
            page_hash = read_page_hash(args.page)
        except Exception as error:  # noqa: BLE001
            print(f"check-price-hash: cannot read page {args.page}: {error}", file=sys.stderr)
            return 2
        if page_hash is None:
            problems.append(f"{args.page}: data-price-hash not found")
        elif page_hash != computed:
            problems.append(f"page hash {page_hash} != canonical hash {computed}")

    compare_problems: list[str] = []
    if args.compare:
        try:
            other = read_book(args.compare)
        except Exception as error:  # noqa: BLE001
            print(f"check-price-hash: cannot read {args.compare}: {error}", file=sys.stderr)
            return 2
        compare_problems = compare_books(book, other)

    result = {
        "book": args.book,
        "version": book.get("version"),
        "models": len(book.get("models", [])),
        "declared_hash": declared,
        "computed_hash": computed,
        "legacy_hash": legacy,
        "page": args.page,
        "page_hash": page_hash,
        "ok": not problems,
        "problems": problems,
        "compare": args.compare,
        "compare_ok": not compare_problems,
        "compare_problems": compare_problems,
    }
    if args.json:
        print(json.dumps(result, indent=2, ensure_ascii=False))
    else:
        print(f"book:     {args.book}")
        print(f"version:  {book.get('version')} ({len(book.get('models', []))} models)")
        print(f"declared: {declared}")
        print(f"canonical:{computed}")
        print(f"legacy:   {legacy}")
        if args.page:
            print(f"page:     {page_hash}")
        if problems:
            print("MISMATCH:", file=sys.stderr)
            for problem in problems:
                print(f"  {problem}", file=sys.stderr)
        else:
            print("OK: declared hash is the canonical content hash")
        if args.compare:
            if compare_problems:
                print(
                    f"COMPARE {args.compare}: {len(compare_problems)} price difference(s)",
                    file=sys.stderr,
                )
                for problem in compare_problems[:50]:
                    print(f"  {problem}", file=sys.stderr)
            else:
                print(f"COMPARE {args.compare}: same model/tier/component retail prices")
    return 0 if not problems else 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
