"""The desktop half against real Hermes: admission lint and the unified-package copy (SETTINGS-PAGE §1.2, §5.7).

``hermes plugins validate`` lints every ``*.js`` of a plugin's desktop surface (no eval, no imports outside the
SDK, no script injection, no reach into the app's DOM); the catalog refuses a plugin with a finding. Hermes
copies ``desktop/plugin.js`` of a unified package into the app-level ``desktop-plugins/<id>/`` (Electron and,
from v0.23.0, the Webapp's host-side copy in ``hermes_cli.desktop_plugin_halves``).
"""

from __future__ import annotations

import importlib.util
import shutil
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
PLUGIN = "ctx_discord_policy"
BUNDLE = REPO / "desktop" / "plugin.js"

pytestmark = pytest.mark.skipif(not BUNDLE.is_file(), reason="no desktop/plugin.js in this tree")


def test_the_desktop_half_passes_hermes_admission_lint(hermes):
    import hermes_cli.plugin_validate_desktop as lint

    source = BUNDLE.read_text(encoding="utf-8")
    refused = lint.desktop_surface_findings(source)
    # The review-level findings (warnings shown at install) exist from v0.23.0 on; v0.22.4 has the refusals only.
    review = lint.desktop_review_findings(source) if hasattr(lint, "desktop_review_findings") else []
    hits = lint.desktop_surface_hits(REPO)
    hermes.write_report("dashboard-desktop-lint", {"refused": refused, "review": review, "hits": hits,
                                                   "bytes": len(source.encode("utf-8"))})
    assert refused == [] and hits == [] and review == []


def test_hermes_publishes_desktop_plugin_js_of_the_package(hermes, tmp_path):
    if importlib.util.find_spec("hermes_cli.desktop_plugin_halves") is None:
        pytest.skip("this Hermes copies the desktop half only in Electron (desktop/plugin.js layout required)")
    from hermes_cli.desktop_plugin_halves import check_desktop_tree, desktop_source, materialize_desktop_half

    package = tmp_path / "plugins" / PLUGIN
    package.mkdir(parents=True)
    for name in ("__init__.py", "plugin.yaml"):
        shutil.copy2(REPO / name, package / name)
    for name in ("dsp", "dashboard", "desktop"):
        shutil.copytree(REPO / name, package / name, ignore=shutil.ignore_patterns("__pycache__"))
    assert desktop_source(package) == package / "desktop"
    assert check_desktop_tree(package / "desktop", package) is None
    app_root = tmp_path / "desktop-plugins"
    app_root.mkdir()
    target = materialize_desktop_half(package, app_root, PLUGIN)
    assert target is not None and (target / "plugin.js").read_bytes() == BUNDLE.read_bytes()
    assert not (target / "plugin_api.py").exists() and not (target / "dsp").exists()   # only the desktop half
    hermes.write_report("dashboard-desktop-copy", {"target": target.name,
                                                   "files": sorted(p.name for p in target.iterdir())})
