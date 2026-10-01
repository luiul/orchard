# pi-model-sync

**TLDR:** pi-model-sync finds the models your [pi](https://github.com/earendil-works/pi) setup can actually use right now, across both providers (`amazon-bedrock` and `ai-model-router`), and rewrites pi's model config to match reality.

Status: early development. The commands below are the agreed interface; the pipeline behind them is being built. Track progress in the [issues](https://github.com/luiul/pi-model-sync/issues); design and decisions live in the [dotfiles epic](https://github.com/luiul/dotfiles/issues/27).

## Why pi-model-sync

pi's model picker drifts away from reality:

- AWS Bedrock entitlements differ per region and change over time. pi ships a static catalog (~180 ids), but only a fraction of it works in your account.
- The ai-model-router's deployed set changes without notice, while pi's router entries are a hand-written list. When the two drift apart, the picker offers models that no longer exist and hides models that do.

The result: you pick a model and the call fails, while working models stay invisible.

pi-model-sync fixes this by measuring instead of assuming. It fetches what each provider actually offers, probes every candidate live through pi, and reports the result in one table.

Once it reaches parity, pi-model-sync replaces the bash script `pi/.pi/agent/bin/sync-enabled-models.sh` in https://github.com/luiul/dotfiles.

## Use cases

- **"What can I use right now?"** Run `pi-model-sync report`: one table across both providers, with a flag per availability level and a drift section.
- **A new model appears on the router.** `pi-model-sync sync` regenerates pi's `models.json` from the live gateway. No hand-editing, nothing missed.
- **A model is retired or renamed.** The report flags `enabledModels` patterns that no longer match anything.
- **A Bedrock model only works outside your default region.** `bedrock-models.json` records the working region per model, which the `bedrock-region-sync` pi extension and the `pi-use` / `pi-region` zsh helpers consume.

## Concepts

**Two providers.** `amazon-bedrock` is pi's built-in provider (AWS SSO auth, one active region per process). `ai-model-router` is an internal LiteLLM gateway (API key auth, no regions, the gateway routes to backends itself).

**The availability ladder.** "Available" has five meanings here, each a subset of the previous one:

1. **Catalog**: pi knows the id (source: `pi --list-models`).
2. **Entitled** (Bedrock) / **deployed** (router): the account may call it in a region, or the gateway serves it right now.
3. **Candidate**: in the catalog and entitled/deployed, so worth probing.
4. **Invocable**: answers a live probe through pi. The only reliable proof; entitlement alone cannot predict per-model failures.
5. **Enabled**: offered in `/model` and Ctrl+P, via `enabledModels` in `settings.json`.

**Three artifacts.** `sync` writes `enabledModels` in `settings.json` (validated in place, never auto-edited), `bedrock-models.json` (model to region map), and the router section of `models.json` (generated from the live gateway).

**Safety rules.** The tool never runs `aws sso login` (it checks once, prints the fix, and exits). Probes are classified by output text, never by exit code (pi exits 0 even when a model call fails). A circuit breaker aborts the run on repeated systemic failures instead of hammering a broken account.

The full vocabulary, with every term mapped to a concrete file, command, or API, lives in `docs/model-availability-vocabulary.md` in https://github.com/luiul/dotfiles.

## Installation

```sh
uv tool install ~/projects/personal/pi-model-sync
```

Or run without installing:

```sh
uv run --project ~/projects/personal/pi-model-sync pi-model-sync --help
```

## Usage

- `pi-model-sync fetch`: refresh all sources (pi catalog, Bedrock entitlements per scanned region, router deployment).
- `pi-model-sync probe`: probe candidates live through pi, with a circuit breaker for systemic failures.
- `pi-model-sync report`: the unified availability table plus drift sections (`--json` for scripting).
- `pi-model-sync sync`: the full pipeline, then write artifacts. Flags: `--dry-run`, `--no-probe`, `--strict`.

## Development

```sh
uv sync
uv run pytest
uv run ruff check
uv run ty check
```

## License

MIT, see [LICENSE](LICENSE).
