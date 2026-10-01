"""Rung 5, invocable: live probes through pi, provider-agnostic.

Ported from the bash script's `probe_one`, extended to the router
(decision 1: deployed does not imply invocable there either).

Semantics, exactly:

- Command: `pi -p "hi" --provider <p> --model <id> --no-session
  --no-extensions`, stdin from devnull. Bedrock probes carry
  `AWS_PROFILE`/`AWS_REGION`; router probes rely on
  `AI_MODEL_ROUTER_API_KEY` in the environment.
- Classification is by output text, NEVER by exit code (pi can exit 0 even
  when the model call fails):
  - OK: exit 0 and no `error|exception|denied|not found|invalid|warning`
    (case-insensitive) in the output.
  - Timeout (kill after `PROBE_TIMEOUT`s) -> FAIL, counted systemic.
  - FAIL reason: first line matching the error regex, truncated to 120 chars.
- Circuit breaker: `PROBE_FAIL_CIRCUIT` consecutive SYSTEMIC failures trips
  and aborts the remaining probes; nothing is written afterwards. Per-model
  failures (marketplace not subscribed, no streaming tool use) are neutral:
  they neither advance nor reset the streak. Any OK resets it. (Lesson from
  the 2026-09-14 false trip: alphabetically clustered, account-unavailable
  catalog ids must not trip the breaker.)
"""

from __future__ import annotations

import asyncio
import enum
import os
import re
from dataclasses import dataclass

from pi_model_sync.ladder import BEDROCK
from pi_model_sync.paths import Config

ERROR_RE = re.compile(r"error|exception|denied|not found|invalid|warning", re.IGNORECASE)
REASON_RE = re.compile(r"error|exception|denied|not found|invalid", re.IGNORECASE)
SYSTEMIC_RE = re.compile(
    r"sso|expired.?token|credential|throttl|econnrefused|enotfound|etimedout|socket hang up|could not connect|unable to locate",
    re.IGNORECASE,
)


class ProbeStatus(enum.Enum):
    OK = "OK"
    FAIL = "FAIL"
    SKIP = "SKIP"  # circuit breaker tripped before this probe ran


@dataclass(frozen=True)
class ProbeOutcome:
    id: str
    provider: str
    region: str
    status: ProbeStatus
    systemic: bool = False
    reason: str = ""


def classify_output(exit_code: int, output: str, *, timed_out: bool, timeout: int) -> tuple[ProbeStatus, bool, str]:
    """Classify one probe by output text. Returns (status, systemic, reason)."""
    if timed_out:
        return ProbeStatus.FAIL, True, f"(timed out after {timeout}s)"
    if exit_code == 0 and not ERROR_RE.search(output):
        return ProbeStatus.OK, False, ""
    match = next((line for line in output.splitlines() if REASON_RE.search(line)), "")
    reason = f"({match.strip()[:120]})" if match else f"(exit {exit_code}, no error line matched)"
    return ProbeStatus.FAIL, bool(SYSTEMIC_RE.search(output)), reason


class CircuitBreaker:
    """Counts consecutive systemic failures. Single-threaded asyncio, so no locks."""

    def __init__(self, threshold: int) -> None:
        self.threshold = threshold
        self.streak = 0
        self.tripped = False

    def record(self, outcome: ProbeOutcome) -> None:
        if outcome.status is ProbeStatus.OK:
            self.streak = 0
        elif outcome.status is ProbeStatus.FAIL and outcome.systemic:
            self.streak += 1
            if self.streak >= self.threshold:
                self.tripped = True
        # Per-model FAILs are neutral: they neither advance nor reset the streak.


async def probe_one(model_id: str, provider: str, region: str, cfg: Config) -> ProbeOutcome:
    """Probe one model through pi, with a kill-on-timeout deadline."""
    env = None
    if provider == BEDROCK:
        env = {**os.environ, "AWS_PROFILE": cfg.aws_profile, "AWS_REGION": region}
    proc = await asyncio.create_subprocess_exec(
        "pi",
        "-p",
        "hi",
        "--provider",
        provider,
        "--model",
        model_id,
        "--no-session",
        "--no-extensions",
        stdin=asyncio.subprocess.DEVNULL,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.STDOUT,
        env=env,
    )
    timed_out = False
    try:
        output, _ = await asyncio.wait_for(proc.communicate(), timeout=cfg.probe_timeout)
        exit_code = proc.returncode or 0
    except TimeoutError:
        timed_out = True
        proc.kill()
        await proc.wait()
        output, exit_code = b"", 1
    status, systemic, reason = classify_output(
        exit_code, output.decode(errors="replace"), timed_out=timed_out, timeout=cfg.probe_timeout
    )
    return ProbeOutcome(id=model_id, provider=provider, region=region, status=status, systemic=systemic, reason=reason)


@dataclass
class ProbeBatch:
    outcomes: list[ProbeOutcome]
    tripped: bool


async def probe_all(
    candidates: list[tuple[str, str, str]],  # (id, provider, region)
    cfg: Config,
) -> ProbeBatch:
    """Probe every candidate, `PROBE_CONCURRENCY` at a time, with the breaker."""
    semaphore = asyncio.Semaphore(cfg.probe_concurrency)
    breaker = CircuitBreaker(cfg.probe_fail_circuit)

    async def run(model_id: str, provider: str, region: str) -> ProbeOutcome:
        async with semaphore:
            if breaker.tripped:
                return ProbeOutcome(
                    id=model_id,
                    provider=provider,
                    region=region,
                    status=ProbeStatus.SKIP,
                    reason="(circuit breaker tripped -- see earlier error)",
                )
            outcome = await probe_one(model_id, provider, region, cfg)
            breaker.record(outcome)
            return outcome

    outcomes = await asyncio.gather(*(run(mid, prov, region) for mid, prov, region in candidates))
    return ProbeBatch(outcomes=list(outcomes), tripped=breaker.tripped)
