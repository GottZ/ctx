"""Layer R (red) for V22 (A part) of the layer-2 contract: the composition root ``jobs`` reads every plugin
setting through the one K0 normaliser ``settings.normalize`` — ``None`` is the schema default, a number that is not
finite or negative is the schema default (``plugin.yaml``), a quoted word is no truthy string."""

from __future__ import annotations

import importlib
import math

import pytest

from ctx_handover.engine import jobs

MIB, GIB = 1024**2, 1024**3


def _config(settings: dict) -> dict:
    return {"plugins": {"entries": {"ctx_handover": {"enabled": True, "settings": settings}}}}


def _options(settings: dict):
    return jobs.local_options(jobs.plugin_settings(_config(settings)))


def test_none_is_the_schema_default_not_false():
    """``include_hermes_files: null`` (or an empty value in ``config.yaml``) keeps the default ``true``; the other
    ``None`` values keep theirs."""
    raw = {"include_hermes_files": None, "refine_auto_include": None, "refine": None, "extra_paths": None}
    o = _options(raw)
    assert o.include_hermes_files is True
    assert o.refine_auto_include is False
    assert o.refine == "auto" and o.extra_paths == "ask"
    assert jobs.plugin_settings(_config({"allow_remote_continue": None})).get("allow_remote_continue") is not True


@pytest.mark.parametrize(
    ("key", "value", "field", "want"),
    [
        ("max_total_gb", math.inf, "max_total_bytes", 10 * GIB),
        ("max_total_gb", math.nan, "max_total_bytes", 10 * GIB),
        ("lease_ttl_s", math.nan, "lease_ttl_s", 120),
        ("max_file_mb", -1, "max_file_bytes", 100 * MIB),
        ("confirm_above_mb", -5, "confirm_above_bytes", 500 * MIB),
        ("max_session_mb", "64", "max_session_bytes", 512 * MIB),
    ],
)
def test_numbers_outside_the_schema_fall_back_to_the_default(key, value, field, want):
    """A YAML ``.inf``/``.nan`` or a negative number never reaches ``Options`` (``int(inf)`` raised
    ``OverflowError`` while composing every job; a negative limit refuses everything)."""
    assert getattr(_options({key: value}), field) == want


def test_jobs_takes_its_settings_from_the_one_normaliser(monkeypatch):
    """``jobs.plugin_settings`` is ``settings.normalize`` of the raw mapping (contract V22: one normaliser, used by
    ``jobs`` and ``surfaces``; no second word table)."""
    settings = importlib.import_module("ctx_handover.engine.settings")
    raw = {"refine": False, "include_hermes_files": "no", "max_file_mb": 7}
    seen: list = []
    real = settings.normalize

    def spy(value):
        seen.append(value)
        return real(value)

    monkeypatch.setattr(settings, "normalize", spy)
    got = jobs.plugin_settings(_config(raw))
    assert seen and dict(seen[-1]) == raw
    assert {k: got[k] for k in raw} == {k: real(raw)[k] for k in raw}
