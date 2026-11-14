"""Shared fixtures: a fresh process-wide runtime anchor per test."""

from __future__ import annotations

import importlib
import importlib.util
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

ROOT = Path(__file__).resolve().parents[1]
ANCHOR = "ctx_discord_policy"


def evict_anchor() -> None:
    for key in [k for k in sys.modules if k == ANCHOR or k.startswith(ANCHOR + ".")]:
        sys.modules.pop(key, None)


def load_entry(plugin_dir: Path = ROOT, name: str = "hermes_plugins_test.ctx_discord_policy"):
    """Import a plugin entry point the way Hermes does: one module object per profile."""
    spec = importlib.util.spec_from_file_location(
        name, plugin_dir / "__init__.py", submodule_search_locations=[str(plugin_dir)])
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


@pytest.fixture(autouse=True)
def _isolated_hermes_home(request, monkeypatch, tmp_path_factory):
    """No test may ever touch a real Hermes home: every test starts with a throwaway HERMES_HOME."""
    if "contract" in Path(str(request.node.path)).parts:
        return  # contract tests manage their own temporary homes
    monkeypatch.setenv("HERMES_HOME", str(tmp_path_factory.mktemp("isolated-home")))


class Anchor(SimpleNamespace):
    def mod(self, name: str):
        return importlib.import_module(f"{ANCHOR}.{name}")


@pytest.fixture
def anchor(monkeypatch, tmp_path):
    evict_anchor()
    home = tmp_path / "home"
    home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(home))
    entry = load_entry()
    package = entry.load_anchor(ROOT)
    ns = Anchor(entry=entry, package=package, home=home, tmp=tmp_path)
    ns.runtime = ns.mod("runtime")
    ns.RUNTIME = ns.runtime.RUNTIME
    ns.hooks = ns.mod("policy.hooks")
    ns.routes = ns.mod("policy.routes")
    ns.bootstrap = ns.mod("bootstrap")
    try:
        yield ns
    finally:
        evict_anchor()
        for key in [k for k in sys.modules if k.startswith("hermes_plugins_test")]:
            sys.modules.pop(key, None)
