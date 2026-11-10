"""Review round 3 (the second review §2 H2, R2-M5): the refusal to start a continuation while approvals are
off held only where the plugin runs inside the gateway that runs the turn. ``hermes chat --resume … -q`` is a gateway
client, so the gateway's ``--yolo`` decides; a separate ``hermes serve``/``hermes dashboard`` serving the routes and
``hermes handover take|import --continue`` in a terminal read only their own environment, and ``doctor`` said
"approvals apply" there although nothing was checked.

Now ``hostcompat.approval_check`` asks Hermes' own gateway identity check (``gateway.status``) for the profile's
running gateway: in that gateway (or the plugin host it started) its environment is this process's; elsewhere the
gateway's recorded command line shows a ``--yolo`` and the continuation is refused, while an environment-only
``HERMES_YOLO_MODE`` stays invisible — ``doctor`` says "not checked" and names the limit, the README states it.

The gateways below are real processes whose command line Hermes' matcher accepts (``<python> …/hermes [--yolo]
gateway run``) with the runtime record a gateway writes (``gateway_state.json``); nothing of Hermes is faked except,
for the in-gateway paths, which process the identity check names."""

from __future__ import annotations

import contextlib
import json
import os
import subprocess
import sys
import time
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

from ctx_handover.engine import commands, hostcompat, jobs
from ctx_handover.engine.errors import ContinueRefused
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.integration._int3_helpers import surface
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.F.test_commands import FakeBackend

SID = "20261109_141003_a1b2c3"
JOB = "01JC0000000000000000000ABC"
PLUGIN_ROOT = Path(hostcompat.__file__).resolve().parents[1]

pytestmark = pytest.mark.skipif(not Path("/proc/self/cmdline").exists(), reason="the gateway stand-in needs /proc")


@pytest.fixture(autouse=True)
def _upstream():
    require_upstream()
    jobs._reset_for_tests()
    yield
    jobs._reset_for_tests()


def _record(home: Home, pid: int, argv: list[str], *, state: str = "running", age_s: int = 0) -> None:
    from gateway.status import get_process_start_time

    payload = {
        "pid": pid,
        "kind": "hermes-gateway",
        "argv": argv,
        "start_time": get_process_start_time(pid),
        "hermes_home": str(home.hermes_home.resolve()),
        "gateway_state": state,
        "updated_at": (datetime.now(UTC) - timedelta(seconds=age_s)).isoformat(),
    }
    (home.hermes_home / "gateway_state.json").write_text(json.dumps(payload), encoding="utf-8")


