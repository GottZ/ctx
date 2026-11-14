"""Hermes' install scanner on this plugin.

``hermes plugins install`` (and ``update``) clones the plugin and runs ``tools.plugin_guard.scan_plugin`` over
the plugin directory before anything is placed. ``safe`` installs; ``caution`` asks a terminal for
confirmation and is refused without one (Hermes Desktop, the dashboard, scripts); ``dangerous`` is refused.
The plugin has to scan ``safe`` on every supported Hermes, so the documented install works without a
terminal. The remaining findings are informational there (medium and low); the report lists them.
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
