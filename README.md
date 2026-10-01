# pi-model-sync

Discover and sync the models [pi](https://github.com/earendil-works/pi) can actually use, across both providers: `amazon-bedrock` and `ai-model-router`.

"Available model" is ambiguous, so the tool walks a six-rung ladder for every model: catalog (pi knows the id), entitled (the AWS account may call it, per region) or deployed (the router serves it right now), candidate (worth probing), invocable (survives a live probe through pi), and enabled (offered in `/model` and Ctrl+P). Each rung is a subset of the one above, and only a live probe is reliable proof of invocability.

From that it produces one unified report across both providers and writes the three artifacts that configure pi: `enabledModels` in `settings.json` (validated, never auto-edited), `bedrock-models.json` (which region each invocable Bedrock model needs), and the router section of `models.json` (generated from the live gateway).

## Install

```sh
uv tool install ~/projects/personal/pi-model-sync
```

Or run without installing:

```sh
uv run --project ~/projects/personal/pi-model-sync pi-model-sync --help
```

## Commands

- `pi-model-sync fetch`: refresh all sources (pi catalog, Bedrock entitlements per scanned region, router deployment).
- `pi-model-sync probe`: probe candidates live through pi, with a circuit breaker for systemic failures.
- `pi-model-sync report`: the unified availability table, plus drift sections (`--json` for scripting).
- `pi-model-sync sync`: the full pipeline, then write artifacts. Flags: `--dry-run`, `--no-probe`, `--strict`.

## Development

```sh
uv sync
uv run pytest
uv run ruff check
uv run ty check
```

## Links

- Design, decisions, and implementation plan: https://github.com/luiul/dotfiles/issues/27
- Vocabulary (the availability ladder and all terms): `docs/model-availability-vocabulary.md` in https://github.com/luiul/dotfiles
- Replaces the bash script `pi/.pi/agent/bin/sync-enabled-models.sh` once it reaches parity.
