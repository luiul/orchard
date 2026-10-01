"""pi-model-sync: discover and sync the models pi can actually use.

Covers both pi providers, `amazon-bedrock` and `ai-model-router`, and walks
the full availability ladder for each: catalog (pi knows the id), entitled
(Bedrock account may call it) or deployed (router serves it), candidate
(worth probing), invocable (survives a live probe through pi), and enabled
(offered in /model and Ctrl+P via `enabledModels` in settings.json).

Writes three artifacts: `enabledModels` in settings.json (validate only,
patterns are never auto-edited), `bedrock-models.json` (model to region
map), and the router section of `models.json` (generated from the live
gateway).

Design, decisions, and the implementation plan:
https://github.com/luiul/dotfiles/issues/27
"""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime
from typing import Annotated

import typer
from rich.console import Console
from rich.table import Table

import pi_model_sync
from pi_model_sync.artifacts import atomic_write, render_bedrock_models, render_models_json, settings_drift
from pi_model_sync.bedrock import SsoInvalidError
from pi_model_sync.catalog import CatalogError, load_catalog
from pi_model_sync.paths import Config, ConfigError
from pi_model_sync.pipeline import Pipeline, RouterUnreachableError, run_pipeline
from pi_model_sync.probe import ProbeStatus
from pi_model_sync.report import build_table, drift_sections, report_json, summarize

APP_HELP = """\
Discover and sync the models [bold]pi[/] can actually use.

Covers both providers, [cyan]amazon-bedrock[/] and
[cyan]ai-model-router[/], across the whole availability ladder:
catalog → entitled/deployed → candidate → invocable → enabled.

Design and decisions: https://github.com/luiul/dotfiles/issues/27
"""

app = typer.Typer(
    help=APP_HELP,
    no_args_is_help=True,
    add_completion=False,
    pretty_exceptions_show_locals=False,
)

stdout = Console()
stderr = Console(stderr=True)


def _version_callback(value: bool) -> None:
    if value:
        stdout.print(f"pi-model-sync {pi_model_sync.__version__}")
        raise typer.Exit


@app.callback()
def main(
    version: Annotated[
        bool, typer.Option("--version", callback=_version_callback, is_eager=True, help="Show version and exit.")
    ] = False,
) -> None:
    """Discover and sync the models pi can actually use."""


def _config() -> Config:
    try:
        return Config.from_env()
    except ConfigError as e:
        stderr.print(f"[red]{e}[/]")
        raise typer.Exit(2) from e


def _run_pipeline(cfg: Config, *, do_probe: bool, strict: bool) -> Pipeline:
    """Shared failure handling for every command that walks the ladder."""
    try:
        return asyncio.run(run_pipeline(cfg, do_probe=do_probe, strict=strict))
    except SsoInvalidError as e:
        stderr.print(f"[red]{e}[/]")
        raise typer.Exit(1) from e
    except RouterUnreachableError as e:
        stderr.print(f"[red]{e}[/]")
        raise typer.Exit(1) from e
    except CatalogError as e:
        stderr.print(f"[red]{e}[/]")
        raise typer.Exit(1) from e


def _check_circuit(pipeline: Pipeline) -> None:
    if pipeline.probes is not None and pipeline.probes.tripped:
        stderr.print(
            "[red]Circuit breaker tripped: too many consecutive probe failures. "
            "Nothing was written.[/]\n"
            "This is NOT auto-retried and did NOT run 'aws sso login'. "
            "Re-run manually once you've confirmed AWS/network health."
        )
        raise typer.Exit(1)


@app.command()
def fetch(
    catalog_only: Annotated[
        bool, typer.Option("--catalog-only", help="Only parse the pi catalog, skip AWS and the router.")
    ] = False,
    strict: Annotated[
        bool, typer.Option("--strict", help="Fail when the router is unreachable instead of degrading.")
    ] = False,
) -> None:
    """Refresh all sources: pi catalog, Bedrock entitlements per scanned region, router deployment."""
    cfg = _config()
    if catalog_only:
        try:
            catalog = load_catalog()
        except CatalogError as e:
            stderr.print(f"[red]{e}[/]")
            raise typer.Exit(1) from e
        table = Table(header_style="bold")
        for column in ("provider", "model", "context", "max-out", "thinking", "images"):
            table.add_column(column)
        for entry in catalog:
            table.add_row(entry.provider, entry.id, entry.context, entry.max_out, entry.thinking, entry.images)
        stdout.print(table)
        stdout.print(f"{len(catalog)} catalog row(s)")
        return

    pipeline = _run_pipeline(cfg, do_probe=False, strict=strict)
    stdout.print(summarize(pipeline))
    for region in cfg.regions:
        entitled = pipeline.entitlements.by_region.get(region)
        if entitled is None:
            continue
        n_cand = sum(1 for r in pipeline.bedrock_candidates.values() if r == region)
        stdout.print(f"  {region}: {len(entitled)} entitled, {n_cand} candidate(s) assigned to probe from here")
    if pipeline.router.reachable:
        stdout.print(f"  router: {len(pipeline.router.deployed)} deployed: {', '.join(pipeline.router.deployed)}")


