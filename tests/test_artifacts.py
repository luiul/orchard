"""Artifact writers and pattern validation."""

import json

import pytest

from pi_model_sync.artifacts import (
    read_patterns,
    render_bedrock_models,
    settings_drift,
    splice_router_models,
    strip_pin,
    validate_patterns,
    write_bedrock_models,
    write_models_json,
)

DOTFILES_MODELS_JSON = __import__("pathlib").Path.home() / "dotfiles/pi/.pi/agent/models.json"


def test_strip_pin():
    assert strip_pin("gpt-5.6-luna:max") == "gpt-5.6-luna"
    assert strip_pin("moonshotai/Kimi-K3:high") == "moonshotai/Kimi-K3"
    assert strip_pin("claude-sonnet-5*") == "claude-sonnet-5*"
    assert strip_pin("model:withcolon") == "model:withcolon"  # not a level


def test_validate_patterns_dead_and_uncovered():
    patterns = ["claude-sonnet-5*", "dead-pattern-*", "exact:id"]
    matchable = {"claude-sonnet-5", "claude-sonnet-5-5", "exact:id", "uncovered-id"}
    dead, covered, uncovered = validate_patterns(patterns, matchable)
    assert dead == ["dead-pattern-*"]
    assert covered == {"claude-sonnet-5", "claude-sonnet-5-5", "exact:id"}
    assert uncovered == ["uncovered-id"]


def test_validate_patterns_pin_stripped_before_matching():
    dead, covered, _ = validate_patterns(["gpt-5.6-luna:max"], {"gpt-5.6-luna"})
    assert dead == []
    assert covered == {"gpt-5.6-luna"}


def test_render_bedrock_models_is_key_sorted_and_stable():
    text = render_bedrock_models({"b": "us-east-1", "a": "eu-west-1"}, "eu-west-1", "2026-10-01T00:00:00Z")
    doc = json.loads(text)
    assert doc == {
        "generatedAt": "2026-10-01T00:00:00Z",
        "defaultRegion": "eu-west-1",
        "models": {"a": "eu-west-1", "b": "us-east-1"},
    }
    assert list(json.loads(text)["models"].keys()) == ["a", "b"]
    # Unchanged inputs produce byte-identical output.
    assert text == render_bedrock_models({"a": "eu-west-1", "b": "us-east-1"}, "eu-west-1", "2026-10-01T00:00:00Z")
    assert text.endswith("\n")


def test_write_bedrock_models_is_atomic(tmp_path):
    path = tmp_path / "bedrock-models.json"
    write_bedrock_models(path, {"m": "eu-west-1"}, "eu-west-1", "2026-10-01T00:00:00Z")
    first = path.read_text()
    write_bedrock_models(path, {"m": "eu-west-1"}, "eu-west-1", "2026-10-01T00:00:00Z")
    assert path.read_text() == first
    assert not list(tmp_path.glob("*.tmp"))


def _sample_models_json() -> str:
    doc = {
        "providers": {
            "amazon-bedrock": {"modelOverrides": {"x.y": {"cost": {"input": 1}}}},
            "ai-model-router": {
                "baseUrl": "https://router.test/v1",
                "api": "openai-completions",
                "apiKey": "$AI_MODEL_ROUTER_API_KEY",
                "compat": {"supportsReasoningEffort": True},
                "models": [
                    {
                        "id": "old-model",
                        "name": "Old",
                        "reasoning": False,
                        "input": ["text"],
                        "cost": {"input": 1, "output": 2, "cacheRead": 0, "cacheWrite": 0},
                        "contextWindow": 100,
                        "maxTokens": 50,
                    }
                ],
            },
        }
    }
    return json.dumps(doc, indent=2) + "\n"


def _entry(model_id: str) -> dict:
    return {
        "id": model_id,
        "name": model_id.title(),
        "reasoning": True,
        "input": ["text", "image"],
        "cost": {"input": 1, "output": 2, "cacheRead": 0.1, "cacheWrite": 0.2},
        "contextWindow": 1000,
        "maxTokens": 100,
    }


def test_splice_preserves_everything_else_byte_for_byte():
    original = _sample_models_json()
    spliced = splice_router_models(original, [_entry("new-model")])
    doc = json.loads(spliced)
    assert [m["id"] for m in doc["providers"]["ai-model-router"]["models"]] == ["new-model"]
    provider = doc["providers"]["ai-model-router"]
    assert provider["baseUrl"] == "https://router.test/v1"
    assert provider["apiKey"] == "$AI_MODEL_ROUTER_API_KEY"
    # Everything outside the models array is byte-identical.
    before = original[: original.index('"models"')]
    assert spliced.startswith(before)


def test_splice_removes_exactly_the_undeployed_entry():
    original = _sample_models_json()
    spliced = splice_router_models(original, [])  # gateway stopped deploying everything
    assert json.loads(spliced)["providers"]["ai-model-router"]["models"] == []


def test_splice_input_arrays_stay_inline():
    spliced = splice_router_models(_sample_models_json(), [_entry("m")])
    assert '"input": ["text", "image"]' in spliced


def test_splice_round_trip_is_stable():
    """Re-running with the same model set yields a byte-identical file."""
    once = splice_router_models(_sample_models_json(), [_entry("b"), _entry("a")])
    twice = splice_router_models(once, [_entry("b"), _entry("a")])
    assert once == twice


def test_write_models_json_sorts_by_id(tmp_path):
    path = tmp_path / "models.json"
    path.write_text(_sample_models_json())
    write_models_json(path, [_entry("zeta"), _entry("alpha")])
    ids = [m["id"] for m in json.loads(path.read_text())["providers"]["ai-model-router"]["models"]]
    assert ids == ["alpha", "zeta"]


@pytest.mark.skipif(not DOTFILES_MODELS_JSON.is_file(), reason="dotfiles checkout not present")
def test_splice_real_models_json_preserves_other_sections():
    """The real dotfiles models.json, re-spliced with its own models, keeps
    baseUrl/api/apiKey/compat and the whole amazon-bedrock section."""
    original = DOTFILES_MODELS_JSON.read_text()
    doc = json.loads(original)
    current = doc["providers"]["ai-model-router"]["models"]
    spliced = json.loads(splice_router_models(original, current))
    assert spliced["providers"]["amazon-bedrock"] == doc["providers"]["amazon-bedrock"]
    router = spliced["providers"]["ai-model-router"]
    assert router["baseUrl"] == doc["providers"]["ai-model-router"]["baseUrl"]
    assert router["compat"] == doc["providers"]["ai-model-router"]["compat"]


def test_settings_drift(tmp_path):
    dotfiles = tmp_path / "dot.json"
    live = tmp_path / "live.json"
    dotfiles.write_text(json.dumps({"enabledModels": ["a", "b"]}))
    live.write_text(json.dumps({"enabledModels": ["a", "b"]}))
    assert settings_drift(dotfiles, live) is None
    live.write_text(json.dumps({"enabledModels": ["a"]}))
    assert settings_drift(dotfiles, live) is not None


def test_read_patterns(tmp_path):
    path = tmp_path / "settings.json"
    path.write_text(json.dumps({"enabledModels": ["a*", 42, "b*"]}))
    assert read_patterns(path) == ["a*", "b*"]
