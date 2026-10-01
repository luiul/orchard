"""Rung 2 for ai-model-router: what the gateway deploys right now, plus the
metadata needed to generate pi's `models.json` router entries.

Live ground truth is `GET /v1/models` (deployed ids) and
`GET /v1/model/info` (cost/context metadata per model, sometimes null).
Auth: Bearer `$AI_MODEL_ROUTER_API_KEY` (loaded from gitignored
`~/dotfiles/.env` when unset, see paths.py). An unreachable gateway (no VPN)
is not an error: every router rung becomes "unknown" and the Bedrock side
continues. `--strict` turns that into a failure.

Metadata chain per field, per issue decision 2:

1. the hand-maintained model registry, stowed in ~/dotfiles
   (`pi/.pi/agent/model-registry.json`, key `routerModelOverrides`): human
   authority. It fills nulls AND corrects values that are present but wrong
   (example: the gateway advertises maxTokens 384000 for a model whose
   backend rejects anything over 262144).
2. live router metadata (`/v1/model/info`, then `/v1/models`)
3. pi's bundled provider data (the per-provider JSON pi-ai ships)

Every generated field records which source it came from, so the report can
show metadata quality.
"""

from __future__ import annotations

import json
import os
import shutil
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import httpx

from pi_model_sync.paths import Config

REQUEST_TIMEOUT = 10.0

FIELD_LABELS = ("name", "reasoning", "input", "cost", "contextWindow", "maxTokens")


def load_registry(path: Path) -> dict[str, Any]:
    """The hand-maintained model registry stowed in ~/dotfiles.

    Missing file is fine (empty registry); invalid JSON is a hard error —
    a broken hand-maintained file should be fixed, not silently ignored.
    """
    if not path.is_file():
        return {"probeRegionOverrides": {}, "routerModelOverrides": {}}
    data = json.loads(path.read_text())
    return {
        "probeRegionOverrides": data.get("probeRegionOverrides", {}),
        "routerModelOverrides": data.get("routerModelOverrides", {}),
    }


def _normalize_model_id(model_id: str) -> str:
    """Lowercase, strip org prefix: `zai-org/GLM-5.3-Flash` -> `glm-5.3-flash`."""
    return model_id.rsplit("/", 1)[-1].lower()


def find_bundled_data_dir() -> Path | None:
    """Locate pi-ai's bundled per-provider model data inside the pi install.

    `pi` resolves to the pi-coding-agent package; pi-ai sits in its
    node_modules. Override with `PI_PROVIDER_DATA_DIR` (used by tests).
    """
    if override := os.environ.get("PI_PROVIDER_DATA_DIR"):
        path = Path(override).expanduser()
        return path if path.is_dir() else None
    pi_bin = shutil.which("pi")
    if not pi_bin:
        return None
    pkg_root = Path(pi_bin).resolve().parent
    for parent in (pkg_root, *pkg_root.parents):
        candidate = parent / "node_modules" / "@earendil-works" / "pi-ai" / "dist" / "providers" / "data"
        if candidate.is_dir():
            return candidate
    return None


def load_bundled_models(data_dir: Path | None) -> dict[str, dict[str, Any]]:
    """Index pi's bundled provider data by normalized model id."""
    if data_dir is None:
        return {}
    index: dict[str, dict[str, Any]] = {}
    for file in sorted(data_dir.glob("*.json")):
        if file.name.startswith("."):
            continue
        try:
            data = json.loads(file.read_text())
        except (OSError, json.JSONDecodeError):
            continue
        for group in data.values():
            if not isinstance(group, dict):
                continue
            for entry in group.values():
                if isinstance(entry, dict) and isinstance(entry.get("id"), str):
                    index.setdefault(_normalize_model_id(entry["id"]), entry)
    return index


def _per_million(per_token: Any) -> int | float | None:
    """litellm reports cost per token; pi wants cost per million tokens."""
    if not isinstance(per_token, (int, float)):
        return None
    value = round(per_token * 1_000_000, 6)
    return int(value) if value == int(value) else value


def _humanize(model_id: str) -> str:
    """`deepseek-ai/DeepSeek-V4.1-Flash` -> `Deepseek V4 1 Flash`."""
    return _normalize_model_id(model_id).replace("-", " ").replace(".", " ").title()


@dataclass
class RouterModelEntry:
    """A generated `models.json` router entry plus per-field provenance."""

    id: str
    entry: dict[str, Any]
    sources: dict[str, str]  # field -> "router" | "bundled" | "override" | "default"


@dataclass
class RouterDeployment:
    reachable: bool
    deployed: list[str] = field(default_factory=list)
    entries: dict[str, RouterModelEntry] = field(default_factory=dict)
    error: str | None = None