@app.command()
def probe(
    timeout: Annotated[
        int, typer.Option(envvar="PROBE_TIMEOUT", help="Seconds before a probe is killed and counted systemic.")
    ] = 45,
    concurrency: Annotated[int, typer.Option(envvar="PROBE_CONCURRENCY", help="How many probes run at once.")] = 3,
    fail_circuit: Annotated[
        int,
        typer.Option(envvar="PROBE_FAIL_CIRCUIT", help="Consecutive systemic failures that trip the circuit breaker."),
    ] = 6,
) -> None:
    """Probe candidate models live through pi. Classified by output text, never by exit code."""
    cfg = _config()
    pipeline = _run_pipeline(cfg, do_probe=True, strict=False)
    assert pipeline.probes is not None
    for outcome in pipeline.probes.outcomes:
        region = f"{outcome.region} " if outcome.region else ""
        stderr.print(f"  {outcome.status.value}\t{outcome.id}\t{region}{outcome.reason}")
    ok = sum(1 for o in pipeline.probes.outcomes if o.status is ProbeStatus.OK)
    stdout.print(f"{ok}/{len(pipeline.probes.outcomes)} probes OK")
    _check_circuit(pipeline)


@app.command()
def report(
    as_json: Annotated[bool, typer.Option("--json", help="Emit the report as JSON for scripting.")] = False,
    no_probe: Annotated[
        bool, typer.Option("--no-probe", help="Skip probing (candidates only; faster, less accurate).")
    ] = False,
    strict: Annotated[
        bool, typer.Option("--strict", help="Fail when the router is unreachable instead of degrading.")
    ] = False,
) -> None:
    """Show the unified availability report across both providers, one flag per rung."""
    cfg = _config()
    pipeline = _run_pipeline(cfg, do_probe=not no_probe, strict=strict)
    _check_circuit(pipeline)
    rows = pipeline.rows()
    drift = pipeline.drift()
    generated_at = datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")
    if as_json:
        stdout.print(report_json(pipeline, rows, drift, generated_at))
        return
    stdout.print(summarize(pipeline))
    stdout.print(build_table(rows))
    for title, items, hint in drift_sections(drift):
        stdout.print(f"\n[bold]{title}[/] ({len(items)}) -- {hint}:")
        for item in items:
            stdout.print(f"  {item}")


@app.command()
def sync(
    dry_run: Annotated[bool, typer.Option("--dry-run", help="Probe and print, do not write any artifact.")] = False,
    no_probe: Annotated[
        bool, typer.Option("--no-probe", help="Skip probing (candidates only; faster, less accurate).")
    ] = False,
    strict: Annotated[
        bool, typer.Option("--strict", help="Fail when the router is unreachable instead of degrading.")
    ] = False,
) -> None:
    """Run the full pipeline: fetch, probe, report, then write artifacts."""
    cfg = _config()
    pipeline = _run_pipeline(cfg, do_probe=not no_probe, strict=strict)
    _check_circuit(pipeline)
    rows = pipeline.rows()
    drift = pipeline.drift()
    generated_at = datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")
    stdout.print(summarize(pipeline))
    stdout.print(build_table(rows))
    for title, items, hint in drift_sections(drift):
        stdout.print(f"\n[bold]{title}[/] ({len(items)}) -- {hint}:")
        for item in items:
            stdout.print(f"  {item}")

    if warning := settings_drift(cfg.pi_settings, cfg.live_settings):
        stderr.print(f"[yellow]settings drift: {warning}[/]")

    invocable = pipeline.invocable_bedrock()
    if not invocable:
        stderr.print("[red]No usable models after probing. Aborting (files unchanged).[/]")
        raise typer.Exit(1)

    if dry_run:
        stdout.print("\n(--dry-run: settings.json / bedrock-models.json / models.json not modified)")
        return

    # Preflight: render every artifact before writing any of them, so a bad
    # input (missing models.json, broken splice) never leaves a half-written run.
    new_bedrock = render_bedrock_models(invocable, cfg.default_region, generated_at)
    new_models: list[dict] = []
    new_models_text: str | None = None
    if pipeline.router.reachable:
        new_models = [entry.entry for entry in pipeline.router.entries.values()]
        new_models_text = render_models_json(cfg.models_json.read_text(), new_models)

    atomic_write(cfg.bedrock_models_json, new_bedrock)
    stdout.print(f"\nWrote {len(invocable)} model(s) to {cfg.bedrock_models_json}")

    if new_models_text is not None:
        atomic_write(cfg.models_json, new_models_text)
        stdout.print(f"Regenerated ai-model-router section in {cfg.models_json} ({len(new_models)} model(s))")
        quality: dict[str, dict[str, int]] = {}
        for entry in pipeline.router.entries.values():
            for field_name, source in entry.sources.items():
                quality.setdefault(field_name, {})[source] = quality.setdefault(field_name, {}).get(source, 0) + 1
        rendered = {f: ", ".join(f"{s} x{n}" for s, n in sorted(srcs.items())) for f, srcs in sorted(quality.items())}
        stdout.print("  metadata sources per field: " + "; ".join(f"{f}: {v}" for f, v in rendered.items()))
    else:
        stdout.print(f"Router unreachable: models.json left untouched ({pipeline.router.error})")

    # enabledModels is validated in place above (dead patterns / uncovered
    # models are in the drift sections) and never written. The dotfiles copy
    # of settings.json is the single store of curated patterns.
    stdout.print(f"enabledModels: {len(pipeline.patterns)} curated pattern(s) validated, file not modified")
