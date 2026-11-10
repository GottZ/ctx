"""Layer R (red) for V22 of the layer-2 contract: one settings normaliser in the new K0 module
``engine/settings.py`` for every plugin setting.

Expected API (fixed here, contract V22): ``settings.DEFAULTS`` — the schema defaults of ``plugin.yaml``
``config_schema`` (secret keys excluded, their values live in ``.env``) — and ``settings.normalize(raw: Mapping |
None) -> dict``: every key of ``DEFAULTS``; YAML boolean words per type (``off``/``False`` and ``on``/``True``, quoted
words alike); ``None`` and any value of the wrong type or outside its choices ⇒ the schema default.
The module is imported inside each test, so a missing module fails every test with ``ModuleNotFoundError``.
"""

from __future__ import annotations

import importlib
import math
from pathlib import Path

import pytest

from ctx_handover.tests.conftest import require_upstream

PLUGIN_ROOT = Path(__file__).resolve().parents[3]

WORDS_WITH_OFF = ("inbound", "refine", "extra_paths", "accept_offers", "ctx_persist")
"""String settings whose choices contain ``off`` (a bare YAML ``off`` arrives as ``False``)."""
BOOLS = ("refine_auto_include", "include_hermes_files", "allow_remote_continue", "allow_agent_start")


def _settings():
    return importlib.import_module("ctx_handover.engine.settings")


def _schema() -> dict:
    require_upstream()
    import hermes_yaml  # the core's YAML loader (test side only; the engine never reads plugin.yaml)

    return hermes_yaml.safe_load((PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8"))["config_schema"]


def test_defaults_are_the_schema_defaults_of_plugin_yaml():
    """``DEFAULTS`` equals ``config_schema`` of ``plugin.yaml`` (every non-secret key with its default);
    ``normalize(None)`` and ``normalize({})`` are exactly these defaults, as a fresh dict."""
    s = _settings()
    want = {k: spec["default"] for k, spec in _schema().items() if spec["type"] != "secret"}
    assert want == s.DEFAULTS
    assert s.normalize(None) == want
    assert s.normalize({}) == want
    out = s.normalize({})
    out["allow_roots"].append("/elsewhere")
    out["refine"] = "llm"
    assert want == s.DEFAULTS, "normalize must not hand out the defaults themselves"


@pytest.mark.parametrize("key", WORDS_WITH_OFF)
def test_word_settings_off_false_quoted_and_none(key):
    """A word setting: bare YAML ``off`` (``False``) and the quoted word ``"off"`` both mean ``off``; ``None`` is
    the schema default, never ``off`` and never ``None``. An unknown word is the schema default — except
    ``inbound``, where it is ``off`` (fail-closed)."""
    s = _settings()
    assert s.normalize({key: False})[key] == "off"
    assert s.normalize({key: "off"})[key] == "off"
    assert s.normalize({key: None})[key] == s.DEFAULTS[key]
    # Specification correction by the integrator (round G′): an unknown ``inbound``
    # word is ``off`` (fail-closed, the layer-2 security fix stays in force), not the default ``auto``.
    unknown = "off" if key == "inbound" else s.DEFAULTS[key]
    assert s.normalize({key: "nonsense"})[key] == unknown


def test_word_setting_on_true_and_words_without_on():
    """``inbound`` has the choice ``on``: bare ``on`` (``True``) and ``"on"`` mean ``on``. A word setting without
    the choice ``on`` reads ``True`` as its default (no invented word)."""
    s = _settings()
    assert s.normalize({"inbound": True})["inbound"] == "on"
    assert s.normalize({"inbound": "on"})["inbound"] == "on"
    for key in ("refine", "extra_paths", "accept_offers", "ctx_persist", "model_mapping"):
        assert s.normalize({key: True})[key] == s.DEFAULTS[key], key
    assert s.normalize({"model_mapping": False})["model_mapping"] == s.DEFAULTS["model_mapping"]


@pytest.mark.parametrize("key", BOOLS)
def test_bool_settings_words_and_none(key):
    """A boolean setting: ``False``/``"off"``/``"false"``/``"no"`` are False, ``True``/``"on"``/``"true"``/
    ``"yes"`` are True (quoted words are not truthy strings); ``None`` and an unknown word are the schema
    default."""
    s = _settings()
    for raw in (False, "off", "false", "no", "Off"):
        assert s.normalize({key: raw})[key] is False, raw
    for raw in (True, "on", "true", "yes"):
        assert s.normalize({key: raw})[key] is True, raw
    assert s.normalize({key: None})[key] is s.DEFAULTS[key]
    assert s.normalize({key: "maybe"})[key] is s.DEFAULTS[key]


def test_values_of_the_wrong_type_fall_back_to_the_schema_default():
    """Numbers must be finite and not negative, never a boolean; lists stay lists of strings; a choice outside
    the list is the default — Hermes only warns at load, so the plugin does not trust ``config.yaml``."""
    s = _settings()
    d = s.DEFAULTS
    bad = {
        "max_file_mb": -1,
        "max_session_mb": True,
        "max_total_gb": math.inf,
        "block_cache_gb": math.nan,
        "lease_ttl_s": "120",
        "allow_roots": "~/hermes-handover",
        "export_roots": None,
        "peer_gate": "cookie",
        "refine": 1,
    }
    out = s.normalize(bad)
    for key in bad:
        assert out[key] == d[key], key
    good = s.normalize({"max_file_mb": 7, "max_total_gb": 1.5, "allow_roots": ["~/a", "", 3]})
    assert good["max_file_mb"] == 7 and good["max_total_gb"] == 1.5
    assert good["allow_roots"] == ["~/a"]
