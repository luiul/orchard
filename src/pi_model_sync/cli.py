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

from typing import Annotated

import typer
from rich.console import Console

import pi_model_sync

APP_HELP = """\
Discover and sync the models [bold]pi[/] can actually use.

Covers both providers, [cyan]amazon-bedrock[/] and
[cyan]ai-model-router[/], across the whole availability ladder:
catalog → entitled/deployed → candidate → invocable → enabled.

Design and decisions: https://github.com/luiul/dotfiles/issues/27
"""

NOT_IMPLEMENTED = "not implemented yet, see https://github.com/luiul/dotfiles/issues/27"

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


@app.command()
def fetch(
    catalog_only: Annotated[
        bool, typer.Option("--catalog-only", help="Only parse the pi catalog, skip AWS and the router.")
    ] = False,
) -> None:
    """Refresh all sources: pi catalog, Bedrock entitlements per scanned region, router deployment."""
    stderr.print(f"fetch is {NOT_IMPLEMENTED}")
    raise typer.Exit(1)


@app.command()
def probe(
    timeout: Annotated[
        int, typer.Option(envvar="PROBE_TIMEOUT", help="Seconds before a probe is killed and counted systemic.")
    ] = 45,
    concurrency: Annotated[
        int, typer.Option(envvar="PROBE_CONCURRENCY", help="How many probes run at once.")
    ] = 3,
    fail_circuit: Annotated[
        int,
        typer.Option(envvar="PROBE_FAIL_CIRCUIT", help="Consecutive systemic failures that trip the circuit breaker."),
    ] = 6,
) -> None:
    """Probe candidate models live through pi. Classified by output text, never by exit code."""
    stderr.print(f"probe is {NOT_IMPLEMENTED}")
    raise typer.Exit(1)


@app.command()
def report(
    as_json: Annotated[bool, typer.Option("--json", help="Emit the report as JSON for scripting.")] = False,
) -> None:
    """Show the unified availability report across both providers, one flag per rung."""
    stderr.print(f"report is {NOT_IMPLEMENTED}")
    raise typer.Exit(1)


@app.command()
def sync(
    dry_run: Annotated[bool, typer.Option("--dry-run", help="Probe and print, do not write any artifact.")] = False,
    no_probe: Annotated[
        bool, typer.Option("--no-probe", help="Skip probing (candidates only; faster, less accurate).")
    ] = False,
    strict: Annotated[
        bool, typer.Option("--strict", help="Fail instead of marking router rungs unknown when the gateway is unreachable.")
    ] = False,
) -> None:
    """Run the full pipeline: fetch, probe, report, then write artifacts."""
    stderr.print(f"sync is {NOT_IMPLEMENTED}")
    raise typer.Exit(1)
