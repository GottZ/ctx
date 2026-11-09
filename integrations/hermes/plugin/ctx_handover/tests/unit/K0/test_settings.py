"""K0: ``engine/settings.py`` beyond the layer-R spec (``test_l2r_settings.py``) — contract V22 and its addendum
(non-finite and broken numbers ⇒ schema default + warning), the API fixed in ``reports/L2/g-k0-settings-api.md``."""

from __future__ import annotations

import logging
import math
from pathlib import Path

import pytest

from ctx_handover.engine import settings
from ctx_handover.tests.conftest import basetemp_lock_path, require_upstream

PLUGIN_ROOT = Path(__file__).resolve().parents[3]


def _config(raw: object) -> dict:
    return {"plugins": {"entries": {"ctx_handover": {"enabled": True, "settings": raw}}}}


def test_schema_mirrors_plugin_yaml_types_choices_and_secret_envs():
    """★ ``SCHEMA`` is ``plugin.yaml`` ``config_schema`` key by key: type, default (env for secrets), choices —
    the R-test pins only the defaults."""
    require_upstream()
    import hermes_yaml

    manifest = hermes_yaml.safe_load((PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8"))["config_schema"]
    assert list(settings.SCHEMA) == list(manifest)
    for key, spec in manifest.items():
        mirror = settings.SCHEMA[key]
        assert mirror["type"] == spec["type"], key
        assert list(mirror.get("choices") or ()) == list(spec.get("choices") or ()), key
        if spec["type"] == "secret":
            assert mirror["env"] == spec["env"] == settings.SECRET_ENVS[key] and "default" not in mirror, key
        else:
            assert mirror["default"] == spec["default"], key
    assert set(settings.SECRET_ENVS) == {"peer_token", "peer_password"}


def test_constants_are_read_only_and_normalize_hands_out_copies():
    """★ ``DEFAULTS``/``SCHEMA`` cannot be rebound; every container in a result is a fresh copy."""
    with pytest.raises(TypeError):
        settings.DEFAULTS["refine"] = "off"  # type: ignore[index]
    with pytest.raises(TypeError):
        settings.SCHEMA["refine"]["default"] = "off"  # type: ignore[index]
    one, two = settings.normalize(None), settings.normalize(None)
    for key in ("allow_roots", "export_roots", "peer_extra_headers", "extra_ignore", "tool_probe"):
        assert one[key] is not two[key] and one[key] is not settings.DEFAULTS[key], key
    kept = settings.normalize({"allow_roots": ["~/a"]})
    kept["allow_roots"].append("/x")
    assert settings.normalize({"allow_roots": ["~/a"]})["allow_roots"] == ["~/a"]


@pytest.mark.parametrize("raw", [None, "", "  ", "null", "None", "~"])
def test_unset_values_are_the_default_for_every_type(raw):
    """★ ``None``, a blank text and the YAML null words (``hermes config set … null``) are the default — for a
    boolean too (an empty value is no ``False``)."""
    for key in ("include_hermes_files", "refine", "listen_url", "workspace_root", "max_files", "allow_roots"):
        assert settings.normalize({key: raw})[key] == settings.DEFAULTS[key], (key, raw)
    assert settings.problems({"include_hermes_files": raw}) == []


def test_bool_words_numbers_and_rejects():
    """★ 0/1 (``hermes config set … 1`` stores an int) and trimmed words are booleans; 2 or a list is not."""
    key = "allow_agent_start"
    assert settings.normalize({key: 1})[key] is True
    assert settings.normalize({key: 0})[key] is False
    assert settings.normalize({key: " YES "})[key] is True
    assert settings.normalize({key: 2})[key] is False
    assert settings.normalize({"include_hermes_files": ["no"]})["include_hermes_files"] is True


def test_word_settings_trim_and_fold_to_the_canonical_word():
    """★ ``" Off "`` and ``"LLM"`` are the words ``off`` and ``llm``; a non-text is the default."""
    out = settings.normalize({"refine": " Off ", "extra_paths": "AUTO", "peer_gate": "Bearer", "inbound": 1.0})
    assert (out["refine"], out["extra_paths"], out["peer_gate"]) == ("off", "auto", "bearer")
    assert out["inbound"] == "auto"


def test_plain_text_settings_take_text_only():
    """★ A text setting without choices keeps any non-blank text as given; a number is the default (quote it)."""
    out = settings.normalize({"workspace_root": " ~/w ", "ctx_category": 2026, "peer_username": True})
    assert out["workspace_root"] == " ~/w "
    assert out["ctx_category"] == "handover" and out["peer_username"] == ""


def test_whole_numbers_floats_and_zero():
    """★ An int setting takes a whole float (``7.0``) as int and refuses a fraction; a float setting returns a
    float; 0 is allowed except for ``POSITIVE_KEYS``; an int too large for a float is no float setting."""
    out = settings.normalize(
        {"max_files": 7.0, "max_file_mb": 0.5, "max_total_gb": 3, "confirm_above_mb": 0, "block_cache_days": 0}
    )
    assert out["max_files"] == 7 and type(out["max_files"]) is int
    assert out["max_file_mb"] == 100
    assert out["max_total_gb"] == 3.0 and type(out["max_total_gb"]) is float
    assert out["confirm_above_mb"] == 0 and out["block_cache_days"] == 0
    for key in settings.POSITIVE_KEYS:
        assert settings.normalize({key: 0})[key] == settings.DEFAULTS[key], key
        assert settings.normalize({key: 1})[key] == 1, key
    assert settings.normalize({"max_total_gb": 10**400})["max_total_gb"] == 10.0
    assert settings.normalize({"max_files": 10**400})["max_files"] == 10**400
    assert settings.normalize({"max_files": -0.0})["max_files"] == 0


def test_lists_and_headers():
    """★ A tuple is a list, ``[]`` stays ``[]`` (an explicit empty list is honoured, fail-closed for
    ``allow_roots``), one text is no list; headers keep only ``Header-Name: ENV_NAME`` entries."""
    assert settings.normalize({"extra_ignore": ("*.log", " ", None)})["extra_ignore"] == ["*.log"]
    assert settings.normalize({"allow_roots": []})["allow_roots"] == []
    assert settings.normalize({"tool_probe": "node"})["tool_probe"] == []
    headers = {"CF-Access-Client-Secret": "CF_SECRET", "bad header": "X", "X-Ok": "not an env name", 3: "Y"}
    assert settings.normalize({"peer_extra_headers": headers})["peer_extra_headers"] == {
        "CF-Access-Client-Secret": "CF_SECRET"
    }
    assert settings.normalize({"peer_extra_headers": ["X-A"]})["peer_extra_headers"] == {}


def test_unknown_and_secret_keys_never_reach_the_result():
    """★ The result has exactly the ``DEFAULTS`` keys: a secret written into settings is dropped (and named as a
    problem without its value), an unknown key is dropped silently."""
    out = settings.normalize({"peer_token": "tok-SECRET-1", "max_jobs_total": 9, "refine": "llm"})
    assert set(out) == set(settings.DEFAULTS) and out["refine"] == "llm"
    found = settings.problems({"peer_token": "tok-SECRET-1", "max_jobs_total": 9})
    assert len(found) == 1 and "CTX_HANDOVER_PEER_TOKEN" in found[0] and "SECRET-1" not in found[0]


def test_problems_name_key_reason_default_and_remedy_without_secret_values():
    """★ One sentence per rejected value in schema order, each naming the key, the default used and the
    ``hermes config set`` remedy; header values are never quoted (a misplaced value may be a credential)."""
    raw = {
        "refine": "turbo",
        "max_total_gb": math.inf,
        "allow_roots": ["~/a", 3],
        "peer_extra_headers": {"X-A": "sk-live-SECRET.value"},
        "lease_ttl_s": "120",
    }
    found = settings.problems(raw)
    keys = [text.split(" ", 1)[0].rstrip(":") for text in found]
    assert keys == ["peer_extra_headers", "allow_roots", "max_total_gb", "lease_ttl_s", "refine"]
    by_key = dict(zip(keys, found, strict=True))
    assert "inf" in by_key["max_total_gb"] and "10.0" in by_key["max_total_gb"]
    assert "'turbo'" in by_key["refine"] and "'auto'" in by_key["refine"]
    assert "3" in by_key["allow_roots"] and "'120'" in by_key["lease_ttl_s"]
    assert all("hermes config set plugins.entries.ctx_handover.settings." in text for text in found)
    assert not any("SECRET" in text for text in found)
    assert settings.problems(None) == [] and settings.problems(settings.DEFAULTS) == []
    assert settings.problems(["refine"]) and settings.normalize(["refine"]) == settings.normalize(None)  # type: ignore[arg-type]


def test_normalize_warns_once_per_problem_and_process(caplog):
    """★ The addendum's warning: a rejected value is logged on ``ctx_handover.settings`` — once, not on every
    reload of the settings."""
    raw = {"max_extra_dir_mb": -7171, "refine": "warn-once-probe"}
    with caplog.at_level(logging.WARNING, logger="ctx_handover.settings"):
        settings.normalize(raw)
        settings.normalize(raw)
        settings.from_config(_config(raw))
    texts = [r.getMessage() for r in caplog.records if r.name == "ctx_handover.settings"]
    assert sum("-7171" in t for t in texts) == 1
    assert sum("warn-once-probe" in t for t in texts) == 1
    caplog.clear()
    with caplog.at_level(logging.WARNING, logger="ctx_handover.settings"):
        assert settings.problems({"refine": "silent-probe"})
        settings.coerce("refine", "silent-probe")
    assert not [r for r in caplog.records if r.name == "ctx_handover.settings"]


def test_raw_settings_and_from_config():
    """★ ``raw_settings`` returns the entry as given (copy) or ``{}``; ``from_config`` normalises it."""
    raw = {"refine": False, "other": 1}
    got = settings.raw_settings(_config(raw))
    assert got == raw and got is not raw
    for broken in (None, {}, {"plugins": None}, {"plugins": {"entries": []}}, _config(["refine"]), _config(None)):
        assert settings.raw_settings(broken) == {}, broken
        assert settings.from_config(broken) == settings.normalize(None), broken
    assert settings.from_config(_config(raw))["refine"] == "off"


def test_coerce_flag_and_positive_int():
    """★ ``coerce`` is one key of ``normalize`` (``KeyError`` for secret and unknown keys); ``flag`` and
    ``positive_int`` read values outside the schema."""
    assert settings.coerce("inbound", False) == "off" and settings.coerce("inbound", None) == "auto"
    for key in ("peer_token", "max_jobs_total"):
        with pytest.raises(KeyError):
            settings.coerce(key, 1)
    assert settings.flag("on") is True and settings.flag("maybe") is False and settings.flag(None, True) is True
    assert settings.flag(0, True) is False
    assert settings.positive_int(4, 2) == 4 and settings.positive_int(3.0, 2) == 3
    for bad in (0, -1, 1.5, True, "4", math.inf, math.nan, None, [4]):
        assert settings.positive_int(bad, 2) == 2, bad
    assert settings.positive_int(10**400, 2) == 10**400


def test_basetemp_lock_sits_beside_the_resolved_directory(tmp_path):
    """★ V32: the lock file of a base temp directory lies next to it (pytest empties the directory itself), and
    two spellings of one directory share one lock."""
    real = (tmp_path / "real").resolve()
    real.mkdir()
    (tmp_path / "link").symlink_to(real)
    assert basetemp_lock_path(real / "bt") == real / ".bt.pytest-lock"
    assert basetemp_lock_path(tmp_path / "link" / "bt") == basetemp_lock_path(real / "bt")
