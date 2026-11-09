"""Cluster F — ``register()`` (R12, DESIGN §2.1 consequence 2, §14.5; TESTPLAN §2.7)."""

from __future__ import annotations

import builtins
import io
import os
import socket
import sqlite3
import subprocess
import threading

import hermes_yaml

import ctx_handover
from ctx_handover.engine import agent_tool, commands
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx

from .test_surfaces import PLUGIN_ROOT


def _manifest() -> dict:
    return hermes_yaml.safe_load((PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8"))


def test_register_declares_what_manifest_says():
    """Registrations equal ``provides_tools``/``provides_hooks`` (incl. ``post_compaction``); ``/handover`` takes
    free text; the CLI command exists where Hermes offers it."""
    ctx = FakeCtx()
    ctx_handover.register(ctx)
    manifest = _manifest()
    assert sorted(ctx.tools) == sorted(manifest["provides_tools"])
    assert sorted(ctx.hooks) == sorted(manifest["provides_hooks"])
    assert all(len(cbs) == 1 for cbs in ctx.hooks.values())
    cmd = ctx.commands["handover"]
    assert cmd["argument_mode"] == "text" and cmd["args_hint"].startswith("to|take|export|import")
    assert ctx.tools["handover"]["toolset"] == "ctx_handover"
    assert ctx.tools["handover"]["schema"] is agent_tool.TOOL_SCHEMA
    assert "handover" in ctx.cli_commands
    assert ctx.cli_commands["handover"]["setup_fn"] is commands.cli_setup


def test_register_without_cli_registration():
    """Under ``plugins.isolation: host`` there is no CLI registration; register() still succeeds."""

    class HostCtx(FakeCtx):
        register_cli_command = None  # type: ignore[assignment]

    ctx = HostCtx()
    ctx_handover.register(ctx)
    assert "handover" in ctx.commands and ctx.cli_commands == {}


def test_register_does_no_io(monkeypatch, fake_host):
    """★ R12: ``register()`` opens no file, socket, thread, subprocess or database and calls no Hermes function —
    it runs in every CLI process and inside ``hermes plugins validate``.

    Red proof: mutant = ``hostcompat.plugin_dir()`` added to ``register()`` (eager data dir) →
    ``AssertionError: I/O in register(): os.mkdir`` / ``assert 'I/O in register(): os.mkdir' is None``.
    """
    ctx = FakeCtx()
    fake_host.calls.clear()

    def forbid(name):
        def blocked(*a, **k):
            raise AssertionError(f"I/O in register(): {name}")

        return blocked

    error = None
    with monkeypatch.context() as m:  # patches end before pytest builds a report
        for target, attr in (
            (builtins, "open"),
            (io, "open"),
            (os, "open"),
            (os, "mkdir"),
            (os, "makedirs"),
            (os, "chmod"),
            (os, "stat"),
            (socket, "socket"),
            (sqlite3, "connect"),
            (subprocess, "Popen"),
            (threading.Thread, "start"),
        ):
            m.setattr(target, attr, forbid(f"{getattr(target, '__name__', target)}.{attr}"))
        try:
            ctx_handover.register(ctx)
        except AssertionError as exc:
            error = str(exc)
    assert error is None, error
    assert fake_host.calls == []
    assert set(ctx.hooks) == {"pre_llm_call", "pre_tool_call", "post_llm_call", "post_compaction"}


def test_one_plugin_per_ctx():
    ctx = FakeCtx()
    assert commands.plugin_for(ctx) is commands.plugin_for(ctx)
    assert commands.plugin_for(FakeCtx()) is not commands.plugin_for(ctx)
    assert commands.slash_handler(ctx) == commands.plugin_for(ctx).slash
