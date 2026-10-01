"""Rung 1, the catalog: parse `pi --list-models` into structured records.

The output is a fixed-width table (`provider  model  context  max-out
thinking  images`). Model ids contain no spaces, so whitespace splitting
after skipping the header is enough. The Bedrock catalog is static and
bundled with pi; the router catalog is whatever `models.json` defines. No
caching: pi is called fresh each run.
"""

from __future__ import annotations

import subprocess

from pi_model_sync.ladder import CatalogEntry


class CatalogError(Exception):
    """`pi --list-models` failed or parsed to nothing."""


def parse_catalog(text: str) -> list[CatalogEntry]:
    """Parse `pi --list-models` output. Header line is skipped."""
    entries: list[CatalogEntry] = []
    for line in text.splitlines():
        if not line.strip() or line.startswith("provider"):
            continue
        parts = line.split()
        if len(parts) != 6:
            continue
        provider, model, context, max_out, thinking, images = parts
        entries.append(
            CatalogEntry(
                provider=provider, id=model, context=context, max_out=max_out, thinking=thinking, images=images
            )
        )
    return entries


def load_catalog() -> list[CatalogEntry]:
    """Run `pi --list-models` and parse it."""
    try:
        proc = subprocess.run(
            ["pi", "--list-models"],
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
    except FileNotFoundError as e:
        raise CatalogError("pi not found on PATH") from e
    entries = parse_catalog(proc.stdout)
    if not entries:
        raise CatalogError(f"pi --list-models produced no rows (exit {proc.returncode}): {proc.stderr.strip()}")
    return entries
