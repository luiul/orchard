"""The pipeline: fetch -> candidates -> probe, shared by every command.

Each stage adds rung flags to the shared record set. `sync` layers artifact
writes on top; `fetch`/`probe`/`report` stop earlier. There is no caching of
source data between runs: pi and AWS are called fresh every time.
"""

from __future__ import annotations

import fnmatch
from dataclasses import dataclass

from pi_model_sync.artifacts import read_patterns, strip_pin, validate_patterns
from pi_model_sync.bedrock import Entitlements, assign_candidates, check_sso, fetch_entitlements
from pi_model_sync.catalog import load_catalog
from pi_model_sync.ladder import BEDROCK, ROUTER, CatalogEntry, Drift, ModelRow, Rung
from pi_model_sync.paths import Config
from pi_model_sync.probe import ProbeBatch, ProbeStatus, probe_all
from pi_model_sync.router import RouterDeployment, fetch_router, load_registry


class RouterUnreachableError(Exception):
    """--strict was given and the router is unreachable."""


@dataclass
class Pipeline:
    cfg: Config
    catalog: list[CatalogEntry]
    entitlements: Entitlements
    router: RouterDeployment
    patterns: list[str]
    bedrock_candidates: dict[str, str]  # id -> assigned probe region
    router_candidates: list[str]
    probes: ProbeBatch | None = None  # None under --no-probe

    @property
    def probed(self) -> bool:
        return self.probes is not None

    def catalog_ids(self, provider: str) -> set[str]:
        return {e.id for e in self.catalog if e.provider == provider}

    def invocable_bedrock(self) -> dict[str, str]:
        """id -> region for every invocable Bedrock model.

        Under --no-probe the candidates are used unchecked (bash parity:
        faster, but may include per-model/per-region failures).
        """
        if self.probes is None:
            return dict(self.bedrock_candidates)
        return {o.id: o.region for o in self.probes.outcomes if o.provider == BEDROCK and o.status is ProbeStatus.OK}

    def invocable_router(self) -> list[str]:
        if self.probes is None:
            return list(self.router_candidates)
        return sorted(o.id for o in self.probes.outcomes if o.provider == ROUTER and o.status is ProbeStatus.OK)

    def matchable_ids(self) -> set[str]:
        """What patterns validate against: invocable Bedrock ids plus invocable
        router ids. With --no-probe, candidates stand in (and the router
        catalog when the gateway is unreachable, same as the bash script)."""
        ids = set(self.invocable_bedrock())
        if not self.router.reachable and self.probes is None:
            ids |= self.catalog_ids(ROUTER)
        else:
            ids |= set(self.invocable_router())
        return ids

    def rows(self) -> list[ModelRow]:
        probe_by_key = {}
        skipped: set[tuple[str, str]] = set()
        if self.probes is not None:
            for o in self.probes.outcomes:
                if o.status is ProbeStatus.SKIP:
                    skipped.add((o.provider, o.id))
                else:
                    probe_by_key[(o.provider, o.id)] = o

        stripped = [strip_pin(p) for p in self.patterns]

        def enabled_flag(model_id: str) -> Rung:
            return Rung.YES if any(fnmatch.fnmatchcase(model_id, p) for p in stripped) else Rung.NO

        rows: list[ModelRow] = []
        entitled_all = self.entitlements.all_ids
        degraded = bool(self.entitlements.degraded)

        for model_id in sorted(self.catalog_ids(BEDROCK)):
            entitled = Rung.YES if model_id in entitled_all else (Rung.UNKNOWN if degraded else Rung.NO)
            candidate = Rung.YES if model_id in self.bedrock_candidates else (Rung.UNKNOWN if degraded else Rung.NO)
            invocable = Rung.UNKNOWN
            note = ""
            if self.probes is not None:
                outcome = probe_by_key.get((BEDROCK, model_id))
                if outcome is not None:
                    invocable = Rung.YES if outcome.status is ProbeStatus.OK else Rung.NO
                    note = outcome.reason
                elif (BEDROCK, model_id) in skipped:
                    note = "(circuit breaker tripped)"
            rows.append(
                ModelRow(
                    provider=BEDROCK,
                    id=model_id,
                    catalog=Rung.YES,
                    entitled_deployed=entitled,
                    candidate=candidate,
                    invocable=invocable,
                    enabled=enabled_flag(model_id),
                    region=self.bedrock_candidates.get(model_id, ""),
                    note=note,
                )
            )

        router_catalog = self.catalog_ids(ROUTER)
        deployed = set(self.router.deployed)
        for model_id in sorted(router_catalog | deployed):
            in_catalog = model_id in router_catalog
            is_deployed = model_id in deployed
            if not self.router.reachable:
                deployed_flag = candidate = invocable = Rung.UNKNOWN
            else:
                deployed_flag = Rung.YES if is_deployed else Rung.NO
                candidate = Rung.YES if (is_deployed and in_catalog) else Rung.NO
                invocable = Rung.UNKNOWN
                if self.probes is not None:
                    outcome = probe_by_key.get((ROUTER, model_id))
                    if outcome is not None:
                        invocable = Rung.YES if outcome.status is ProbeStatus.OK else Rung.NO
            note = ""
            if self.router.reachable:
                if is_deployed and not in_catalog:
                    note = "deployed but unknown to pi"
                elif in_catalog and not is_deployed:
                    note = "no longer deployed"
                elif self.probes is not None and (o := probe_by_key.get((ROUTER, model_id))) is not None:
                    note = o.reason
            else:
                note = "router unreachable"
            rows.append(
                ModelRow(
                    provider=ROUTER,
                    id=model_id,
                    catalog=Rung.YES if in_catalog else Rung.NO,
                    entitled_deployed=deployed_flag,
                    candidate=candidate,
                    invocable=invocable,
                    enabled=enabled_flag(model_id),
                    note=note,
                )
            )
        return rows

    def drift(self) -> Drift:
        dead, _covered, uncovered = validate_patterns(self.patterns, self.matchable_ids())
        if self.router.reachable:
            deployed = set(self.router.deployed)
            catalog_router = self.catalog_ids(ROUTER)
            deployed_unknown = sorted(deployed - catalog_router)
            listed_undeployed = sorted(catalog_router - deployed)
        else:
            deployed_unknown = []
            listed_undeployed = []
        return Drift(
            deployed_unknown_to_pi=deployed_unknown,
            listed_but_undeployed=listed_undeployed,
            dead_patterns=dead,
            invocable_uncovered=uncovered,
        )


