"""The two list settings (``operators``, ``locked_profiles``) in every form a reader meets (#268).

Hermes' settings form edits them as text (comma-separated), earlier versions and ``hermes config set``
wrote YAML lists, a single ID set on the command line arrives as a number. Every reader goes through
``dsp.config``: ``operators`` fails closed (an entry that is no user ID counts for nobody, an unreadable
value means no operator), ``locked_profiles`` never unlocks a profile on a doubtful value.
"""

from __future__ import annotations

import pytest

A = "123456789012345678"
B = "234567890123456789"


@pytest.fixture
def config(anchor):
    mod = anchor.mod("config")
    yield mod
    mod.set_reader(None)


def _set(config, **values):
    config.set_reader(lambda home, key, default: values.get(key, default))


@pytest.mark.parametrize("raw", [
    f"{A}, {B}",
    f" {A} ,{B} ",
    f",{A},, ,{B},",
    f'["{A}", "{B}"]',
    f'  [{A}, "{B}"]  ',
    [A, B],
    [int(A), f" {B} "],
    (A, B),
])
def test_every_form_names_the_same_operators(config, anchor, raw):
    _set(config, operators=raw)
    assert config.operator_list(anchor.home) == (frozenset({A, B}), True)


def test_a_single_id_as_text_or_number_is_one_operator(config, anchor):
    for raw in (A, f"  {A}  ", int(A), [int(A)]):
        _set(config, operators=raw)
        assert config.operators(anchor.home) == frozenset({A}), raw


@pytest.mark.parametrize("raw", [
    "", "  ", ",", " , ,", "[]", None, [],
])
def test_empty_forms_name_nobody(config, anchor, raw):
    _set(config, operators=raw)
    assert config.operator_list(anchor.home) == (frozenset(), True)


@pytest.mark.parametrize("raw", [
    {"id": A},                     # a mapping
    True, False,                   # YAML booleans
    1.5,                           # a float
    f'["{A}", "{B}"',              # JSON text that does not parse
    f"[{A}, {B}",
    f"{A} {B}",                    # separated by a space: one entry, no ID
    f"{A}; {B}",                   # separated by a semicolon: one entry, no ID
    f'"{A}", "{B}"',               # quoted without brackets: no ID
    [[A], {"id": B}],              # nested structures
    f'[["{A}"], {{"id": "{B}"}}]',
    [True, 1.5, None],
    "abc, 12345, 0123456789012345678",
    f'{{"{A}": 1}}',
])
def test_garbage_never_names_an_operator(config, anchor, raw):
    _set(config, operators=raw)
    assert config.operators(anchor.home) == frozenset()


def test_valid_entries_next_to_garbage_count_and_nothing_else(config, anchor):
    for raw in (f"{A}, not-an-id, {B} x", [A, "not-an-id", [B]], f'["{A}", "x", ["{B}"]]'):
        _set(config, operators=raw)
        assert config.operators(anchor.home) == frozenset({A}), raw


def test_ignored_entries_are_logged(config, anchor, caplog):
    import logging

    caplog.set_level(logging.WARNING, logger="ctx_discord_policy")
    _set(config, operators=f"{A}, bogus-entry-268")
    assert config.operators(anchor.home) == frozenset({A})
    assert any("bogus-entry-268" in r.getMessage() for r in caplog.records)
    _set(config, operators={"unreadable": "mapping-268"})
    assert config.operators(anchor.home) == frozenset()
    assert any("settings.operators" in r.getMessage() and "dict" in r.getMessage() for r in caplog.records)


def test_deeply_nested_json_text_fails_closed(config, anchor):
    """json.loads raises RecursionError (no ValueError) on very deep nesting; it must read as unreadable."""
    deep = "[" * 100_000 + "]" * 100_000
    _set(config, operators=deep, locked_profiles=deep)
    assert config.operator_list(anchor.home) == (frozenset(), True)
    assert config.locked_profiles(anchor.home) is None


# ── locked_profiles ───────────────────────────────────────────────────────────────────────────────────

@pytest.mark.parametrize("raw", [
    "community, beta",
    " Community ,beta, ,",
    '["community", "beta"]',
    ["community", "beta"],
    [" Community ", "BETA"],
])
def test_every_form_locks_the_same_profiles(config, anchor, raw):
    _set(config, locked_profiles=raw)
    assert config.locked_profiles(anchor.home) == frozenset({"community", "beta"})


@pytest.mark.parametrize("raw", ["", " , ", "[]", None, []])
def test_empty_forms_lock_nothing(config, anchor, raw):
    _set(config, locked_profiles=raw)
    assert config.locked_profiles(anchor.home) == frozenset()


@pytest.mark.parametrize("raw", [
    {"beta": True},
    True,
    1.5,
    7,                              # a bare number is no profile name
    '["beta"',                      # JSON text that does not parse
    "beta; community",              # a wrong separator: the entry is no profile name
    "beta, ../x",
    "beta, two words",
    '["beta", 7]',
    '["beta", ["community"]]',
])
def test_a_doubtful_text_or_value_locks_every_profile(config, anchor, raw):
    _set(config, locked_profiles=raw)
    assert config.locked_profiles(anchor.home) is None


def test_a_yaml_list_keeps_ignoring_entries_that_are_no_profile_name(config, anchor):
    """Unchanged rule of the list form: such an entry can't name a profile (Hermes' PROFILE_ID_RE is
    the same pattern), so ignoring it unlocks nothing that is there."""
    _set(config, locked_profiles=["Default", " community ", "../x", 7, ["beta"]])
    assert config.locked_profiles(anchor.home) == frozenset({"default", "community"})


# ── the settings page reads and writes the same form ─────────────────────────────────────────────────

class _Backend:
    def __init__(self, settings):
        self.settings = dict(settings)
        self.writes = []

    def read_settings(self):
        return dict(self.settings)

    def write_settings(self, plugin_dir, values):
        self.writes.append(dict(values))
        self.settings.update(values)
        return list(values)

    def settings_changed(self, written):
        pass


@pytest.mark.parametrize("raw", [f"{A}, {B}", f'["{A}", "{B}"]', [A, B], [int(A), B], f"{A}, junk, {B}"])
def test_the_settings_page_shows_the_operators_the_policy_uses(config, anchor, raw):
    settings_io = anchor.mod("web.settings_io")
    _set(config, operators=raw)
    shown = settings_io.read(_Backend({"operators": raw}))["values"]["operators"]
    assert shown == [A, B]
    assert frozenset(shown) == config.operators(anchor.home)


def test_the_settings_page_writes_the_text_form_hermes_form_accepts(config, anchor):
    """Hermes' writer checks a value against the manifest type; the list fields are text there, so the page
    saves the comma form, and every reader gets the same operators back."""
    settings_io = anchor.mod("web.settings_io")
    backend = _Backend({"operators": [A]})
    state = settings_io.read(backend)
    after = settings_io.write(backend, ".", state["digest"], {"operators": [A, B, A]})
    assert backend.writes == [{"operators": f"{A}, {B}"}]
    assert after["values"]["operators"] == [A, B]
    _set(config, operators=backend.settings["operators"])
    assert config.operators(anchor.home) == frozenset({A, B})
    settings_io.write(backend, ".", after["digest"], {"operators": []})
    assert backend.writes[-1] == {"operators": ""}
    _set(config, operators=backend.settings["operators"])
    assert config.operators(anchor.home) == frozenset()
