"""Rung 2 for amazon-bedrock: every model this AWS account may call, per region.

Two disjoint sources per region, because Bedrock has two invocation modes:

- `ListInferenceProfiles`: profile ids for INFERENCE_PROFILE-only models (all
  current Claude). The id prefix shows the region group: `eu.` -> eu-west-1,
  `us.` -> us-east-1, `jp.`/`apac.` -> ap-northeast-1, `au.` -> ap-southeast-2,
  `global.` -> any scanned region.
- `ListFoundationModels` filtered to ON_DEMAND: bare ids (`qwen.*`,
  `mistral.*`, ...), present only in regions where the model is deployed.
  These ids never appear in list-inference-profiles.

Auth contract, ported verbatim from the bash script: NEVER run
`aws sso login`. SSO validity is checked exactly once per run
(`sts:GetCallerIdentity`); on failure the fix command is printed for a human
and the run exits non-zero.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import boto3

from pi_model_sync.paths import Config

# Inference-profile id prefix -> the region group it is invocable from.
PREFIX_HOME_REGION = {
    "eu.": "eu-west-1",
    "us.": "us-east-1",
    "jp.": "ap-northeast-1",
    "apac.": "ap-northeast-1",
    "au.": "ap-southeast-2",
    # "global." is special: the default region.
}


class SsoInvalidError(Exception):
    """SSO session invalid. The message carries the exact fix command."""


@dataclass
class Entitlements:
    """Entitled model ids per scanned region, plus per-region failures."""

    by_region: dict[str, set[str]] = field(default_factory=dict)
    degraded: dict[str, str] = field(default_factory=dict)  # region -> error summary

    @property
    def all_ids(self) -> set[str]:
        return set().union(*self.by_region.values()) if self.by_region else set()


def check_sso(cfg: Config) -> None:
    """The single SSO validity check for the whole run. Never logs in."""
    try:
        boto3.Session(profile_name=cfg.aws_profile).client("sts").get_caller_identity()
    except Exception as e:  # any failure here means: human must log in
        raise SsoInvalidError(
            f"AWS SSO session invalid for '{cfg.aws_profile}'. "
            f"Run this yourself: aws sso login --profile {cfg.aws_profile}\n({type(e).__name__}: {e})"
        ) from e


def _inference_profile_ids(client: Any) -> set[str]:
    ids: set[str] = set()
    paginator = client.get_paginator("list_inference_profiles")
    for page in paginator.paginate():
        for summary in page.get("inferenceProfileSummaries", []):
            if profile_id := summary.get("inferenceProfileId"):
                ids.add(profile_id)
    return ids


def _on_demand_model_ids(client: Any) -> set[str]:
    # ListFoundationModels has no paginator in botocore; one call returns all.
    response = client.list_foundation_models()
    ids: set[str] = set()
    for summary in response.get("modelSummaries", []):
        if "ON_DEMAND" in (summary.get("inferenceTypesSupported") or []) and (model_id := summary.get("modelId")):
            ids.add(model_id)
    return ids


def fetch_entitlements(cfg: Config, session: Any = None) -> Entitlements:
    """Entitled ids per scanned region. A failing region is degraded, not fatal."""
    session = session or boto3.Session(profile_name=cfg.aws_profile)
    result = Entitlements()
    for region in cfg.regions:
        try:
            client = session.client("bedrock", region_name=region)
            result.by_region[region] = _inference_profile_ids(client) | _on_demand_model_ids(client)
        except Exception as e:  # region down/throttled: degrade, report, continue
            result.degraded[region] = f"{type(e).__name__}: {e}"
    return result


def assign_candidates(
    entitled_by_region: dict[str, set[str]],
    catalog_ids: set[str],
    regions: tuple[str, ...],
    default_region: str,
    region_overrides: dict[str, str],
) -> dict[str, str]:
    """Intersect entitled ids with the catalog per region, then assign each
    candidate id exactly ONE probe region.

    Preference order, ported from the bash script: explicit registry override,
    then the default region (if the id is a candidate there), then the id's
    prefix home region, then the first scanned region that listed it.
    """
    per_region: dict[str, list[str]] = {}
    for region in regions:
        entitled = entitled_by_region.get(region, set())
        per_region[region] = sorted(entitled & catalog_ids)

    assigned: dict[str, str] = {}
    for region in regions:
        for model_id in per_region[region]:
            if model_id in assigned:
                continue
            if model_id in region_overrides:
                assigned[model_id] = region_overrides[model_id]
            elif model_id in per_region.get(default_region, []):
                assigned[model_id] = default_region
            else:
                home = next((r for p, r in PREFIX_HOME_REGION.items() if model_id.startswith(p)), None)
                if model_id.startswith("global."):
                    home = default_region
                assigned[model_id] = home or region
    return assigned
