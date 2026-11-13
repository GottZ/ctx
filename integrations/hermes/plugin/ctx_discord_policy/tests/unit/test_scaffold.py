"""S0: the entry point imports without Hermes and its register() is safe to call."""

from __future__ import annotations

import importlib.util
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def _load_entry():
    spec = importlib.util.spec_from_file_location("dsp_entry_scaffold", ROOT / "__init__.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_register_imports_without_hermes():
    entry = _load_entry()
    calls = []

    class Ctx:
        def __getattr__(self, name):
            def record(*args, **kwargs):
                calls.append(name)
            return record

    entry.register(Ctx())
    assert entry.PLUGIN_VERSION == "3.0.0"
