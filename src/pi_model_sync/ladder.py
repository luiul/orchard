"""The availability ladder: shared record types every stage builds on.

Six rungs, each a subset of the one above (see
`docs/model-availability-vocabulary.md` in the dotfiles repo):

1. catalog   — pi knows the id (`pi --list-models`)
2. entitled  — the AWS account may call it in a region (Bedrock only)
   deployed  — the router serves it right now (router only)
3. candidate — worth probing (catalog ∩ entitled, or deployed ∩ catalog)
4. invocable — survives a live probe through pi
5. enabled   — matched by an `enabledModels` pattern in settings.json

Every rung cell is tri-state: YES, NO, or UNKNOWN (source unreachable or
stage not run). The shared currency is `ModelRow`: one report row per model.
"""

from __future__ import annotations

import enum
from dataclasses import dataclass, field

BEDROCK = "amazon-bedrock"
ROUTER = "ai-model-router"
PROVIDERS = (BEDROCK, ROUTER)


class Rung(enum.Enum):
    YES = "yes"
    NO = "no"
    UNKNOWN = "unknown"

    def __str__(self) -> str:
        return self.value


@dataclass(frozen=True)
class CatalogEntry:
    """One parsed row of `pi --list-models`."""

    provider: str
    id: str
    context: str
    max_out: str
    thinking: str
    images: str


@dataclass
class ModelRow:
    """One report row: a model and its rung flags."""

    provider: str
    id: str
    catalog: Rung = Rung.UNKNOWN
    entitled_deployed: Rung = Rung.UNKNOWN  # entitled (Bedrock) / deployed (router)
    candidate: Rung = Rung.UNKNOWN
    invocable: Rung = Rung.UNKNOWN
    enabled: Rung = Rung.UNKNOWN
    region: str = ""  # Bedrock: the region it probes from
    note: str = ""


@dataclass
class Drift:
    """The four drift findings reported below the table."""

    deployed_unknown_to_pi: list[str] = field(default_factory=list)
    listed_but_undeployed: list[str] = field(default_factory=list)
    dead_patterns: list[str] = field(default_factory=list)
    invocable_uncovered: list[str] = field(default_factory=list)
