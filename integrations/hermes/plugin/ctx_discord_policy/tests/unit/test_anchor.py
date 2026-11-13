"""S1: one runtime anchor per process, per-profile arming, version conflicts, fail-closed registration."""

from __future__ import annotations

import json
import logging
import os
import re
import shutil
import stat
import sys

import pytest
from conftest import ANCHOR, ROOT, load_entry
from fakes.ctx import FakeCtx


def _register_in(anchor, entry, home, profile, monkeypatch, **ctx_kwargs):
    home.mkdir(parents=True, exist_ok=True)
    monkeypatch.setenv("HERMES_HOME", str(home))
    ctx = FakeCtx(profile, **ctx_kwargs)
    entry.register(ctx)
    return ctx


def test_two_profile_instances_share_one_anchor_and_both_arm(anchor, monkeypatch):
    launch = anchor.entry
    routed = load_entry(name="hermes_plugins_test.ctx_discord_policy__home_routed")
    assert launch is not routed  # Hermes' per-profile module split
    ctx_a = _register_in(anchor, launch, anchor.tmp / "root", "default", monkeypatch)
    ctx_b = _register_in(anchor, routed, anchor.tmp / "root" / "profiles" / "routed", "routed", monkeypatch)
    assert sys.modules[ANCHOR] is anchor.package
    runtime = anchor.RUNTIME
    homes = set(runtime.armed.snapshot())
    assert homes == {anchor.mod("hermes").home_key(anchor.tmp / "root"),
                     anchor.mod("hermes").home_key(anchor.tmp / "root" / "profiles" / "routed")}
    # Both profiles registered the very same callables of the shared runtime.
    assert [cb for _, cb in ctx_a.hooks] == [cb for _, cb in ctx_b.hooks]
    assert {name for name, _ in ctx_a.hooks} == {
        "pre_llm_call", "pre_tool_call", "on_session_end", "on_session_reset", "on_session_finalize"}


def test_second_plugin_version_in_process_registers_nothing(anchor, monkeypatch, tmp_path, caplog):
    _register_in(anchor, anchor.entry, anchor.tmp / "root", "default", monkeypatch)
    other_dir = tmp_path / "other-version"
    shutil.copytree(ROOT, other_dir, ignore=shutil.ignore_patterns(".git", "tests", "__pycache__"))
    entry_file = other_dir / "__init__.py"
    entry_file.write_text(re.sub(r'PLUGIN_VERSION = "[^"]+"', 'PLUGIN_VERSION = "3.9.9"',
                                 entry_file.read_text(encoding="utf-8")), encoding="utf-8")
    other = load_entry(other_dir, name="hermes_plugins_test.ctx_discord_policy__home_other")
    other_home = anchor.tmp / "root" / "profiles" / "other"
    with caplog.at_level(logging.ERROR, logger="ctx_discord_policy"):
        ctx = _register_in(anchor, other, other_home, "other", monkeypatch)
    assert ctx.hooks == []  # registers nothing
    assert not anchor.RUNTIME.armed.is_armed(anchor.mod("hermes").home_key(other_home))
    assert "restart" in caplog.text.lower()
    refused = anchor.RUNTIME.status.snapshot()["refused"]
    assert refused and refused[-1]["version"] == "3.9.9"
    assert sys.modules[ANCHOR] is anchor.package  # the running anchor stays


def test_runtime_api_mismatch_also_refuses(anchor, monkeypatch):
    anchor.package.RUNTIME_API = 99
    ctx = _register_in(anchor, anchor.entry, anchor.tmp / "root", "default", monkeypatch)
    assert ctx.hooks == []
    assert anchor.RUNTIME.armed.snapshot() == {}


def test_hook_registration_failure_fails_closed(anchor, monkeypatch):
    # Generalised from 2.0.6 test_register_fail_closed: a partial hook set never arms the profile.
    with pytest.raises(RuntimeError):
        _register_in(anchor, anchor.entry, anchor.tmp / "root", "default", monkeypatch, fail_hook_at=2)
    assert anchor.RUNTIME.armed.snapshot() == {}
    result = anchor.RUNTIME.status.get("A8[default]")
    assert result is not None and result.outcome.value == "off"


def test_host_isolation_registers_nothing(anchor, monkeypatch):
    monkeypatch.setattr(anchor.mod("hermes"), "isolation_mode", lambda: ("host", "test"))
    ctx = _register_in(anchor, anchor.entry, anchor.tmp / "root", "default", monkeypatch)
    assert ctx.hooks == []
    assert anchor.RUNTIME.armed.snapshot() == {}
    assert anchor.RUNTIME.status.get("A1[default]").outcome.value == "off"


def test_unload_disarms_the_profile(anchor, monkeypatch):
    ctx = _register_in(anchor, anchor.entry, anchor.tmp / "root", "default", monkeypatch)
    key = anchor.mod("hermes").home_key(anchor.tmp / "root")
    assert anchor.RUNTIME.armed.is_armed(key)
    ctx.run_unload()
    assert not anchor.RUNTIME.armed.is_armed(key)


def test_status_file_is_private_and_reports_changes(anchor, monkeypatch, caplog):
    home = anchor.tmp / "root"
    _register_in(anchor, anchor.entry, home, "default", monkeypatch)
    path = home / "plugin-data" / "ctx_discord_policy" / "status.json"
    assert not path.exists()  # register() never writes it (CLI processes load plugins too)
    anchor.bootstrap.write_status(home)
    data = json.loads(path.read_text(encoding="utf-8"))
    assert data["plugin_version"] == "3.0.0"
    assert data["interventions"]["A8[default]"]["outcome"] == "armed"
    if os.name == "posix":
        assert stat.S_IMODE(path.stat().st_mode) == 0o600
        assert stat.S_IMODE(path.parent.stat().st_mode) == 0o700
    # A degraded outcome on the next start is reported against the last status file.
    monkeypatch.setattr(anchor.mod("hermes"), "isolation_mode", lambda: ("host", "test"))
    with caplog.at_level(logging.WARNING, logger="ctx_discord_policy"):
        _register_in(anchor, anchor.entry, home, "default", monkeypatch)
        anchor.bootstrap.write_status(home)
    assert "A1[default]: ok -> off" in caplog.text


def test_anchor_survives_profile_module_eviction(anchor, monkeypatch):
    """Hermes evicts ``hermes_plugins.<slug>`` on force reload; the anchor (and its routes) must stay."""
    _register_in(anchor, anchor.entry, anchor.tmp / "root", "default", monkeypatch)
    sys.modules.pop("hermes_plugins_test.ctx_discord_policy", None)
    fresh = load_entry()
    assert fresh.load_anchor(ROOT) is anchor.package
