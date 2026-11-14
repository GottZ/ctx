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