async def run_pipeline(cfg: Config, *, do_probe: bool, strict: bool) -> Pipeline:
    """Fetch every source, compute candidates, and (unless --no-probe) probe."""
    catalog = load_catalog()
    check_sso(cfg)  # exactly once per run; never logs in itself
    entitlements = fetch_entitlements(cfg)
    router = fetch_router(cfg)
    if strict and not router.reachable:
        raise RouterUnreachableError(f"router unreachable and --strict given: {router.error}")

    registry = load_registry(cfg.model_registry)
    bedrock_candidates = assign_candidates(
        entitlements.by_region,
        {e.id for e in catalog if e.provider == BEDROCK},
        cfg.regions,
        cfg.default_region,
        registry["probeRegionOverrides"],
    )
    router_candidates = sorted(set(router.deployed) & {e.id for e in catalog if e.provider == ROUTER})
    patterns = read_patterns(cfg.pi_settings)

    probes: ProbeBatch | None = None
    if do_probe:
        targets = [(mid, BEDROCK, region) for mid, region in sorted(bedrock_candidates.items())]
        if router.reachable:
            targets += [(mid, ROUTER, "") for mid in router_candidates]
        probes = await probe_all(targets, cfg)

    return Pipeline(
        cfg=cfg,
        catalog=catalog,
        entitlements=entitlements,
        router=router,
        patterns=patterns,
        bedrock_candidates=bedrock_candidates,
        router_candidates=router_candidates,
        probes=probes,
    )