def _build_entry(
    model_id: str,
    models_item: dict[str, Any],
    info_item: dict[str, Any] | None,
    bundled: dict[str, dict[str, Any]],
    overrides: dict[str, Any],
) -> RouterModelEntry:
    """Assemble one pi router entry, walking the fallback chain per field."""
    model_info = (info_item or {}).get("model_info") or {}
    params = (info_item or {}).get("litellm_params") or {}
    bundled_entry = bundled.get(_normalize_model_id(model_id)) or {}
    override = overrides.get(model_id) or overrides.get(_normalize_model_id(model_id)) or {}

    # A field is only as trustworthy as its weakest source, so the reported
    # source for a multi-part field (cost) is the lowest-priority one used.
    source_rank = {"router": 0, "bundled": 1, "override": 2, "default": 3}

    def pick(field_name: str, *candidates: tuple[str, Any]) -> Any:
        for source, value in candidates:
            if value is not None:
                sources[field_name] = source
                return value
        sources[field_name] = "default"
        return None

    sources: dict[str, str] = {}

    cost: dict[str, Any] = {}
    cost_sources: list[str] = []
    for key, router_value in {
        "input": _per_million(model_info.get("input_cost_per_token") or params.get("input_cost_per_token")),
        "output": _per_million(model_info.get("output_cost_per_token") or params.get("output_cost_per_token")),
        "cacheRead": _per_million(model_info.get("cache_read_input_token_cost")),
        "cacheWrite": _per_million(model_info.get("cache_creation_input_token_cost")),
    }.items():
        override_cost = (override.get("cost") or {}).get(key)
        bundled_cost = (bundled_entry.get("cost") or {}).get(key)
        if override_cost is not None:
            cost[key] = override_cost
            cost_sources.append("override")
        elif router_value is not None:
            cost[key] = router_value
            cost_sources.append("router")
        elif bundled_cost is not None:
            cost[key] = bundled_cost
            cost_sources.append("bundled")
        else:
            cost[key] = 0
            cost_sources.append("default")
    sources["cost"] = min(cost_sources, key=source_rank.__getitem__)

    context_window = model_info.get("max_input_tokens") or models_item.get("max_input_tokens")
    max_tokens = model_info.get("max_output_tokens") or models_item.get("max_output_tokens")
    vision = model_info.get("supports_vision")
    router_input = ["text", "image"] if vision else (["text"] if vision is False else None)

    name = pick("name", ("override", override.get("name")), ("bundled", bundled_entry.get("name")))
    if name is None:
        name = _humanize(model_id)

    entry = {
        "id": model_id,
        "name": name,
        "reasoning": bool(
            pick(
                "reasoning",
                ("override", override.get("reasoning")),
                ("router", model_info.get("supports_reasoning")),
                ("bundled", bundled_entry.get("reasoning")),
            )
            or False
        ),
        "input": pick(
            "input",
            ("override", override.get("input")),
            ("router", router_input),
            ("bundled", bundled_entry.get("input")),
        )
        or ["text"],
        "cost": cost,
        "contextWindow": pick(
            "contextWindow",
            ("override", override.get("contextWindow")),
            ("router", context_window),
            ("bundled", bundled_entry.get("contextWindow")),
        )
        or 0,
        "maxTokens": pick(
            "maxTokens",
            ("override", override.get("maxTokens")),
            ("router", max_tokens),
            ("bundled", bundled_entry.get("maxTokens")),
        )
        or 0,
    }
    return RouterModelEntry(id=model_id, entry=entry, sources=sources)


def fetch_router(cfg: Config, *, client: httpx.Client | None = None) -> RouterDeployment:
    """Fetch the live deployment and generate entries for every deployed id.

    Never raises on network/auth failures: returns `reachable=False` with the
    error recorded. The caller decides (strict mode) whether that is fatal.
    """
    if not cfg.router_api_key:
        return RouterDeployment(reachable=False, error="AI_MODEL_ROUTER_API_KEY not set (env or ~/dotfiles/.env)")

    owns_client = client is None
    client = client or httpx.Client(
        base_url=cfg.router_base_url,
        headers={"Authorization": f"Bearer {cfg.router_api_key}"},
        timeout=REQUEST_TIMEOUT,
    )
    try:
        models_resp = client.get("/models")
        info_resp = client.get("/model/info")
        models_resp.raise_for_status()
        info_resp.raise_for_status()
        models_data = models_resp.json().get("data", [])
        info_data = info_resp.json().get("data", [])
    except Exception as e:
        return RouterDeployment(reachable=False, error=f"{type(e).__name__}: {e}")
    finally:
        if owns_client:
            client.close()

    info_by_name = {item.get("model_name"): item for item in info_data if isinstance(item, dict)}
    models_by_id = {item.get("id"): item for item in models_data if isinstance(item, dict)}
    bundled = load_bundled_models(find_bundled_data_dir())
    overrides = load_registry(cfg.model_registry)["routerModelOverrides"]

    deployed = sorted(models_by_id)
    entries = {
        model_id: _build_entry(model_id, models_by_id[model_id], info_by_name.get(model_id), bundled, overrides)
        for model_id in deployed
    }
    return RouterDeployment(reachable=True, deployed=deployed, entries=entries)
