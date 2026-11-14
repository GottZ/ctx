"""S2 contract: operators are read through Hermes' own config loader, from the bot owner's home, and an
edit of config.yaml applies to the next read without a restart."""

from __future__ import annotations

import os
import sys
import time

import pytest
from fakes.hermes_homes import ANCHOR, PLUGIN, Homes, _evict, load_in

OP_A = "900000000000000011"
OP_B = "900000000000000012"
OP_ROUTED = "900000000000000013"


def _config(home, operators):
    ops = ", ".join(f'"{o}"' for o in operators)
    text = (f"plugins:\n  enabled: [{PLUGIN}]\n  entries:\n    {PLUGIN}:\n      settings:\n"
            f"        operators: [{ops}]\n")
    path = home / "config.yaml"
    before = path.stat().st_mtime_ns if path.exists() else 0
    path.write_text(text, encoding="utf-8")
    # Make the edit visible to a signature cache even on coarse-mtime filesystems.
    os.utime(path, ns=(max(time.time_ns(), before + 1_000_000), max(time.time_ns(), before + 1_000_000)))


@pytest.fixture
def homes(hermes, tmp_path, monkeypatch):
    _evict(ANCHOR)
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "root"))
    built = Homes(tmp_path)
    yield built
    _evict(ANCHOR)


def test_operator_edit_applies_without_restart_and_follows_the_owner_home(homes):
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    routed = homes.profile("routed")
    _config(homes.root, [OP_A])
    _config(routed, [OP_ROUTED])
    load_in(homes.root)
    load_in(routed)
    config = sys.modules[f"{ANCHOR}.config"]
    assert config.operators(homes.root) == {OP_A}

    _config(homes.root, [OP_B])
    assert config.operators(homes.root) == {OP_B}  # same process, no reload

    # A hook runs under the routed profile's scope; the owner's list still decides.
    token = set_hermes_home_override(str(routed))
    try:
        assert config.operators(homes.root) == {OP_B}
        assert config.operators(routed) == {OP_ROUTED}
    finally:
        reset_hermes_home_override(token)


def test_unreadable_config_yields_no_operator(homes):
    """Hermes serves its last-known-good copy for a broken config.yaml; an operator removed in that very
    edit must not keep authority from it: a broken config means no operator at all."""
    _config(homes.root, [OP_A, OP_B])
    load_in(homes.root)
    config = sys.modules[f"{ANCHOR}.config"]
    assert config.operators(homes.root) == {OP_A, OP_B}
    path = homes.root / "config.yaml"
    path.write_text(f"plugins:\n  enabled: [{PLUGIN}]\n  entries: [unclosed\n", encoding="utf-8")
    os.utime(path, ns=(time.time_ns() + 2_000_000, time.time_ns() + 2_000_000))
    assert config.operators(homes.root) == frozenset()


# ── #268: what Hermes' settings form shows and saves for these settings ──────────────────────────────

FRAME = ("\n\nYou speak for {operator_label}: keep it short.\n# not a comment\n  {braces} [x] key: value, \"quoted\"\n"
         "\ttab and trailing space \n\n")


def _js_string(value) -> str:
    """``String(value)`` in the Desktop form (apps/desktop/src/app/settings/plugin-settings-form.tsx
    ``INITIAL_TEXT.string``): an array joins its items with commas. Read, not executed."""
    if value is None:
        return ""
    if isinstance(value, (list, tuple)):
        return ",".join(_js_string(v) for v in value)
    return str(value)


def _hermes_fields(home):
    from hermes_cli.plugins_settings import plugin_settings_fields
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    token = set_hermes_home_override(str(home))
    try:
        return {f["key"]: f for f in plugin_settings_fields(PLUGIN, homes_plugin_dir(home))}
    finally:
        reset_hermes_home_override(token)


def homes_plugin_dir(home):
    return home / "plugins" / PLUGIN


def _hermes_save(home, values):
    from hermes_cli.plugins_settings import save_plugin_settings
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    token = set_hermes_home_override(str(home))
    try:
        written = save_plugin_settings(PLUGIN, homes_plugin_dir(home), values)
    finally:
        reset_hermes_home_override(token)
    path = home / "config.yaml"
    os.utime(path, ns=(time.time_ns() + 2_000_000, time.time_ns() + 2_000_000))
    return written


def test_hermes_form_edits_the_list_settings_as_text(homes):
    """Hermes renders ``list`` as a JSON editor; the two list settings must reach it as text fields."""
    fields = _hermes_fields(homes.root)
    assert fields["operators"]["type"] == "string"
    assert fields["locked_profiles"]["type"] == "string"
    assert fields["frame_prompt"]["type"] == "string"        # Hermes' form has no multi-line text field


def test_an_existing_yaml_list_survives_the_form_unchanged_and_edited(homes):
    """Round trip: a YAML list (every install today) shown in Hermes' form, saved unchanged (the form sends
    only changed fields, plugin-settings-form.tsx collectChanges: nothing is written), then edited."""
    _config(homes.root, [OP_A, OP_B])
    load_in(homes.root)
    config = sys.modules[f"{ANCHOR}.config"]
    shown = _js_string(_hermes_fields(homes.root)["operators"]["value"])
    assert shown == f"{OP_A},{OP_B}"
    assert config.operators(homes.root) == {OP_A, OP_B}       # unchanged: nothing saved, same operators

    _hermes_save(homes.root, {"operators": shown + f", {OP_ROUTED} "})
    from hermes_cli.config import load_config

    stored = load_config()["plugins"]["entries"][PLUGIN]["settings"]["operators"]
    assert isinstance(stored, str)
    assert config.operators(homes.root) == {OP_A, OP_B, OP_ROUTED}
    assert _js_string(_hermes_fields(homes.root)["operators"]["value"]) == shown + f", {OP_ROUTED} "

    _hermes_save(homes.root, {"operators": OP_A})              # one ID, as the form saves it
    assert config.operators(homes.root) == {OP_A}
    _hermes_save(homes.root, {"operators": ""})                # cleared
    assert config.operators(homes.root) == frozenset()


def test_locked_profiles_saved_as_text_reach_the_policy(homes):
    load_in(homes.root)
    config = sys.modules[f"{ANCHOR}.config"]
    _hermes_save(homes.root, {"locked_profiles": " community, Beta "})
    assert config.locked_profiles(homes.root) == {"community", "beta"}
    _hermes_save(homes.root, {"locked_profiles": "community; beta"})
    assert config.locked_profiles(homes.root) is None          # doubtful text: every profile counts as locked


def test_a_multi_line_frame_goes_through_hermes_writer_unchanged(homes):
    """What the plugin's own frame editor saves (through the same Hermes writer) comes back byte for byte:
    line breaks, leading and trailing blank lines, YAML characters and placeholders."""
    load_in(homes.root)
    config = sys.modules[f"{ANCHOR}.config"]
    _hermes_save(homes.root, {"frame_prompt": FRAME})
    assert config.setting(homes.root, "frame_prompt") == FRAME
