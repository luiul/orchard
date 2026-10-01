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


def test_stubs_report_not_implemented():
    for cmd in SUBCOMMANDS:
        result = runner.invoke(app, [cmd])
        assert result.exit_code == 1
        assert "not implemented yet" in result.output
