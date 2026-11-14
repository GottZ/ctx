"""What ``hermes plugins install`` does with this plugin, read from the Hermes checkout under test.

The installer clones the plugin and runs ``tools.plugin_guard.scan_plugin`` over the plugin directory before
anything is placed. ``safe`` installs; ``caution`` asks a terminal for confirmation and is refused without one
(Hermes Desktop, the dashboard, scripts); ``dangerous`` is refused. The plugin has to scan ``safe`` on every
supported Hermes, so the documented install works without a terminal. The remaining findings are
informational there (medium and low); the report lists them.

It then reads the plugin's Python declaration (``pm.plugin_declarations``): a plugin that declares packages
gets a dependency question (refused without a terminal or ``--yes-deps``) and becomes a member of Hermes' uv
workspace, whose lock must agree with Hermes' own pins. The plugin needs nothing beyond Hermes, so it declares
nothing: no question, no workspace member.
"""

from __future__ import annotations

import shutil
import subprocess
from collections import Counter
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
SOURCE = "GottZ/ctx/integrations/hermes/plugin/ctx_discord_policy"


def _shipped_tree(target: Path) -> Path:
    """A copy of what an install clones: the files git tracks in the plugin directory (a test run or a local
    checkout leaves untracked files next to them that no clone carries)."""
    listed = subprocess.run(["git", "-C", str(REPO), "ls-files", "-z", "--", "."], capture_output=True)
    if listed.returncode != 0:
        pytest.fail(f"the plugin directory is not a git checkout: {listed.stderr.decode(errors='replace')}")
    names = [n for n in listed.stdout.decode("utf-8").split("\0") if n]
    assert names, "git lists no file of the plugin directory"
    for name in names:
        source, dest = REPO / name, target / name
        dest.parent.mkdir(parents=True, exist_ok=True)
        if source.is_symlink():
            dest.symlink_to(source.readlink())
        elif source.is_file():
            shutil.copy2(source, dest)
    return target


def test_install_scan_is_safe(hermes, tmp_path):
    from tools.plugin_guard import format_scan_report, scan_plugin, should_allow_plugin_install

    tree = _shipped_tree(tmp_path / "ctx_discord_policy")
    result = scan_plugin(tree, source=SOURCE)
    allowed, reason = should_allow_plugin_install(result, force=False)
    severities = Counter(f.severity for f in result.findings)
    hermes.write_report("install-scan", {
        "verdict": result.verdict, "allowed": allowed, "reason": reason, "counts": dict(severities),
        "findings": [{"severity": f.severity, "category": f.category, "pattern_id": f.pattern_id,
                      "file": f.file, "line": f.line, "match": f.match} for f in result.findings]})
    print(f"install scan on {hermes.ref}: {result.verdict}, {len(result.findings)} findings {dict(severities)}")
    blocking = [f for f in result.findings if f.severity in ("high", "critical")]
    assert result.verdict == "safe" and allowed is True and not blocking, format_scan_report(result)


def test_install_declares_no_python_dependencies(hermes, tmp_path, monkeypatch):
    import builtins

    from hermes_cli import plugins_cmd_install
    from pm import workspace
    from pm.plugin_declarations import read_native_manifest, read_python_declaration

    tree = _shipped_tree(tmp_path / "ctx_discord_policy")
    declaration = read_python_declaration(tree)
    assert declaration.install_requirements == () and not declaration.installs_packages, declaration
    # The predicate the workspace generator selects uv members with (v0.22.4: any pyproject.toml).
    assert not workspace._is_member_candidate(tree), "the plugin would enter Hermes' uv workspace and lock"

    printed = []

    class Console:
        def print(self, *args, **kwargs):
            printed.append(" ".join(str(a) for a in args))

    def no_question(*_args, **_kwargs):
        raise AssertionError("the installer asked a question")

    monkeypatch.setattr(builtins, "input", no_question)
    manifest = read_native_manifest(tree / "plugin.yaml")
    for terminal in (False, True):
        monkeypatch.setattr("sys.stdin.isatty", lambda: terminal, raising=False)
        monkeypatch.setattr("sys.stdout.isatty", lambda: terminal, raising=False)
        assert plugins_cmd_install._install_plugin_python_deps(manifest, tree, Console()) == (True, None)
    assert printed == [], printed


