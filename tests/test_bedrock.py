"""Bedrock entitlement fetcher against captured AWS API fixtures."""

import pytest

from pi_model_sync.bedrock import (
    SsoInvalidError,
    _inference_profile_ids,
    _on_demand_model_ids,
    assign_candidates,
    check_sso,
)
from pi_model_sync.paths import Config

REGIONS = ("eu-west-1", "us-east-1", "ap-northeast-1", "ap-southeast-2")


class _FakePaginator:
    def __init__(self, pages):
        self.pages = pages

    def paginate(self):
        return self.pages


class _FakeBedrockClient:
    """Serves captured list-inference-profiles / list-foundation-models responses."""

    def __init__(self, profiles: dict, foundations: dict):
        self._profiles = profiles
        self._foundations = foundations

    def get_paginator(self, operation: str):
        assert operation == "list_inference_profiles"
        return _FakePaginator([self._profiles])

    def list_foundation_models(self):
        return self._foundations


def test_inference_profiles_from_fixture(bedrock_fixture):
    profiles = bedrock_fixture["load"]("eu-west-1", "inference-profiles")
    ids = _inference_profile_ids(_FakeBedrockClient(profiles, {}))
    assert "eu.anthropic.claude-sonnet-5" in ids or any(i.startswith("eu.anthropic.") for i in ids)
    assert len(ids) == len(profiles["inferenceProfileSummaries"])


def test_on_demand_from_fixture_filters_inference_types(bedrock_fixture):
    foundations = bedrock_fixture["load"]("us-east-1", "foundation-models")
    ids = _on_demand_model_ids(_FakeBedrockClient({}, foundations))
    assert ids  # us-east-1 has ON_DEMAND models
    for summary in foundations["modelSummaries"]:
        if "ON_DEMAND" not in (summary.get("inferenceTypesSupported") or []):
            assert summary["modelId"] not in ids


def test_every_prefix_group_is_represented_in_fixtures(bedrock_fixture):
    """The jp/au/apac models the old script could never offer must show up."""
    all_ids: set[str] = set()
    for region in REGIONS:
        profiles = bedrock_fixture["load"](region, "inference-profiles")
        all_ids |= _inference_profile_ids(_FakeBedrockClient(profiles, {}))
    prefixes = {i.split(".")[0] for i in all_ids if "." in i}
    assert {"eu", "us", "global", "jp", "au", "apac"} <= prefixes


def _cfg(**overrides) -> Config:
    env = {
        "AWS_PROFILE": "sso-bedrock",
        "AWS_REGION": "eu-west-1",
        "BEDROCK_REGIONS": "us-east-1 ap-northeast-1 ap-southeast-2",
        "DOTFILES": "/nonexistent-dotfiles",
    }
    env.update(overrides)
    return Config.from_env(env)


def test_assign_candidates_prefers_default_region():
    entitled = {"eu-west-1": {"a", "b"}, "us-east-1": {"a", "c"}}
    assigned = assign_candidates(entitled, {"a", "b", "c"}, ("eu-west-1", "us-east-1"), "eu-west-1", {})
    assert assigned == {"a": "eu-west-1", "b": "eu-west-1", "c": "us-east-1"}


def test_assign_candidates_uses_prefix_home_region():
    entitled = {"us-east-1": {"us.anthropic.x"}, "ap-northeast-1": {"jp.anthropic.y"}}
    assigned = assign_candidates(
        entitled, {"us.anthropic.x", "jp.anthropic.y"}, ("eu-west-1", "us-east-1", "ap-northeast-1"), "eu-west-1", {}
    )
    assert assigned["us.anthropic.x"] == "us-east-1"
    assert assigned["jp.anthropic.y"] == "ap-northeast-1"


def test_assign_candidates_registry_override_wins():
    entitled = {"eu-west-1": {"global.openai.gpt-5.6-sol"}, "us-east-1": {"global.openai.gpt-5.6-sol"}}
    assigned = assign_candidates(
        entitled,
        {"global.openai.gpt-5.6-sol"},
        ("eu-west-1", "us-east-1"),
        "eu-west-1",
        {"global.openai.gpt-5.6-sol": "us-east-1"},
    )
    assert assigned["global.openai.gpt-5.6-sol"] == "us-east-1"


def test_assign_candidates_intersects_with_catalog():
    entitled = {"eu-west-1": {"known", "unknown-to-pi"}}
    assigned = assign_candidates(entitled, {"known"}, ("eu-west-1",), "eu-west-1", {})
    assert assigned == {"known": "eu-west-1"}


def test_sso_failure_prints_fix_command(monkeypatch):
    import boto3

    class _Boom:
        def client(self, *_args, **_kwargs):
            raise RuntimeError("token expired")

    monkeypatch.setattr(boto3, "Session", lambda **_: _Boom())
    cfg = _cfg()
    with pytest.raises(SsoInvalidError, match="aws sso login --profile sso-bedrock"):
        check_sso(cfg)


def test_sso_check_runs_once(monkeypatch):
    import boto3

    calls = []

    class _Session:
        def client(self, service, **_kwargs):
            calls.append(service)

            class _Sts:
                def get_caller_identity(self):
                    return {}

            return _Sts()

    monkeypatch.setattr(boto3, "Session", lambda **_: _Session())
    check_sso(_cfg())
    assert calls == ["sts"]
