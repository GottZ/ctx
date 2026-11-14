"""``hermes ctx-discord-policy status``: the isolation mode of this profile, and whether the gateway that
wrote ``status.json`` still runs (a status file outlives its gateway)."""

from __future__ import annotations

import argparse
import io
import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

# A gateway in another process: loads the runtime the way the entry point does, writes the status file on
# the production path, then waits for a line on stdin (or exits at once).
WRITER = r"""
import importlib.util, sys
from pathlib import Path
root, home, mode = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
spec = importlib.util.spec_from_file_location("hermes_plugins_writer.ctx_discord_policy", root / "__init__.py",
                                              submodule_search_locations=[str(root)])
entry = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = entry
spec.loader.exec_module(entry)
entry.load_anchor(root)
sys.modules["ctx_discord_policy.bootstrap"].write_status(home)
print("written", flush=True)
if mode == "wait":
    sys.stdin.readline()
"""


def _status(anchor, home, *argv):
    cli = anchor.mod("cli")
    parser = argparse.ArgumentParser()
    cli.setup(parser)
    out, err = io.StringIO(), io.StringIO()
    code = cli.run(parser.parse_args(["status", *argv]), home=home, out=out, err=err)
    assert code == 0, err.getvalue()
    return out.getvalue()


def _gateway(home: Path, *, wait: bool) -> subprocess.Popen:
    env = {**os.environ, "HERMES_HOME": str(home), "PYTHONDONTWRITEBYTECODE": "1"}
    proc = subprocess.Popen([sys.executable, "-I", "-B", "-c", WRITER, str(ROOT), str(home),
                             "wait" if wait else "exit"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            text=True, env=env)
    assert proc.stdout.readline().strip() == "written"
    return proc


def test_host_isolation_is_named_as_the_reason_nothing_is_active(anchor, monkeypatch):
    home = anchor.tmp / "owner"
    monkeypatch.setattr(anchor.mod("hermes"), "isolation_mode", lambda: ("host", "probe"))
    text = _status(anchor, home)
    assert text.startswith("Not active in this profile: plugins.isolation is host"), text
    report = json.loads(_status(anchor, home, "--json"))
    assert report["plugins_isolation"] == {"mode": "host", "evidence": "probe", "allows_plugin": False}
    monkeypatch.setattr(anchor.mod("hermes"), "isolation_mode", lambda: ("in_process", "probe"))
    assert "Not active" not in _status(anchor, home)
    assert json.loads(_status(anchor, home, "--json"))["plugins_isolation"]["allows_plugin"] is True

    def unreadable():
        raise ValueError("broken config.yaml")

    monkeypatch.setattr(anchor.mod("hermes"), "isolation_mode", unreadable)
    text = _status(anchor, home)
    assert text.startswith("Isolation mode unknown: plugins.isolation could not be read: broken config.yaml"), text


@pytest.mark.skipif(os.name != "posix", reason="the writer's process lock needs flock")
def test_a_status_whose_gateway_has_stopped_is_marked_stale(anchor):
    home = anchor.tmp / "owner"
    gateway = _gateway(home, wait=True)
    try:
        running = _status(anchor, home)
        assert "that gateway is running" in running and "stale" not in running, running
        assert json.loads(_status(anchor, home, "--json"))["gateway_status_writer"]["state"] == "running"
    finally:
        gateway.communicate("\n", timeout=60)
    stopped = _status(anchor, home)
    assert f"Gateway status (pid {gateway.pid}" in stopped and "is stale: the gateway that wrote it has stopped" \
        in stopped, stopped
    writer = json.loads(_status(anchor, home, "--json"))["gateway_status_writer"]
    assert writer["state"] == "stopped", writer


@pytest.mark.skipif(os.name != "posix", reason="the writer's process lock needs flock")
def test_a_gateway_that_exits_at_once_leaves_a_stale_status(anchor):
    home = anchor.tmp / "owner"
    _gateway(home, wait=False).communicate(timeout=60)
    assert "is stale" in _status(anchor, home)


@pytest.mark.skipif(os.name != "posix", reason="the writer's process lock needs flock")
def test_the_writing_process_itself_counts_as_running(anchor):
    home = anchor.tmp / "owner"
    anchor.bootstrap.write_status(home)
    anchor.bootstrap.write_status(home)          # a second bind of the same gateway: one lock, still held
    assert json.loads(_status(anchor, home, "--json"))["gateway_status_writer"]["state"] == "running"


def test_a_status_without_the_writer_lock_is_unknown(anchor):
    """A status file written before the lock existed (or where files cannot be locked): no guess."""
    home = anchor.tmp / "owner"
    data = home / "plugin-data" / "ctx_discord_policy"
    data.mkdir(parents=True)
    (data / "status.json").write_text(json.dumps({"pid": 4242, "plugin_version": "3.0.0", "written_at": 0,
                                                  "armed": {"k": {"profile": "default"}}}), encoding="utf-8")
    (data / "status.lock").touch()   # free, but this writer never took it: free says nothing about it
    text = _status(anchor, home)
    assert "whether that gateway still runs is unknown" in text and "armed profiles: default" in text, text
    assert json.loads(_status(anchor, home, "--json"))["gateway_status_writer"]["state"] == "unknown"


@pytest.mark.skipif(os.name != "posix", reason="the writer's process lock needs flock")
def test_a_status_written_under_another_kernel_is_unknown(anchor, monkeypatch):
    """A lock is seen only by processes of one kernel: a CLI on the host of Docker Desktop, the gateway in its
    Linux VM, would take the lock and call a running gateway stopped."""
    home = anchor.tmp / "owner"
    _gateway(home, wait=False).communicate(timeout=60)
    path = home / "plugin-data" / "ctx_discord_policy" / "status.json"
    data = json.loads(path.read_text(encoding="utf-8"))
    assert data["writer_lock"] is True and data["writer_kernel"] == anchor.mod("status_lock").kernel_id()
    data["writer_kernel"] = "another-kernel"
    path.write_text(json.dumps(data), encoding="utf-8")
    writer = json.loads(_status(anchor, home, "--json"))["gateway_status_writer"]
    assert writer["state"] == "unknown" and "another kernel" in writer["reason"], writer


# ── the settings page reads the same status (under host isolation it is the one surface left) ─────────


def _page(anchor, home, path):
    from fakes.dashboard import FakeBackend

    routes = anchor.mod("web.routes")
    backend = FakeBackend()
    backend.hermes = anchor.mod("hermes")
    req = routes.WebRequest(method="GET", path=path, query={}, body=None, home=home,
                            plugin_dir=ROOT, process="dashboard", surface="dashboard", backend=backend)
    resp = routes.handle(req)
    assert resp.status == 200, resp.body
    return resp.body


@pytest.mark.skipif(os.name != "posix", reason="the writer's process lock needs flock")
def test_the_settings_page_marks_a_stopped_gateway_and_host_isolation(anchor, monkeypatch):
    home = anchor.tmp / "owner"
    gateway = _gateway(home, wait=True)
    try:
        assert _page(anchor, home, "/status")["writer"]["state"] == "running"
        assert _page(anchor, home, "/overview")["protection"]["writer"]["state"] == "running"
    finally:
        gateway.communicate("\n", timeout=60)
    status = _page(anchor, home, "/status")
    assert status["writer"]["state"] == "stopped" and status["isolation"]["allows_plugin"] is True, status
    assert _page(anchor, home, "/overview")["protection"]["writer"]["state"] == "stopped"
    monkeypatch.setattr(anchor.mod("hermes"), "isolation_mode", lambda: ("host", "probe"))
    assert _page(anchor, home, "/status")["isolation"] == {"mode": "host", "evidence": "probe",
                                                             "allows_plugin": False}
    assert _page(anchor, home, "/overview")["protection"]["isolation"]["allows_plugin"] is False


def test_the_settings_page_without_a_status_names_no_writer(anchor):
    home = anchor.tmp / "owner"
    assert _page(anchor, home, "/status")["writer"] is None
    assert _page(anchor, home, "/overview")["protection"]["writer"] is None