def test_a_profile_finds_only_the_plugins_installed_in_its_own_home(hermes, tmp_path):
    """Why the README installs the plugin in every profile: discovery and ``plugins install`` both use the
    plugins directory of the active home, and ``hermes -p <profile>`` makes the profile's home active."""
    from fakes.hermes_homes import install_plugin
    from hermes_cli.plugins_cmd import _plugins_dir
    from hermes_cli.plugins_discovery import collect_directory_manifests
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    root = tmp_path / "root"
    install_plugin(root / "plugins" / "ctx_discord_policy")
    (root / "config.yaml").write_text("plugins:\n  enabled: [ctx_discord_policy]\n", encoding="utf-8")
    profile = root / "profiles" / "beta"
    profile.mkdir(parents=True)
    (profile / "config.yaml").write_text("plugins:\n  enabled: [ctx_discord_policy]\n", encoding="utf-8")

    def seen(home: Path) -> tuple[list[str], Path]:
        token = set_hermes_home_override(str(home))
        try:
            names = sorted(m.name for m in collect_directory_manifests() if m.source == "user")
            return names, _plugins_dir()
        finally:
            reset_hermes_home_override(token)

    assert seen(root) == (["ctx_discord_policy"], root / "plugins")
    assert seen(profile) == ([], profile / "plugins")       # installed in the default home only
    install_plugin(profile / "plugins" / "ctx_discord_policy")
    assert seen(profile) == (["ctx_discord_policy"], profile / "plugins")


PLACEHOLDERS = {
    "<server id>": "123456789012345678", "<profile>": "beta", "<your Discord user id>": "234567890123456789",
    "<file>": "server.json", "<application id>": "345678901234567890", "<commit>": "0" * 40,
}


def _readme_commands() -> list[str]:
    """Every whole ``hermes …`` command of the README: the lines of its code blocks and the code spans of its
    tables (prose mentions a command by name only and is not parsed)."""
    import re

    text = (REPO / "README.md").read_text(encoding="utf-8")
    commands = []
    for block in re.findall(r"^ *```[a-z]*\n(.*?)^ *```", text, re.M | re.S):  # list items indent
        for line in block.splitlines():
            line = re.split(r"\s{2,}", line.strip())[0]   # the overview block explains each command after it
            if line.startswith("hermes "):
                commands.append(line)
    for row in (line for line in text.splitlines() if line.startswith("|")):
        commands += [span for span in re.findall(r"`([^`]+)`", row) if span.startswith("hermes ")]
    return commands


def test_every_command_in_the_readme_parses(hermes, anchor):
    """Subcommands, options and their order as this Hermes parses them; ``ctx-discord-policy`` is the plugin's
    own command, attached the way Hermes attaches a plugin command."""
    import contextlib
    import io
    import shlex

    from hermes_cli import main as hermes_main
    from hermes_cli._parser import PRE_ARGPARSE_INHERITED_FLAGS

    parser, subparsers = hermes_main._build_cli_parser()
    if "ctx-discord-policy" not in subparsers.choices:
        anchor.mod("cli").setup(subparsers.add_parser("ctx-discord-policy"))
    profile_flags = {flag for flag, _takes_value in PRE_ARGPARSE_INHERITED_FLAGS}
    commands = _readme_commands()
    assert len(commands) >= 10, commands
    # Named in prose: the command and the option, as the README puts them to use.
    prose = " ".join((REPO / "README.md").read_text(encoding="utf-8").split())
    assert "`hermes plugins info ctx_discord_policy`" in prose and "`--ref <commit>`" in prose
    commands += ["hermes plugins info ctx_discord_policy", "hermes -p <profile> plugins install "
                 "GottZ/ctx/integrations/hermes/plugin/ctx_discord_policy --ref <commit> --enable"]
    failures = []
    for command in commands:
        for placeholder, value in PLACEHOLDERS.items():
            command = command.replace(placeholder, value)
        assert "<" not in command, f"unknown placeholder in {command!r}"
        argv = shlex.split(command)[1:]
        if argv[0] in profile_flags:   # Hermes reads -p <profile> before argparse, ahead of the subcommand
            argv = argv[2:]
        stderr = io.StringIO()
        try:
            with contextlib.redirect_stderr(stderr):
                parser.parse_args(argv)
        except SystemExit as exc:
            failures.append(f"{command}: exit {exc.code}: {stderr.getvalue().strip()[-300:]}")
    assert not failures, "\n".join(failures)
