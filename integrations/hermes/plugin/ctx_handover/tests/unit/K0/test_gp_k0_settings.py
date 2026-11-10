"""K0 round G′: word settings read the YAML boolean words of every spelling (integrator decision,
correcting the layer-R test for ``inbound``).

Rules under test, for a word setting (``str`` with ``choices``) whose choices contain ``off``:
``no``/``false``/``off``/``0`` — any case, quoted or not, the YAML boolean ``False``, the integer 0 — are ``off``;
``yes``/``true``/``on``/``1`` are ``on`` when the choices hold ``on``, else the schema default (no problem entry);
anything else is the schema default plus a ``settings.problems`` entry — except ``inbound``, where an unknown value
is ``off`` (fail-closed, the layer-2 security fix). ``None`` stays the schema default everywhere.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine import settings
from ctx_handover.tests.conftest import require_upstream

WORDS_WITH_OFF = ("inbound", "refine", "extra_paths", "accept_offers", "ctx_persist")
WORDS_WITHOUT_OFF = ("peer_gate", "model_mapping")
OFF_SPELLINGS = ("no", "No", "NO", " no ", "false", "False", "FALSE", "off", "OFF", "0", " 0 ", False, 0)
ON_SPELLINGS = ("yes", "Yes", "YES", "true", "TRUE", "on", "On", "1", True, 1)


def _yaml(text: str) -> dict:
    require_upstream()
    import hermes_yaml  # the core's YAML loader, as Hermes reads config.yaml

    return hermes_yaml.safe_load(text)


@pytest.mark.parametrize("key", WORDS_WITH_OFF)
def test_every_spelling_of_no_is_off(key):
    """★ ``ctx_persist: "no"`` (and every other off-word, quoted or bare, 0) is ``off`` — not the default.

    Red before the fix: ``AssertionError: assert 'auto' == 'off'`` for ``"no"`` (the word was outside the choices,
    so the default ``auto`` was used and recording stayed on)."""
    for raw in OFF_SPELLINGS:
        assert settings.normalize({key: raw})[key] == "off", (key, raw)
        assert settings.coerce(key, raw) == "off", (key, raw)
        assert settings.problems({key: raw}) == [], (key, raw)


@pytest.mark.parametrize("key", WORDS_WITH_OFF)
def test_every_spelling_of_yes_is_on_or_the_default(key):
    """★ The on-words are ``on`` where the choices know ``on`` (``inbound``), else the schema default — silently:
    "yes" is a valid way of saying "not off".

    Red before the fix: ``inbound: "yes"`` was ``auto`` (unknown word ⇒ default), and the words produced a problem
    entry."""
    expected = "on" if "on" in settings.SCHEMA[key]["choices"] else settings.DEFAULTS[key]
    for raw in ON_SPELLINGS:
        assert settings.normalize({key: raw})[key] == expected, (key, raw)
        assert settings.problems({key: raw}) == [], (key, raw)


@pytest.mark.parametrize("key", WORDS_WITH_OFF)
def test_unset_is_the_default_never_off(key):
    """``None`` and the null words stay the schema default (an empty value is no ``off``)."""
    for raw in (None, "", "null", "~"):
        assert settings.normalize({key: raw})[key] == settings.DEFAULTS[key], (key, raw)
        assert settings.problems({key: raw}) == []


@pytest.mark.parametrize("key", [k for k in WORDS_WITH_OFF if k != "inbound"])
def test_unknown_word_is_the_default_with_a_problem(key):
    """★ An unknown word, a wrong type or another number is the schema default plus one problem sentence."""
    for raw in ("nonsense", "of", 2, 1.0, ["off"], {"a": 1}):
        assert settings.normalize({key: raw})[key] == settings.DEFAULTS[key], (key, raw)
        found = settings.problems({key: raw})
        assert len(found) == 1 and found[0].startswith(f"{key} "), (key, raw, found)


def test_unknown_inbound_is_off_fail_closed_with_a_problem():
    """★ ``inbound`` with an unknown value is ``off`` (fail-closed: the default ``auto`` would answer paired peers),
    and ``problems`` says so; ``None`` is still ``auto``.

    Red before the fix: ``AssertionError: assert 'auto' == 'off'`` for ``"nonsense"`` (the layer-R rule)."""
    for raw in ("nonsense", "of", "enabled", 2, 1.0, -1, ["on"], {"on": True}):
        assert settings.normalize({"inbound": raw})["inbound"] == "off", raw
        assert settings.coerce("inbound", raw) == "off", raw
        found = settings.problems({"inbound": raw})
        assert len(found) == 1 and found[0].startswith("inbound "), (raw, found)
        assert "off" in found[0] and "fail-closed" in found[0], found
    assert settings.coerce("inbound", None) == "auto"
    assert settings.normalize({"inbound": "auto"})["inbound"] == "auto"


@pytest.mark.parametrize("key", WORDS_WITHOUT_OFF)
def test_words_without_off_keep_their_default_for_boolean_words(key):
    """Word settings without ``off`` (``peer_gate``, ``model_mapping``) take only their own words; boolean words
    are the default plus a problem (no invented word)."""
    for raw in (False, True, "no", "yes", "off", "0", 0, 1):
        assert settings.normalize({key: raw})[key] == settings.DEFAULTS[key], (key, raw)
        assert len(settings.problems({key: raw})) == 1, (key, raw)


def test_words_as_hermes_reads_them_from_config_yaml():
    """★ The settings as ``hermes_yaml`` loads them from ``config.yaml``: bare ``no``/``off``/``false`` (YAML
    booleans), quoted ``"no"``/``'0'``, bare ``0`` — all ``off``; bare ``yes`` is ``on`` for ``inbound``.

    Red before the fix: ``ctx_persist: "no"`` read as ``auto``."""
    loaded = _yaml(
        """
inbound: 'garbage'
refine: "no"
extra_paths: '0'
accept_offers: 0
ctx_persist: "no"
peer_gate: off
"""
    )
    out = settings.normalize(loaded)
    assert (out["refine"], out["extra_paths"], out["accept_offers"], out["ctx_persist"]) == ("off",) * 4
    assert out["inbound"] == "off" and out["peer_gate"] == "session_token"
    for text in ("no", "NO", "off", "Off", "false", '"no"', "'FALSE'", "0", "'0'"):
        assert settings.normalize(_yaml(f"ctx_persist: {text}"))["ctx_persist"] == "off", text
    for text in ("yes", "on", "true", '"yes"', "1"):
        assert settings.normalize(_yaml(f"inbound: {text}"))["inbound"] == "on", text
        assert settings.normalize(_yaml(f"ctx_persist: {text}"))["ctx_persist"] == "auto", text
