"""The unified availability report (decision 3): one table across both
providers, one row per model, one flag per rung. Drift sections below the
table. Exit 0 even with drift: drift is information, not failure.
"""

from __future__ import annotations

import json
from typing import Any

from rich.table import Table

from pi_model_sync.ladder import BEDROCK, Drift, ModelRow, Rung
from pi_model_sync.pipeline import Pipeline

RUNG_STYLES = {Rung.YES: "green", Rung.NO: "red", Rung.UNKNOWN: "yellow"}
RUNG_MARKS = {Rung.YES: "yes", Rung.NO: "no", Rung.UNKNOWN: "?"}
# Short provider labels keep the table readable; the JSON report carries full names.
PROVIDER_LABELS = {"amazon-bedrock": "bedrock", "ai-model-router": "router"}


def _cell(rung: Rung) -> str:
    return f"[{RUNG_STYLES[rung]}]{RUNG_MARKS[rung]}[/]"


def build_table(rows: list[ModelRow]) -> Table:
    table = Table(title="Model availability ladder", header_style="bold")
    table.add_column("provider")
    table.add_column("model")
    table.add_column("catalog", justify="center")
    table.add_column("entitled/deployed", justify="center")
    table.add_column("candidate", justify="center")
    table.add_column("invocable", justify="center")
    table.add_column("enabled", justify="center")
    table.add_column("region / note")
    for row in rows:
        note = " ".join(part for part in (row.region, row.note) if part)
        table.add_row(
            PROVIDER_LABELS.get(row.provider, row.provider),
            row.id,
            _cell(row.catalog),
            _cell(row.entitled_deployed),
            _cell(row.candidate),
            _cell(row.invocable),
            _cell(row.enabled),
            note,
        )
    return table


def drift_sections(drift: Drift) -> list[tuple[str, list[str], str]]:
    """(title, items, hint) per non-empty drift finding."""
    sections = []
    if drift.deployed_unknown_to_pi:
        sections.append(
            (
                "Deployed but unknown to pi",
                drift.deployed_unknown_to_pi,
                "run `pi-model-sync sync` to regenerate models.json",
            )
        )
    if drift.listed_but_undeployed:
        sections.append(
            (
                "Listed in pi but no longer deployed",
                drift.listed_but_undeployed,
                "run `pi-model-sync sync` to regenerate models.json",
            )
        )
    if drift.dead_patterns:
        sections.append(
            (
                "Dead patterns (match nothing currently invocable)",
                drift.dead_patterns,
                "edit enabledModels in the dotfiles settings.json",
            )
        )
    if drift.invocable_uncovered:
        sections.append(
            (
                "Invocable but covered by no pattern",
                drift.invocable_uncovered,
                "add a pattern to enabledModels to offer them in the picker",
            )
        )
    return sections


def report_json(pipeline: Pipeline, rows: list[ModelRow], drift: Drift, generated_at: str) -> str:
    """Stable, diffable JSON for scripting: sorted keys, fixed row order."""
    doc: dict[str, Any] = {
        "generatedAt": generated_at,
        "meta": {
            "regions": list(pipeline.cfg.regions),
            "defaultRegion": pipeline.cfg.default_region,
            "probed": pipeline.probed,
            "routerReachable": pipeline.router.reachable,
            "routerError": pipeline.router.error,
            "degradedRegions": pipeline.entitlements.degraded,
        },
        "rows": [
            {
                "provider": row.provider,
                "id": row.id,
                "catalog": str(row.catalog),
                "entitledDeployed": str(row.entitled_deployed),
                "candidate": str(row.candidate),
                "invocable": str(row.invocable),
                "enabled": str(row.enabled),
                "region": row.region,
                "note": row.note,
            }
            for row in rows
        ],
        "drift": {
            "deployedUnknownToPi": drift.deployed_unknown_to_pi,
            "listedButUndeployed": drift.listed_but_undeployed,
            "deadPatterns": drift.dead_patterns,
            "invocableUncovered": drift.invocable_uncovered,
        },
    }
    return json.dumps(doc, indent=2, sort_keys=True) + "\n"


def summarize(pipeline: Pipeline) -> str:
    """One-line counts per provider for the report header."""
    invocable_bedrock = pipeline.invocable_bedrock()
    lines = [
        f"regions scanned: {', '.join(pipeline.cfg.regions)} (default: {pipeline.cfg.default_region})",
        f"bedrock: {len(pipeline.catalog_ids(BEDROCK))} catalog, {len(pipeline.bedrock_candidates)} candidates, "
        f"{len(invocable_bedrock)} invocable" + ("" if pipeline.probed else " (--no-probe: candidates unchecked)"),
    ]
    if pipeline.router.reachable:
        lines.append(
            f"router: {len(pipeline.catalog_ids('ai-model-router'))} catalog, {len(pipeline.router.deployed)} deployed, "
            f"{len(pipeline.invocable_router())} invocable"
            + ("" if pipeline.probed else " (--no-probe: candidates unchecked)")
        )
    else:
        lines.append(f"router: unreachable ({pipeline.router.error}) -- router rungs unknown")
    for region, error in pipeline.entitlements.degraded.items():
        lines.append(f"degraded region {region}: {error}")
    return "\n".join(lines)
