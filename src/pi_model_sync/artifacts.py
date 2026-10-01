"""The three artifacts, and the rules around them (decision 4).

- `enabledModels` in settings.json is the single store of curated patterns.
  The tool READS it as the pattern list, validates in place, and writes
  nothing: dead patterns and uncovered invocable models are flagged in the
  report, a human edits the list. This removes the bash script's duplicate
  CURATED_PATTERNS store and its "hand-added entries would be dropped"
  warning class.
- `bedrock-models.json`: the {modelId: region} map of every invocable
  Bedrock model across all scanned regions. Key-sorted, atomic write. The
  live file is a stow symlink, so writing the dotfiles copy is enough.
- `models.json` router section: `providers["ai-model-router"].models` is
  regenerated from the live deployment. Everything else (baseUrl, api,
  apiKey, amazon-bedrock.modelOverrides, formatting) is preserved
  byte-for-byte via a string-aware splice of just the models array. Also a
  stow symlink; writing the dotfiles copy is enough.

settings.json is NOT stowed (pi rewrites the live file at runtime), so the
validator also warns when live and dotfiles copies disagree on
`enabledModels`.
"""

from __future__ import annotations

import json
import os
import re
import tempfile
from fnmatch import fnmatchcase
from pathlib import Path
from typing import Any

THINKING_LEVELS = ("off", "minimal", "low", "medium", "high", "xhigh", "max")


def strip_pin(pattern: str) -> str:
    """`gpt-5.6-luna:max` -> `gpt-5.6-luna` (thinking-level pin suffix)."""
    base, sep, level = pattern.rpartition(":")
    if sep and level in THINKING_LEVELS:
        return base
    return pattern


def read_patterns(settings_path: Path) -> list[str]:
    """The curated pattern list = the current `enabledModels` entries."""
    data = json.loads(settings_path.read_text())
    return [p for p in data.get("enabledModels", []) if isinstance(p, str)]


def validate_patterns(patterns: list[str], matchable_ids: set[str]) -> tuple[list[str], set[str], list[str]]:
    """Glob-match (fnmatch) pin-stripped patterns against matchable ids.

    Returns (dead_patterns, covered_ids, uncovered_ids). A dead pattern
    matches nothing currently invocable; an uncovered id is invocable but no
    pattern offers it in the picker.
    """
    dead: list[str] = []
    covered: set[str] = set()
    for raw in patterns:
        pattern = strip_pin(raw)
        matches = {mid for mid in matchable_ids if fnmatchcase(mid, pattern)}
        if matches:
            covered |= matches
        else:
            dead.append(raw)
    uncovered = sorted(matchable_ids - covered)
    return dead, covered, uncovered


def settings_drift(dotfiles_settings: Path, live_settings: Path) -> str | None:
    """Warn when the live (not stowed, pi-rewritten) settings disagree with
    the dotfiles copy on `enabledModels`. None means in sync."""
    try:
        dot = read_patterns(dotfiles_settings)
        live = read_patterns(live_settings)
    except (OSError, json.JSONDecodeError) as e:
        return f"could not compare settings files: {e}"
    if dot == live:
        return None
    return (
        f"live {live_settings} enabledModels differs from the dotfiles copy "
        f"({len(live)} vs {len(dot)} entries). The dotfiles copy is the source "
        "of truth: edit it there, then refresh the live file."
    )


def atomic_write(path: Path, text: str) -> None:
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=path.name, suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(text)
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise


def render_bedrock_models(mapping: dict[str, str], default_region: str, generated_at: str) -> str:
    """jq -S-compatible rendering: sorted keys, 2-space indent, trailing newline."""
    doc = {"generatedAt": generated_at, "defaultRegion": default_region, "models": dict(sorted(mapping.items()))}
    return json.dumps(doc, indent=2, sort_keys=True) + "\n"


def write_bedrock_models(path: Path, mapping: dict[str, str], default_region: str, generated_at: str) -> None:
    atomic_write(path, render_bedrock_models(mapping, default_region, generated_at))


def _find_value_span(text: str, start: int) -> tuple[int, int]:
    """String-aware bracket match: given the index of an opening `[` or `{`,
    return the (start, end) span of the whole JSON value."""
    open_to_close = {"[": "]", "{": "}"}
    stack = [open_to_close[text[start]]]
    in_string = False
    escaped = False
    i = start + 1
    while i < len(text) and stack:
        ch = text[i]
        if escaped:
            escaped = False
        elif in_string:
            if ch == "\\":
                escaped = True
            elif ch == '"':
                in_string = False
        elif ch == '"':
            in_string = True
        elif ch in open_to_close:
            stack.append(open_to_close[ch])
        elif ch in "]}" and (not stack or ch != stack.pop()):
            raise ValueError(f"unbalanced JSON near offset {i}")
        i += 1
    if stack:
        raise ValueError("unterminated JSON value")
    return start, i


def splice_router_models(text: str, new_models: list[dict[str, Any]]) -> str:
    """Replace exactly the `providers["ai-model-router"].models` array in
    `models.json` text, preserving every other byte (formatting included).

    The result is re-parsed and checked against the original: everything must
    be identical except that one array.
    """
    original = json.loads(text)
    router = original.get("providers", {}).get("ai-model-router")
    if not isinstance(router, dict) or "models" not in router:
        raise ValueError('models.json has no providers["ai-model-router"].models')

    marker = '"ai-model-router"'
    provider_pos = text.find(marker)
    if provider_pos == -1 or text.find(marker, provider_pos + 1) != -1:
        raise ValueError('expected exactly one "ai-model-router" key in models.json')
    key_pos = text.find('"models"', provider_pos)
    if key_pos == -1:
        raise ValueError('no "models" key after "ai-model-router"')
    bracket_pos = text.find("[", key_pos)
    start, end = _find_value_span(text, bracket_pos)

    # Match the file's style: 2-space indent, "models" key sits at depth 3
    # (root > providers > ai-model-router), so array items indent by 8. The
    # opening bracket stays on the key line; the short flat "input" arrays
    # stay inline, like the hand-written entries.
    rendered = json.dumps(new_models, indent=2)
    rendered = re.sub(
        r'"input": \[\s*("(?:text|image)"(?:\s*,\s*"(?:text|image)")*)\s*\]',
        lambda m: '"input": [' + re.sub(r"\s*,\s*", ", ", m.group(1)) + "]",
        rendered,
    )
    lines = rendered.splitlines()
    indented = "\n".join([lines[0], *("      " + line for line in lines[1:])])
    spliced = text[:start] + indented + text[end:]

    check = json.loads(spliced)
    check["providers"]["ai-model-router"]["models"] = original["providers"]["ai-model-router"]["models"]
    if check != original:
        raise ValueError("splice changed something outside the router models array")
    return spliced


def render_models_json(text: str, new_models: list[dict[str, Any]]) -> str:
    """Regenerate the router section. `new_models` is sorted by id for
    deterministic, near-empty diffs on unchanged runs."""
    return splice_router_models(text, sorted(new_models, key=lambda m: m["id"]))


def write_models_json(path: Path, new_models: list[dict[str, Any]]) -> None:
    atomic_write(path, render_models_json(path.read_text(), new_models))
