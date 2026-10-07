# pi-model-sync (legacy Python command)

**TLDR:** This is the previous implementation. Use the Go Orchard command from this checkout for the corrected discovery and probe flow. The Python package remains installed for transition checks, not as the recommended workflow.

## Commands

The Python command provides `fetch`, `probe`, `report`, and `sync`. It has no picker. Its report uses the older terms `entitled` and `enabled`; these mean provider discovery and pattern coverage, not proof that a model works. It probes only candidates known to the current Pi configuration, so newly deployed router IDs may not be tested until after a sync. Its `sync --no-probe` can publish unverified candidates. Do not use that flag as a verification step.

See the [Orchard README](../README.md) for current concepts, safety rules, and commands. The two report schemas and candidate sets differ deliberately. Compare shared IDs and outcomes, not whole JSON documents.

## Development

Run `uv run --group dev python -m pytest` for the Python tests. Keep this package until the Go flow has passed the local test suite and a complete live read-only probe. Do not use either implementation to overwrite live files during a parity check.