@contextlib.contextmanager
def gateway(home: Home, tmp_path: Path, *flags: str):
    """A process whose command line is a gateway's (``…/hermes <flags> gateway run``) and its runtime record."""
    script = tmp_path / f"gw-{len(flags)}-{time.monotonic_ns()}" / "hermes"
    script.parent.mkdir(parents=True)
    script.write_text("import time\ntime.sleep(300)\n", encoding="utf-8")
    argv = [str(script), *flags, "gateway", "run"]
    proc = subprocess.Popen(  # noqa: S603 - this interpreter, a fixed script
        [sys.executable, *argv], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
    try:
        deadline = time.monotonic() + 10
        while b"gateway" not in Path(f"/proc/{proc.pid}/cmdline").read_bytes():
            assert time.monotonic() < deadline, "the stand-in never exec'd"
            time.sleep(0.01)
        _record(home, proc.pid, argv)
        yield proc.pid
    finally:
        proc.kill()
        proc.wait(10)
        with contextlib.suppress(FileNotFoundError):
            (home.hermes_home / "gateway_state.json").unlink()


def _spawner(home: Home):
    _source, target = jobs.make_local_apis(None, calling_session=None, session_key=None)
    spawner = target.continuation
    spawner._which = lambda _name: "/usr/bin/hermes"
    return spawner


class _Started(Exception):
    pass


def _popen(*_a, **_kw):
    raise _Started


# ---------------------------------------------------------------------------------------------------------
# Terminal path: ``hermes handover take|import --continue`` — the plugin runs in the CLI process
# ---------------------------------------------------------------------------------------------------------


def test_r3_f_h2_terminal_continue_refused_behind_a_gateway_started_with_yolo(tmp_path):
    """★ The reviewer's terminal entrance: this process (the CLI) is not the gateway; the profile's gateway runs with
    ``--yolo``. The continuation is refused (``approvals_off``) and nothing starts.

    Red before the fix: the spawner reached ``popen`` (``_Started``) — the CLI saw only its own environment."""
    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    with home.active(), gateway(home, tmp_path, "--yolo") as pid:
        check = hostcompat.approval_check(hostcompat.load_config())
        assert (check.off, check.why, check.pid, check.flag) == (True, "gateway_flag", pid, True)
        spawner = _spawner(home)
        spawner._popen = _popen
        with pytest.raises(ContinueRefused) as info:
            spawner.spawn(SID, "go on", JOB, requested_by="laptop")
        assert info.value.details.get("reason") == "approvals_off"


@pytest.mark.parametrize("flag", ["--yo", "--yol"])
def test_r3_f_h2_an_abbreviated_flag_counts(tmp_path, flag):
    """argparse takes an unambiguous prefix of ``--yolo``; the recorded command line keeps what was typed."""
    home = Home(tmp_path, "server")
    with home.active(), gateway(home, tmp_path, flag):
        assert hostcompat.approval_check(hostcompat.load_config()).why == "gateway_flag"


def test_r3_f_h2_terminal_plain_gateway_is_not_checked_and_not_refused(tmp_path):
    """The same gateway without ``--yolo``: its environment is not visible from the CLI, so the answer is "not
    checked" (``off`` None, R33a: not refused — the README and ``doctor`` name it), never "approvals apply"."""
    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    with home.active(), gateway(home, tmp_path) as pid:
        check = hostcompat.approval_check(hostcompat.load_config())
        assert (check.off, check.why, check.pid, check.flag) == (None, "gateway_elsewhere", pid, False)
        assert hostcompat.approvals_off(hostcompat.load_config()) is False
        spawner = _spawner(home)
        spawner._popen = _popen
        with pytest.raises(_Started):
            spawner.spawn(SID, "go on", JOB, requested_by="laptop")


def test_r3_f_h2_terminal_without_a_gateway_applies_approvals(tmp_path):
    """No gateway of the profile runs (no record, a stopped one, a stale one): the continuation's ``hermes chat``
    starts its own from the cleaned environment — approvals apply, checked. A fresh record of a running gateway this
    process cannot identify (another PID namespace) is "not checked"."""
    home = Home(tmp_path, "server")
    with home.active():
        config = hostcompat.load_config()
        assert hostcompat.approval_check(config).why == "no_gateway"
        assert hostcompat.approval_check(config).off is False
        gone = subprocess.Popen([sys.executable, "-c", "pass"])
        gone.wait(10)
        _record(home, gone.pid, ["hermes", "gateway", "run"], state="stopped")
        assert hostcompat.approval_check(config).why == "no_gateway"
        _record(home, gone.pid, ["hermes", "gateway", "run"], age_s=600)
        assert hostcompat.approval_check(config).why == "no_gateway"
        _record(home, gone.pid, ["hermes", "gateway", "run"])
        check = hostcompat.approval_check(config)
        assert (check.off, check.why) == (None, "gateway_unverified")


# ---------------------------------------------------------------------------------------------------------
# Separate process serving the routes: ``hermes serve`` / ``hermes dashboard`` (in-process plugin or its plugin host)
# ---------------------------------------------------------------------------------------------------------

_PROBE = r"""
import dataclasses, json
from ctx_handover.engine import hostcompat
print(json.dumps(dataclasses.asdict(hostcompat.approval_check(hostcompat.load_config()))))
"""


@pytest.mark.parametrize("plugin_host", [False, True], ids=["serve", "serve-plugin-host"])
def test_r3_f_h2_serve_process_sees_the_gateway_flag(tmp_path, plugin_host):
    """★ A route served by a separate process (``hermes serve``, or the plugin host it started — its parent is not the
    gateway) finds the profile's gateway and its ``--yolo``; without the flag it says "not checked".

    Red before the fix: ``approval_check`` did not exist; ``approvals_off`` there was False (g6 P1 entrance)."""
    from ctx_handover.tests.unit.K0.test_i_f20_home_source import run_json

    home = Home(tmp_path, "server")
    env = {**home.env(), "PATH": os.environ.get("PATH", ""), "PYTHONPATH": os.environ.get("PYTHONPATH", "")}
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    if plugin_host:
        env[hostcompat.PLUGIN_HOST_ENV] = "1"
    with gateway(home, tmp_path, "--yolo") as pid:
        assert run_json(_PROBE, env, home.hermes_home) == {"off": True, "why": "gateway_flag", "pid": pid, "flag": True}
    with gateway(home, tmp_path) as pid:
        got = run_json(_PROBE, env, home.hermes_home)
        assert got == {"off": None, "why": "gateway_elsewhere", "pid": pid, "flag": False}


# ---------------------------------------------------------------------------------------------------------
# Gateway path: the plugin runs inside the gateway (or the plugin host the gateway started)
# ---------------------------------------------------------------------------------------------------------


def test_r3_f_h2_in_the_gateway_its_own_environment_decides(tmp_path, monkeypatch):
    """Inside the gateway (Hermes' identity check names this process) the environment is the gateway's: without the
    variable approvals apply (checked), with it they are off (``gateway_env``; ``flag`` False — set only in the
    environment, which other processes do not see). The plugin host the gateway started (parent = gateway, Hermes'
    host marker) counts the same; another child does not."""
    import gateway.status as gs

    home = Home(tmp_path, "server")
    with home.active():
        config = hostcompat.load_config()
        monkeypatch.setattr(gs, "live_gateway_pid_for_home", lambda _home: os.getpid())
        monkeypatch.delenv("HERMES_YOLO_MODE", raising=False)
        check = hostcompat.approval_check(config)
        assert (check.off, check.why, check.pid) == (False, "gateway_process", os.getpid())
        monkeypatch.setenv("HERMES_YOLO_MODE", "1")
        check = hostcompat.approval_check(config)
        assert (check.off, check.why, check.flag) == (True, "gateway_env", False)
        monkeypatch.setattr(sys, "argv", ["hermes", "--yolo", "gateway", "run"])
        assert hostcompat.approval_check(config).flag is True
        monkeypatch.delenv("HERMES_YOLO_MODE")
        monkeypatch.setattr(gs, "live_gateway_pid_for_home", lambda _home: os.getppid())
        monkeypatch.setenv(hostcompat.PLUGIN_HOST_ENV, "1")
        assert hostcompat.approval_check(config).why == "gateway_process"
        monkeypatch.delenv(hostcompat.PLUGIN_HOST_ENV)
        assert hostcompat.approval_check(config).why == "gateway_elsewhere"


def test_r3_f_h2_config_still_decides_first(tmp_path):
    """``approvals.mode: off`` / an approving ``single_query_mode`` are read from the profile's config in every
    process — they refuse everywhere, with or without a gateway."""
    home = Home(tmp_path, "server")
    with home.active():
        assert hostcompat.approval_check({"approvals": {"mode": False}}).why == "config_mode"
        assert hostcompat.approval_check({"approvals": {"single_query_mode": "allow"}}).why == "config_single_query"


def test_r3_f_h2_gateway_status_failure_is_not_checked(tmp_path, monkeypatch):
    """Hermes' gateway check raising is "not checked", never "approvals apply"."""
    import gateway.status as gs

    def boom(_home):
        raise OSError("unreadable")

    home = Home(tmp_path, "server")
    with home.active():
        monkeypatch.setattr(gs, "live_gateway_pid_for_home", boom)
        check = hostcompat.approval_check(hostcompat.load_config())
        assert (check.off, check.why) == (None, "gateway_unknown")


# ---------------------------------------------------------------------------------------------------------
# doctor
# ---------------------------------------------------------------------------------------------------------


def _doctor_line(fake_host, monkeypatch, check) -> dict:
    monkeypatch.setattr(hostcompat, "approval_check", lambda config: check)
    monkeypatch.setattr(hostcompat, "approvals_off", lambda config: check.off is True)
    settings = {"allow_remote_continue": True}
    backend = FakeBackend(config={"plugins": {"entries": {"ctx_handover": {"settings": settings}}}})
    checks = {c["name"]: c for c in commands.local_checks(FakeCtx(host=fake_host), backend)}
    return checks["allow_remote_continue"]


@pytest.mark.parametrize(
    "why, ok, must",
    [
        ("gateway_process", True, ["approvals apply", "checked in this gateway's process"]),
        ("no_gateway", True, ["approvals apply", "no gateway of this profile runs"]),
        ("gateway_elsewhere", None, ["not checked", "hermes serve", "--continue", "HERMES_YOLO_MODE"]),
        ("gateway_unverified", None, ["not checked", "cannot identify"]),
        ("gateway_unknown", None, ["not checked"]),
        ("gateway_flag", None, ["--yolo", "a --continue from a paired device does not start the agent here"]),
        (
            "config_mode",
            None,
            ["approvals are off on this gateway, so a --continue from a paired device never starts the agent here"],
        ),
        ("config_single_query", None, ["single_query_mode", "never starts the agent here"]),
        ("process_env", None, ["HERMES_YOLO_MODE", "this process"]),
    ],
)
def test_r3_f_h2_doctor_says_what_it_checked(fake_host, monkeypatch, why, ok, must):
    """★ ``doctor`` names per process what holds: "approvals apply" only where it was checked; "not checked" with the
    limit where the gateway's environment is not visible.

    Red before the fix: ``gateway_elsewhere``/``gateway_unverified``/``gateway_unknown`` gave ``ok True`` and
    "(approvals apply)" (``commands.py:1520-1521`` @ 803af136)."""
    check = hostcompat.ApprovalCheck(hostcompat.APPROVAL_WHY[why], why, 4242 if why.startswith("gateway") else None)
    line = _doctor_line(fake_host, monkeypatch, check)
    assert line["ok"] is ok, line
    for text in must:
        assert text in line["detail"], (text, line["detail"])
    if ok is not True:
        assert "approvals apply)" not in line["detail"]


def test_r3_f_h2_doctor_names_an_environment_only_yolo_in_the_gateway(fake_host, monkeypatch):
    """In a gateway that runs with ``HERMES_YOLO_MODE`` only in its environment, a continuation started there is
    refused, one started through ``hermes serve`` or a terminal is not: the line says both and asks to turn the
    setting off. With ``--yolo`` on its command line the other processes see it, and the line says so."""
    line = _doctor_line(fake_host, monkeypatch, hostcompat.ApprovalCheck(True, "gateway_env", 4242, False))
    assert line["ok"] is None
    for text in ("HERMES_YOLO_MODE", "refused", "hermes serve", "terminal", "not visible there", "turn"):
        assert text in line["detail"], (text, line["detail"])
    line = _doctor_line(fake_host, monkeypatch, hostcompat.ApprovalCheck(True, "gateway_env", 4242, True))
    assert "--yolo" in line["detail"] and "not visible there" not in line["detail"], line["detail"]


def test_r3_f_h2_doctor_in_a_terminal_behind_real_gateways(tmp_path):
    """The real ``hermes handover doctor`` in a terminal (this process) beside a stand-in gateway: ``--yolo`` is named,
    a plain gateway gives "not checked" — never "(approvals apply)" for what the terminal cannot see."""
    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    home.identity()
    with gateway(home, tmp_path, "--yolo"):
        call, _ = surface(home, "doctor")
    out = "\n".join(call.out)
    assert "allow_remote_continue" in out and "--yolo" in out, out
    with gateway(home, tmp_path):
        call, _ = surface(home, "doctor")
    out = "\n".join(call.out)
    line = next(x for x in out.splitlines() if "allow_remote_continue" in x)
    assert "not checked" in line and "approvals apply)" not in line, line


# ---------------------------------------------------------------------------------------------------------
# README
# ---------------------------------------------------------------------------------------------------------


def _readme() -> str:
    return " ".join((PLUGIN_ROOT / "README.md").read_text(encoding="utf-8").split())


def test_r3_f_h2_readme_states_where_the_refusal_holds():
    """★ The refusal is promised only as far as the starting process can tell, the limit is its own point in Limits
    (not inside the ``isolation: host`` paragraph), and Commands/Unredacted point to it.

    Red before the change: ``README.md:272`` "never when approvals are off there." and ``:336`` "refused when
    approvals are off)" stood without the limit; the ``--yolo`` sentence sat in the host paragraph."""
    text = _readme()
    limits = text.split("## Limits — read this first", 1)[1].split("## What the lock", 1)[0]
    host = next(p for p in limits.split(" - ") if p.startswith("Under `plugins.isolation: host`"))
    assert "--yolo" not in host and "HERMES_YOLO_MODE" not in host, host
    bullet = next(p for p in limits.split(" - ") if p.startswith("**Continuation and approvals.**"))
    for part in (
        "as far as the process that starts it can tell",
        "`hermes serve`",
        "`hermes dashboard`",
        "`hermes handover take|import --continue` in a terminal",
        "see the gateway's command line (its `--yolo`) but not its environment",
        "leave `allow_remote_continue` off",
        "`/handover doctor`",
    ):
        assert part in bullet, (part, bullet)
    assert "never when approvals are off there." not in text
    assert "never when approvals are off there as far as the starting process can tell (see Limits)" in text
    assert "refused when approvals are off as far as the starting process can tell)" in text
    assert "`gateway.status`" in text.split("**Core modules used:**", 1)[1].split("**", 1)[0]


def test_r3_f_h2_readme_discloses_the_gateway_record_read():
    """Regel 13: the new read (the profile's gateway identity through Hermes' ``gateway.status``) is under Reads."""
    reads = _readme().split("**Reads:**", 1)[1].split("**Writes:**", 1)[0]
    assert "`gateway.status`" in reads and "`gateway_state.json`" in reads and "command line" in reads, reads


def test_r3_f_h2_settings_form_states_the_limit():
    """The settings form (``plugin.yaml`` ``allow_remote_continue``) carries the same limit, not "never".

    Red before the change: "Never when approvals are off on this gateway"."""
    import hermes_yaml as yaml

    schema = yaml.safe_load((PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8"))["config_schema"]
    text = schema["allow_remote_continue"]["description"]
    assert "Never when approvals are off" not in text
    assert "as far as the process that starts the turn can tell" in text and "HERMES_YOLO_MODE" in text, text
