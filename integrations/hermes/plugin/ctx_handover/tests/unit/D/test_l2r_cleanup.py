"""Layer R for §2 "Bereinigung" (D) of the layer-2 contract: the key ``changed`` is gone from
``tests/fakes/planning_fake.py`` and ``tests/unit/D/test_plan_r25.py`` — the fake ``model_change`` has exactly the
keys of the one producer, B's ``session_import.map_model`` (contract X04), and no D test builds a ``model_change``
with a ``changed`` key."""

from __future__ import annotations

import ast
from pathlib import Path

from ctx_handover.engine import session_import
from ctx_handover.tests.fakes import planning_fake

ROOT = Path(__file__).resolve().parents[2]


def _real_model_change() -> dict:
    row = {"model": "qwen3:32b", "model_config": "{}", "billing_provider": "ollama"}
    target = {"model": {"default": "gpt-x", "provider": "openrouter"}}
    _fields, change = session_import.map_model(row, "auto", None, target, lambda name, cfg: False)
    assert isinstance(change, dict)
    return change


def test_fake_model_change_has_the_keys_of_map_model():
    real = _real_model_change()
    fake = planning_fake.model_change()
    assert set(fake) == set(real)
    assert set(fake["from"]) == set(real["from"]) and set(fake["to"]) == set(real["to"])


def test_no_changed_key_in_the_d_planning_fakes_and_r25_tests():
    found = []
    for rel in ("fakes/planning_fake.py", "unit/D/test_plan_r25.py"):
        tree = ast.parse((ROOT / rel).read_text(encoding="utf-8"))
        for node in ast.walk(tree):
            if isinstance(node, ast.Dict):
                keys = [k for k in node.keys if isinstance(k, ast.Constant) and k.value == "changed"]
                found += [f"{rel}:{k.lineno}" for k in keys]
            elif isinstance(node, ast.Call):
                found += [f"{rel}:{kw.value.lineno}" for kw in node.keywords if kw.arg == "changed"]
    assert found == []
