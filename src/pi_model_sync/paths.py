"""Runtime configuration: paths, AWS region contract, probe tuning, router auth.

Everything is env-driven, with the same env contract as the retired bash
script (`sync-enabled-models.sh`) where one exists:

- `DOTFILES`            dotfiles checkout (default: `~/dotfiles`)
- `PI_SETTINGS`         dotfiles copy of settings.json (patterns live here)
- `BEDROCK_MODELS_JSON` dotfiles copy of bedrock-models.json (stow symlinked)
- `MODELS_JSON`         dotfiles copy of models.json (stow symlinked)
- `MODEL_REGISTRY`      dotfiles model-registry.json (hand-maintained, stowed)
- `AWS_PROFILE`         default `sso-bedrock`
- `AWS_REGION`          default `eu-west-1` (pi's default invoke region)
- `BEDROCK_REGIONS`     extra regions to scan, space-separated
- `PROBE_TIMEOUT` / `PROBE_CONCURRENCY` / `PROBE_FAIL_CIRCUIT`
- `AI_MODEL_ROUTER_API_KEY`  router bearer token; if unset, loaded from
  gitignored `~/dotfiles/.env` and exported so router probes inherit it
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

ROUTER_BASE_URL = "https://ai-model-router-api.eu.foundations.prod.int.hellofresh.io/v1"

DEFAULT_REGION = "eu-west-1"
DEFAULT_EXTRA_REGIONS = ("us-east-1", "ap-northeast-1", "ap-southeast-2")


class ConfigError(Exception):
    """Fatal misconfiguration (bad env value). Exit 2, mirroring the bash script."""


def _positive_int(env: dict[str, str], name: str, default: int) -> int:
    raw = env.get(name)
    if raw is None:
        return default
    if not raw.isdigit() or int(raw) == 0:
        raise ConfigError(f"{name} must be a positive integer (got: {raw})")
    return int(raw)


def load_router_api_key(env: dict[str, str], dotfiles: Path) -> str | None:
    """Return the router API key, falling back to `~/dotfiles/.env`.

    Side effect on success: the key is exported into `os.environ` so live
    probes through pi (subprocesses) inherit it, matching how an interactive
    shell with a sourced .env behaves.
    """
    if key := env.get("AI_MODEL_ROUTER_API_KEY"):
        return key
    env_file = dotfiles / ".env"
    if env_file.is_file():
        for line in env_file.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            key_part, _, value = line.partition("=")
            if key_part.strip() in {"AI_MODEL_ROUTER_API_KEY", "export AI_MODEL_ROUTER_API_KEY"}:
                key = value.strip().strip('"').strip("'")
                if key:
                    os.environ["AI_MODEL_ROUTER_API_KEY"] = key
                    return key
    return None


@dataclass(frozen=True)
class Config:
    dotfiles: Path
    pi_settings: Path
    live_settings: Path
    bedrock_models_json: Path
    models_json: Path
    model_registry: Path
    aws_profile: str
    default_region: str
    extra_regions: tuple[str, ...]
    probe_timeout: int
    probe_concurrency: int
    probe_fail_circuit: int
    router_base_url: str
    router_api_key: str | None

    @property
    def regions(self) -> tuple[str, ...]:
        """All scanned regions, default first (its usable subset drives patterns)."""
        return (self.default_region, *self.extra_regions)

    @classmethod
    def from_env(
        cls,
        env: dict[str, str] | None = None,
        *,
        probe_timeout: int | None = None,
        probe_concurrency: int | None = None,
        probe_fail_circuit: int | None = None,
    ) -> Config:
        """Build from env. Explicit CLI option values win over env vars."""
        env = dict(os.environ if env is None else env)
        dotfiles = Path(env.get("DOTFILES", "~/dotfiles")).expanduser()
        agent_dir = dotfiles / "pi" / ".pi" / "agent"
        default_region = env.get("AWS_REGION", DEFAULT_REGION)
        extra = env.get("BEDROCK_REGIONS")
        extra_regions = tuple(extra.split()) if extra else DEFAULT_EXTRA_REGIONS
        return cls(
            dotfiles=dotfiles,
            pi_settings=Path(env.get("PI_SETTINGS", agent_dir / "settings.json")).expanduser(),
            live_settings=Path("~/.pi/agent/settings.json").expanduser(),
            bedrock_models_json=Path(env.get("BEDROCK_MODELS_JSON", agent_dir / "bedrock-models.json")).expanduser(),
            models_json=Path(env.get("MODELS_JSON", agent_dir / "models.json")).expanduser(),
            model_registry=Path(env.get("MODEL_REGISTRY", agent_dir / "model-registry.json")).expanduser(),
            aws_profile=env.get("AWS_PROFILE", "sso-bedrock"),
            default_region=default_region,
            extra_regions=extra_regions,
            probe_timeout=probe_timeout if probe_timeout is not None else _positive_int(env, "PROBE_TIMEOUT", 45),
            probe_concurrency=probe_concurrency
            if probe_concurrency is not None
            else _positive_int(env, "PROBE_CONCURRENCY", 3),
            probe_fail_circuit=probe_fail_circuit
            if probe_fail_circuit is not None
            else _positive_int(env, "PROBE_FAIL_CIRCUIT", 6),
            router_base_url=env.get("ROUTER_BASE_URL", ROUTER_BASE_URL),
            router_api_key=load_router_api_key(env, dotfiles),
        )
