"""Catalog parsing against a captured real `pi --list-models` fixture."""

from pi_model_sync.catalog import parse_catalog
from pi_model_sync.ladder import BEDROCK, ROUTER


def test_parses_captured_fixture(catalog_text):
    entries = parse_catalog(catalog_text)
    bedrock = [e for e in entries if e.provider == BEDROCK]
    router = [e for e in entries if e.provider == ROUTER]
    assert len(bedrock) == 180
    assert len(router) == 11


def test_fields_are_split(catalog_text):
    entries = {e.id: e for e in parse_catalog(catalog_text)}
    kimi = entries["moonshotai/Kimi-K3"]
    assert kimi.provider == ROUTER
    assert kimi.thinking == "yes"
    assert kimi.images == "yes"
    haiku = entries["eu.anthropic.claude-haiku-4-5-20251001-v1:0"]
    assert haiku.provider == BEDROCK
    assert haiku.context == "200K"


def test_header_and_blank_lines_are_skipped():
    text = "provider  model  context  max-out  thinking  images\n\nai-model-router  m1  200K  64K  yes  no\n"
    entries = parse_catalog(text)
    assert [e.id for e in entries] == ["m1"]


def test_malformed_lines_are_skipped():
    entries = parse_catalog("provider  model  context  max-out  thinking  images\nonly three fields here\n")
    assert entries == []
