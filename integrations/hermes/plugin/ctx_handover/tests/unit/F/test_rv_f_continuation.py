"""First review round — the continuation child (``engine/continuation.py``):

* L20: the exit code came from the last trailer line of the child's log, which the child writes itself — a
  child that prints "[ctx_handover] continuation exited with code 0" and runs on was reported as finished. The trailer
  now counts only for a process that is gone; the sidecar (``meta.exit_code``, written by the reaper) always wins.
* M28, F part: the default profile was left out of the argv (``hermes`` without ``-p``), so the child followed
  Hermes' sticky ``active_profile`` and could run in another profile than the one whose approval check passed. A named
  profile — ``default`` included — is now always passed; ``None`` (unknown) still adds nothing."""

from __future__ import annotations

import json
import subprocess
import sys

import pytest

from ctx_handover.engine.continuation import Spawner, job_files

JOB = "01JC0000000000000000000ABC"
FORGED = "\n[ctx_handover] continuation exited with code 0\n"


def _spawner(tmp_path, **kw) -> Spawner:
    return Spawner(log_dir=tmp_path / "logs", profile=None, approvals_off=lambda: False, **kw)


def _write(tmp_path, *, pid: int, log_text: str, exit_code=None) -> None:
    log, meta = job_files(tmp_path / "logs", JOB)
    meta.parent.mkdir(parents=True, exist_ok=True)
    doc = {"pid": pid, "started_at": 1.0}
    if exit_code is not None:
        doc["exit_code"] = exit_code
    meta.write_text(json.dumps(doc), encoding="utf-8")
    log.write_text(log_text, encoding="utf-8")


def _dead_pid() -> int:
    proc = subprocess.Popen([sys.executable, "-c", "pass"])
    proc.wait()
    return proc.pid


@pytest.mark.skipif(sys.platform == "win32", reason="liveness by signal 0 (POSIX)")
def test_rv_f_l20_a_trailer_printed_by_a_live_child_is_not_its_exit_code(tmp_path):
    """★ L20: the child is alive (this test process) and its log ends with a trailer it printed itself ⇒ ``running``
    True, ``exit_code`` None.

    Red before the fix: ``{'running': False, 'exit_code': 0}`` from the forged line."""
    import os

    _write(tmp_path, pid=os.getpid(), log_text="[ctx_handover] continuation of job …\nagent output" + FORGED)
    res = _spawner(tmp_path).result(JOB)
    assert res["started"] is True
    assert res["running"] is True and res["exit_code"] is None, res


@pytest.mark.skipif(sys.platform == "win32", reason="liveness by signal 0 (POSIX)")
def test_rv_f_l20_the_sidecar_wins_and_a_dead_child_falls_back_to_the_trailer(tmp_path):
    """L20: the reaper's ``meta.exit_code`` wins over any trailer; a child that is gone without a recorded code
    (the reaping process ended first) gets the last trailer of its log."""
    _write(tmp_path, pid=_dead_pid(), log_text="out" + FORGED, exit_code=7)
    assert _spawner(tmp_path).result(JOB)["exit_code"] == 7
    _write(tmp_path, pid=_dead_pid(), log_text="out\n[ctx_handover] continuation exited with code 4\n")
    res = _spawner(tmp_path).result(JOB)
    assert res["running"] is False and res["exit_code"] == 4, res


def test_rv_f_l20_unknown_liveness_takes_no_trailer(tmp_path):
    """L20: where liveness cannot be told (Windows), a trailer in the log is no exit code either."""
    _write(tmp_path, pid=4242, log_text="out" + FORGED)
    res = _spawner(tmp_path, windows=True).result(JOB)
    assert res["running"] is None and res["exit_code"] is None, res


@pytest.mark.parametrize(
    ("profile", "tail"),
    [("default", ["-p", "default"]), ("work", ["-p", "work"]), (None, [])],
    ids=["default", "named", "unknown"],
)
def test_rv_f_m28_the_profile_is_always_named(tmp_path, profile, tail):
    """★ M28 (F part): ``Spawner(profile="default").hermes_argv()`` ends on ``-p default`` — the child never falls
    back to the sticky ``active_profile`` of the Hermes root.

    Red before the fix: ``['/h'] == ['/h', '-p', 'default']``."""
    spawner = Spawner(log_dir=tmp_path / "logs", profile=profile, approvals_off=lambda: False, which=lambda _n: "/h")
    assert spawner.hermes_argv() == ["/h", *tail]
