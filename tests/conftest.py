import json
from pathlib import Path

import pytest

FIXTURES = Path(__file__).parent / "fixtures"


@pytest.fixture()
def fixtures_dir() -> Path:
    return FIXTURES


@pytest.fixture()
def catalog_text() -> str:
    return (FIXTURES / "pi-list-models.txt").read_text()


@pytest.fixture()
def router_models() -> dict:
    return json.loads((FIXTURES / "router-models.json").read_text())


@pytest.fixture()
def router_model_info() -> dict:
    return json.loads((FIXTURES / "router-model-info.json").read_text())


@pytest.fixture()
def bedrock_fixture() -> dict[str, dict]:
    """(region, source) -> captured AWS API response."""

    def load(region: str, source: str) -> dict:
        return json.loads((FIXTURES / f"bedrock-{source}-{region}.json").read_text())

    return {"load": load}
