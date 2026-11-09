"""Review round rv-f — hand-over W4 of i-f20 (L3-F20): bundle paths typed for ``export``/``import`` were resolved by
the process (``os.path.expanduser`` + the process's working folder). Under ``plugins.isolation: host`` the plugin
host runs with ``cwd = HERMES_HOME`` and its own ``HOME``, and in a gateway the process folder is wherever the gateway
started — so ``/handover export out.ctxh.tar`` wrote into the Hermes home.

Now: ``~`` is the home Hermes' own tools use (``hostcompat.effective_home``); a relative path in a chat is resolved
against the calling session's working folder (its session row, R11) and refused when there is none; in the
terminal (``hermes handover``) it is the terminal's working folder."""

from __future__ import annotations

import os

import pytest

from ctx_handover.engine import hostcompat
from ctx_handover.engine.commands import Plugin
from ctx_handover.engine.model import Mode
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer

from .test_g3_f import _cli

SID = "20261109_101500_aaaaaa"


def _plugin(backend: FakeBackend) -> Plugin:
    from .test_commands import Ctx

    return Plugin(Ctx(), backend_factory=lambda c, p: backend)


def _slash(plugin: Plugin, raw: str) -> str:
    return plugin.slash(raw, session_id=SID, session_key=f"agent:main:local:dm:{SID}", platform="local", source=None)


@pytest.fixture
def host_like(tmp_path, monkeypatch, fake_host):
    """A plugin-host-like process: cwd = HERMES_HOME, ``HOME`` = the profile home under it; the session works in
    ``work``; Hermes' own home (``effective_home``) is the user's."""
    work = tmp_path / "work"
    work.mkdir()
    monkeypatch.chdir(fake_host.hermes_home)
    profile_home = fake_host.hermes_home / "home"
    profile_home.mkdir(exist_ok=True)
    monkeypatch.setenv("HOME", str(profile_home))
    backend = FakeBackend(peers=[make_peer("server")])
    backend.session_cwds = {SID: str(work)}
    return backend, work, fake_host


def _started_bundle(backend: FakeBackend) -> str:
    return str(backend.last("start_job")["bundle_path"])


def test_rv_f_w4_slash_export_relative_path_lands_in_the_session_folder(host_like):
    """★ W4: ``/handover export out.ctxh.tar`` in a chat of a process whose cwd is HERMES_HOME ⇒ the bundle path is the
    session's working folder + ``out.ctxh.tar``, never under HERMES_HOME.

    Red before the fix: ``bundle_path == 'out.ctxh.tar'`` (resolved later against the process cwd = HERMES_HOME)."""
    backend, work, host = host_like
    plugin = _plugin(backend)
    _slash(plugin, "export out.ctxh.tar --yes")
    got = _started_bundle(backend)
    assert got == str(work / "out.ctxh.tar"), got
    assert not got.startswith(str(host.hermes_home)), got


def test_rv_f_w4_tilde_is_hermes_home_rule_not_the_process_home(host_like):
    """★ W4: ``~/b.ctxh.tar`` ⇒ ``hostcompat.effective_home()/b.ctxh.tar`` (the home Hermes' file tools use), not the
    process ``HOME`` of the plugin host.

    Red before the fix: ``<HERMES_HOME>/home/b.ctxh.tar`` (``os.path.expanduser``)."""
    backend, _work, _host = host_like
    plugin = _plugin(backend)
    _slash(plugin, "export ~/b.ctxh.tar --yes")
    assert _started_bundle(backend) == str(hostcompat.effective_home() / "b.ctxh.tar")


def test_rv_f_w4_slash_import_relative_path_is_found_in_the_session_folder(host_like):
    """★ W4: ``/handover import b.ctxh.tar`` with the bundle in the session's folder ⇒ found there and imported.

    Red before the fix: "No bundle at b.ctxh.tar" (looked up in the process cwd)."""
    backend, work, _host = host_like
    (work / "b.ctxh.tar").write_bytes(b"x")
    plugin = _plugin(backend)
    out = _slash(plugin, "import b.ctxh.tar --yes")
    assert backend.names().count("start_job") == 1, out
    assert _started_bundle(backend) == str(work / "b.ctxh.tar")
    assert backend.last("start_job")["mode"] is Mode.IMPORT


def test_rv_f_w4_a_relative_path_without_a_session_folder_is_refused(host_like):
    """W4: no working folder known for the calling session ⇒ the relative path is refused with a remedy; nothing
    starts (never a guess at the process folder)."""
    backend, _work, _host = host_like
    backend.session_cwds = {}
    plugin = _plugin(backend)
    out = _slash(plugin, "export out.ctxh.tar --yes")
    assert "start_job" not in backend.names(), out
    assert "absolute path" in out, out


def test_rv_f_w4_terminal_uses_its_own_working_folder(host_like, tmp_path, monkeypatch, capsys):
    """W4: ``hermes handover export rel.ctxh.tar`` runs where it was typed: the terminal's working folder."""
    backend, _work, _host = host_like
    shell = tmp_path / "shell"
    shell.mkdir()
    monkeypatch.chdir(shell)
    plugin = _plugin(backend)
    _cli(plugin, "export", "rel.ctxh.tar", "--session", SID, "--yes", "--json")
    capsys.readouterr()
    assert _started_bundle(backend) == os.path.join(str(shell), "rel.ctxh.tar")
