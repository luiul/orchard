"""Router deployment fetcher against captured live gateway fixtures."""

import json

import httpx
import pytest

from pi_model_sync.paths import Config
from pi_model_sync.router import (
    _build_entry,
    _normalize_model_id,
    _per_million,
    fetch_router,
    load_registry,
)


def _cfg(tmp_path, **env) -> Config:
    base = {"DOTFILES": str(tmp_path), "AI_MODEL_ROUTER_API_KEY": "test-key"}
    base.update(env)
    return Config.from_env(base)


def _mock_client(router_models, router_model_info) -> httpx.Client:
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/models":
            return httpx.Response(200, json=router_models)
        if request.url.path == "/v1/model/info":
            return httpx.Response(200, json=router_model_info)
        return httpx.Response(404)

    return httpx.Client(transport=httpx.MockTransport(handler), base_url="https://router.test/v1")


def test_live_fixture_shape(router_models):
    ids = sorted(item["id"] for item in router_models["data"])
    assert len(ids) == 13
    assert "claude-opus-5-5" in ids  # deployed but unknown to pi when captured


def test_fetch_deployed_ids_and_entries(tmp_path, router_models, router_model_info, monkeypatch):
    monkeypatch.setenv("PI_PROVIDER_DATA_DIR", str(tmp_path / "no-such-dir"))  # no bundled data
    cfg = _cfg(tmp_path)
    client = _mock_client(router_models, router_model_info)
    deployment = fetch_router(cfg, client=client)
    assert deployment.reachable
    assert len(deployment.deployed) == 13
    entry = deployment.entries["claude-sonnet-5"].entry
    assert entry["id"] == "claude-sonnet-5"
    assert entry["cost"] == {"input": 2, "output": 10, "cacheRead": 0.2, "cacheWrite": 2.5}
    assert entry["contextWindow"] == 1_000_000
    assert entry["maxTokens"] == 128_000
    assert entry["reasoning"] is True
    assert entry["input"] == ["text", "image"]
    assert deployment.entries["claude-sonnet-5"].sources["cost"] == "router"


def test_unreachable_gateway_degrades_without_raising(tmp_path):
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("no route to host", request=request)

    cfg = _cfg(tmp_path)
    client = httpx.Client(transport=httpx.MockTransport(handler), base_url="https://router.test/v1")
    deployment = fetch_router(cfg, client=client)
    assert not deployment.reachable
    assert "ConnectError" in (deployment.error or "")
    assert deployment.deployed == []


def test_missing_api_key_is_unreachable(tmp_path):
    cfg = Config.from_env({"DOTFILES": str(tmp_path)})  # no key, no .env fallback
    deployment = fetch_router(cfg)
    assert not deployment.reachable
    assert "AI_MODEL_ROUTER_API_KEY" in (deployment.error or "")


def test_fallback_chain_null_metadata(tmp_path):
    """A model with null router metadata falls back to bundled data, then overrides."""
    bundled = {
        "gpt-6-luna": {
            "id": "gpt-6-luna",
            "name": "GPT-6 Luna",
            "reasoning": True,
            "input": ["text", "image"],
            "cost": {"input": 0.1, "output": 0.2, "cacheRead": 0.02, "cacheWrite": 0.25},
            "contextWindow": 1_000_000,
            "maxTokens": 128_000,
        }
    }
    entry = _build_entry("gpt-6-luna", {}, None, bundled, {})
    assert entry.entry["name"] == "GPT-6 Luna"
    assert entry.entry["cost"]["input"] == 0.1
    assert entry.entry["reasoning"] is True
    assert all(source == "bundled" for source in entry.sources.values())

    overrides = {"gpt-6-luna": {"name": "Luna Six", "cost": {"input": 9}}}
    entry = _build_entry("gpt-6-luna", {}, None, {}, overrides)
    assert entry.entry["name"] == "Luna Six"
    assert entry.sources["name"] == "override"
    assert entry.entry["cost"]["input"] == 9
    assert entry.sources["cost"] == "override"
    assert entry.entry["cost"]["output"] == 0  # nothing anywhere -> default 0

    entry = _build_entry("mystery-model", {}, None, {}, {})
    assert entry.entry["input"] == ["text"]
    assert entry.entry["reasoning"] is False
    assert set(entry.sources.values()) == {"default"}


def test_override_wins_over_present_but_wrong_router_data(tmp_path):
    """The registry is human authority: it corrects live values that break the
    backend, not just nulls (the DeepSeek maxTokens 384000 -> 262144 case)."""
    info_item = {
        "model_name": "deepseek-ai/DeepSeek-V4-Flash-0731",
        "model_info": {"max_output_tokens": 384000, "max_input_tokens": 1048576, "supports_reasoning": True},
        "litellm_params": {},
    }
    overrides = {"deepseek-ai/DeepSeek-V4-Flash-0731": {"maxTokens": 262144}}
    entry = _build_entry("deepseek-ai/DeepSeek-V4-Flash-0731", {}, info_item, {}, overrides)
    assert entry.entry["maxTokens"] == 262144
    assert entry.sources["maxTokens"] == "override"
    assert entry.entry["contextWindow"] == 1048576  # router data still used where no override exists
    assert entry.sources["contextWindow"] == "router"


def test_registry_overrides_match_normalized_ids(tmp_path):
    overrides = {"glm-5.3-flash": {"name": "GLM 5.3 Flash"}}
    entry = _build_entry("zai-org/GLM-5.3-Flash", {}, None, {}, overrides)
    assert entry.entry["name"] == "GLM 5.3 Flash"
    assert entry.sources["name"] == "override"


def test_per_million_conversion():
    assert _per_million(0.000002) == 2
    assert _per_million(2e-7) == 0.2
    assert _per_million(None) is None
    assert _per_million("nope") is None


def test_normalize_model_id():
    assert _normalize_model_id("zai-org/GLM-5.3-Flash") == "glm-5.3-flash"
    assert _normalize_model_id("claude-opus-5-5") == "claude-opus-5-5"


def test_load_registry_missing_file(tmp_path):
    registry = load_registry(tmp_path / "model-registry.json")
    assert registry == {"probeRegionOverrides": {}, "routerModelOverrides": {}}


def test_load_registry_reads_overrides(tmp_path):
    path = tmp_path / "model-registry.json"
    path.write_text(json.dumps({"probeRegionOverrides": {"a": "b"}, "routerModelOverrides": {"m": {"name": "M"}}}))
    registry = load_registry(path)
    assert registry["probeRegionOverrides"] == {"a": "b"}
    assert registry["routerModelOverrides"]["m"]["name"] == "M"


def test_load_registry_invalid_json_is_a_hard_error(tmp_path):
    path = tmp_path / "model-registry.json"
    path.write_text("{ not json")
    with pytest.raises(json.JSONDecodeError):
        load_registry(path)
