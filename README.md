# Orchard

**TLDR:** Orchard checks which models Pi can call through Amazon Bedrock and the HelloFresh AI Model Router. It discovers models in every configured Bedrock region, probes them through Pi, and reports what worked. Use Pi's `/model` command to choose a model.

Orchard is a small Go command for this Pi setup. It replaces the older Python `pi-model-sync` after the new path is verified. Development and earlier decisions are tracked in [the Orchard issue](https://github.com/luiul/orchard/issues/9).

## Why it exists

Pi knows many Bedrock model IDs, but a catalog entry does not prove that your AWS account can invoke one. A model may also work only in another region. The router's deployed list changes independently from Pi's configured router models. Orchard compares these sources and tests calls before writing a region map.

Pi already has a model picker. Orchard does not replace it or choose your default. Pi's picker can show models that Orchard has not verified, so check `orchard report` when availability matters.

## Concepts

These terms are independent facts, not steps in a ladder:

- **Catalog model:** Pi lists its ID in `pi --list-models`. This is not proof of access.
- **Listed model:** Bedrock lists a model or inference profile in a scanned AWS region. This is a discovery hint, not proof that an invocation works.
- **Deployed model:** The router lists it in `/v1/models`. It might not yet be configured in Pi.
- **Candidate:** An ID Orchard will probe. This includes discovered Bedrock catalog models, catalog models matched by the curated scope, previously mapped Bedrock models still in Pi's catalog, and deployed router models. A candidate may fail.
- **Invocable model:** A probe through Pi returned a usable response. For Bedrock, Orchard records the region that worked. Only a successful probe earns this label.
- **Scoped model:** Pi's own resolver includes it in the startup and cycling scope. Patterns in `enabledModels` can include a provider, a thinking pin, a case-insensitive glob, or a fuzzy model name. If the setting is absent or empty, or no patterns resolve, Pi cycles through all authenticated models. Scope does not prove a model works. Pi's `/model` can still show other models.
- **Unknown:** Discovery failed, probing did not run, or the outcome is uncertain. Do not treat unknown as a confirmed failure.

Orchard scans `eu-west-1`, `us-east-1`, `ap-northeast-1`, and `ap-southeast-2` by default. It does **not** filter results to EU or US. `jp.`, `apac.`, `au.`, global, and models without a region prefix can be probed. A model found in more than one region gets another try when its first probe fails. The curated Pi scope remains a human choice: adding a model to the region map does not add it to `enabledModels`.

## Use

Build from this checkout with `go run ./cmd/orchard <command>`, or install it with `make install` after reviewing the changes. The older `pi-model-sync` command is still a separate Python program until it is retired.

- `orchard fetch`: show catalog and provider discovery counts without model calls. Use `--catalog-only` to skip AWS and the router.
- `orchard probe`: test candidates without writing configuration. Each call can incur provider cost. Tune with `--timeout`, `--concurrency`, and `--fail-circuit`.
- `orchard report`: show model status and scope gaps without writing files. It probes by default. Use `--no-probe` for discovery only or `--json` for scripts.
- `orchard sync --dry-run`: probe and show proposed ID and region changes. It changes no files.
- `orchard sync`: probe, check the old map and all sources, then write configuration when the evidence is complete.

`--strict` makes `fetch` and `report` fail when the router is unreachable. `sync` always refuses incomplete discovery. It does not accept `--no-probe`.

Commands show discovery stages, probe starts, and completed results on stderr. `report --json` keeps stdout as one JSON document. If the circuit breaker stops probing, the report still includes completed results and unknown models, sets `meta.circuitTripped` to `true`, and exits with code 1. Stderr includes the failure reasons; Orchard does not log in or retry the batch automatically.

Once a model is configured, run `/model` inside Pi. Press `Ctrl+S` there to save the default for new sessions. Pi may update its live `~/.pi/agent/settings.json`; copy that file to `~/dotfiles/pi/.pi/agent/settings.json` to refresh the tracked snapshot. Orchard never edits either settings file.

## What sync changes

- `~/dotfiles/pi/.pi/agent/bedrock-models.json`: a `model ID → working region` map. The Pi `bedrock-region-sync.ts` extension and zsh `pi-use` commands read it.
- `~/dotfiles/pi/.pi/agent/models.json`: only the router's model definitions, generated from the live gateway. Other provider settings and formatting remain intact.

Orchard reads the curated `enabledModels` patterns from the dotfiles settings snapshot. For probe coverage, it also includes models selected by the live Pi scope. It reports verified models outside the snapshot scope, but it never changes either scope. If the live settings and snapshot differ, sync stops before probing. Every deployed router ID is probed against the generated definitions in an isolated temporary Pi configuration. Sync writes those definitions only when every router probe passes. If `models.json` or the scope changes during probing, sync stops rather than applying a different configuration.

Each file is replaced through a temporary file in its directory. The two files are **not** one transaction. If the second write fails, the first may already have changed. A sync refuses missing or malformed source data, skipped or systemic probes, and any previously mapped Bedrock model that it could not verify again. A failed run leaves the existing files alone unless a file write itself fails after another succeeded.

Orchard uses Pi's completed JSON assistant response to judge a probe. The provider and model must match the request. An empty, malformed, failed, or unfinished response is not verified. A successful response can contain words such as “error” without being treated as a failure. Probes turn off tools, extensions, project configuration, and context files. A short text probe verifies this invocation path, not every tool or image capability.

## Network access

Orchard does not open network connections itself. It runs the installed `aws` CLI for AWS identity and Bedrock discovery, `curl` for router listings and metadata, and Pi for model probes. These tools still need network access and their own LuLu rules. `go run` can create a new Orchard executable each time, but that executable no longer needs outbound permission.

The Pi catalog is read offline. Refresh it explicitly with `pi update --models` when needed. Live AWS and router discovery still needs connectivity; `fetch --catalog-only` skips those requests. Orchard does not change firewall rules or disable TLS verification.

## Configuration and credentials

`DOTFILES`, `PI_SETTINGS`, `BEDROCK_MODELS_JSON`, `MODELS_JSON`, and `MODEL_REGISTRY` override file paths. `AWS_PROFILE` defaults to `sso-bedrock`, `AWS_REGION` to `eu-west-1`, and `BEDROCK_REGIONS` replaces the extra regions to scan (set it to an empty string to scan only the default region). `PROBE_TIMEOUT`, `PROBE_CONCURRENCY`, and `PROBE_FAIL_CIRCUIT` control live probes. `ROUTER_BASE_URL` must match the router endpoint in `models.json`, and `AI_MODEL_ROUTER_API_KEY` supplies the gateway credential. The router key can also come from the gitignored `~/dotfiles/.env` file. Do not commit keys.

Orchard checks AWS identity, but never runs `aws sso login`. If that check fails, sign in yourself with `aws sso login --profile sso-bedrock`. An unreachable router or failed region scan is reported, not treated as an empty deployment. Router probes use a temporary config with no user extensions or stored credentials.

## Development

Use `make check` for format, build, vet, race tests, and golangci-lint. Tests use captured Pi and provider fixtures plus local fakes, not live settings or AWS writes. `aws` CLI v2, `curl`, Node, and Pi must be installed. Run `ORCHARD_PI_INTEGRATION=1 go test ./internal/catalog` to test scope matching against the installed Pi without model calls. The scope helper uses Pi's own implementation rather than copying its matching rules. It uses Pi 1.0.4's SDK and fails if that API is unavailable. See the [shared Go conventions](https://github.com/luiul/dashkit/blob/main/CONVENTIONS.md) for the repo family. Orchard keeps domain code in `internal/` and the CLI in `cmd/orchard/`.

The [legacy Python package](docs/pi-model-sync.md) stays in `src/` during the transition. Its report schema differs from the simplified Go report. Compare shared IDs, probe outcomes, and proposed changes before removing it.
