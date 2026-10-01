"""Probe classification and circuit-breaker semantics.

Classification tests use captured real pi outputs (tests/fixtures/probe-*).
Breaker tests stub `probe_one`; no subprocess ever runs in tests.
"""

import asyncio

import pytest

from pi_model_sync.ladder import BEDROCK
from pi_model_sync.paths import Config
from pi_model_sync.probe import (
    CircuitBreaker,
    ProbeOutcome,
    ProbeStatus,
    classify_output,
    probe_all,
)


@pytest.fixture()
def cfg(tmp_path) -> Config:
    return Config.from_env({"DOTFILES": str(tmp_path), "PROBE_TIMEOUT": "5", "PROBE_FAIL_CIRCUIT": "3"})


def _fixture(fixtures_dir, name) -> str:
    return (fixtures_dir / name).read_text()


def test_classify_ok_bedrock(fixtures_dir):
    status, systemic, reason = classify_output(
        0, _fixture(fixtures_dir, "probe-ok-bedrock.txt"), timed_out=False, timeout=45
    )
    assert status is ProbeStatus.OK
    assert not systemic
    assert reason == ""


def test_classify_ok_router(fixtures_dir):
    status, _, _ = classify_output(0, _fixture(fixtures_dir, "probe-ok-router.txt"), timed_out=False, timeout=45)
    assert status is ProbeStatus.OK


def test_classify_per_model_failure(fixtures_dir):
    status, systemic, reason = classify_output(
        1, _fixture(fixtures_dir, "probe-fail-model.txt"), timed_out=False, timeout=45
    )
    assert status is ProbeStatus.FAIL
    assert not systemic  # marketplace subscription missing is per-model
    assert reason.startswith("(AccessDeniedException")
    assert len(reason) <= 122  # truncated to 120 chars plus parens


def test_classify_systemic_failure(fixtures_dir):
    status, systemic, _ = classify_output(
        1, _fixture(fixtures_dir, "probe-fail-systemic.txt"), timed_out=False, timeout=45
    )
    assert status is ProbeStatus.FAIL
    assert systemic  # "Could not load credentials"


def test_classify_timeout_is_systemic():
    status, systemic, reason = classify_output(1, "", timed_out=True, timeout=45)
    assert status is ProbeStatus.FAIL
    assert systemic
    assert "timed out after 45s" in reason


def test_exit_zero_with_error_text_still_fails(fixtures_dir):
    """pi can exit 0 even when the model call fails: text wins over exit code."""
    status, systemic, _ = classify_output(
        0, _fixture(fixtures_dir, "probe-fail-model.txt"), timed_out=False, timeout=45
    )
    assert status is ProbeStatus.FAIL
    assert not systemic


def _outcome(status, systemic=False):
    return ProbeOutcome(id="m", provider=BEDROCK, region="r", status=status, systemic=systemic)


def test_breaker_trips_on_consecutive_systemic():
    breaker = CircuitBreaker(3)
    for _ in range(2):
        breaker.record(_outcome(ProbeStatus.FAIL, systemic=True))
    assert not breaker.tripped
    breaker.record(_outcome(ProbeStatus.FAIL, systemic=True))
    assert breaker.tripped


def test_breaker_per_model_failures_do_not_trip():
    breaker = CircuitBreaker(3)
    for _ in range(10):
        breaker.record(_outcome(ProbeStatus.FAIL, systemic=False))
    assert not breaker.tripped
    assert breaker.streak == 0


def test_breaker_per_model_failures_do_not_reset_systemic_streak():
    """A systemic cascade in progress must not be hidden by per-model noise."""
    breaker = CircuitBreaker(3)
    breaker.record(_outcome(ProbeStatus.FAIL, systemic=True))
    breaker.record(_outcome(ProbeStatus.FAIL, systemic=False))  # neutral
    breaker.record(_outcome(ProbeStatus.FAIL, systemic=True))
    assert breaker.streak == 2
    assert not breaker.tripped
    breaker.record(_outcome(ProbeStatus.FAIL, systemic=True))
    assert breaker.tripped


def test_breaker_ok_resets_streak():
    breaker = CircuitBreaker(2)
    breaker.record(_outcome(ProbeStatus.FAIL, systemic=True))
    breaker.record(_outcome(ProbeStatus.OK))
    assert breaker.streak == 0
    breaker.record(_outcome(ProbeStatus.FAIL, systemic=True))
    assert not breaker.tripped


def test_probe_all_skips_after_trip(cfg, monkeypatch):
    scripted = [ProbeStatus.FAIL] * 3 + [ProbeStatus.OK] * 3
    calls = []

    async def fake_probe_one(model_id, provider, region, cfg):
        calls.append(model_id)
        return ProbeOutcome(
            id=model_id, provider=provider, region=region, status=scripted[len(calls) - 1], systemic=True
        )

    monkeypatch.setattr("pi_model_sync.probe.probe_one", fake_probe_one)
    candidates = [(f"m{i}", BEDROCK, "eu-west-1") for i in range(6)]
    batch = asyncio.run(probe_all(candidates, cfg))
    assert batch.tripped
    statuses = [o.status for o in batch.outcomes]
    assert ProbeStatus.SKIP in statuses
