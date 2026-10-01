"""CLI surface. Only offline-safe paths are tested here: help, version, and
env validation (which exits before any network/AWS call). Live behavior is
covered by the module tests with captured fixtures plus manual sync runs."""

from typer.testing import CliRunner

from pi_model_sync.cli import app

runner = CliRunner()

SUBCOMMANDS = ("fetch", "probe", "report", "sync")


def test_help_lists_all_subcommands():
    result = runner.invoke(app, ["--help"])
    assert result.exit_code == 0
    for cmd in SUBCOMMANDS:
        assert cmd in result.output


def test_each_subcommand_help_renders():
    for cmd in SUBCOMMANDS:
        result = runner.invoke(app, [cmd, "--help"])
        assert result.exit_code == 0, f"{cmd} --help failed: {result.output}"


def test_version_flag():
    result = runner.invoke(app, ["--version"])
    assert result.exit_code == 0
    assert "pi-model-sync" in result.output


def test_invalid_probe_timeout_env_exits_2():
    result = runner.invoke(app, ["report", "--no-probe"], env={"PROBE_TIMEOUT": "abc"})
    assert result.exit_code == 2
    assert "PROBE_TIMEOUT must be a positive integer" in result.output


def test_sync_help_documents_parity_flags():
    result = runner.invoke(app, ["sync", "--help"])
    for flag in ("--dry-run", "--no-probe", "--strict"):
        assert flag in result.output
