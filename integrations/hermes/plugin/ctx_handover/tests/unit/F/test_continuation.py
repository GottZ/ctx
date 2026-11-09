"""Cluster F — continuation subprocess (DESIGN §11.5, R10, R22, SEC 4.4; TESTPLAN §2.7).

The real-process tests put an executable named ``hermes`` into a temp bin directory that records its argv,
environment and session id and exits with a chosen code; the Spawner starts it through the production path
(``subprocess.Popen``, detached, scrubbed environment, log file, waiter thread).
"""

from __future__ import annotations

import json
import os
import stat
import sys
import time
from pathlib import Path

import pytest

from ctx_handover.engine import continuation, hostcompat
from ctx_handover.engine.continuation import SCRUBBED_ENV, Spawner, inject_after_adoption
from ctx_handover.engine.errors import CapabilityMissing, ContinueRefused, HandoverError

_FAKE_HERMES = """#!{python}
import json, os, sys
out = {{"argv": sys.argv[1:], "env": dict(os.environ), "pid": os.getpid(), "sid": os.getsid(0),
        "cwd": os.getcwd(), "stdin_tty": os.isatty(0)}}
with open({record!r}, "w", encoding="utf-8") as fh:
    json.dump(out, fh)
print("fake hermes ran")
sys.exit({code})
"""


def _fake_hermes(tmp_path: Path, code: int = 0) -> tuple[Path, Path]:
    bindir = tmp_path / "bin"
    bindir.mkdir()
    record = tmp_path / "record.json"
    exe = bindir / "hermes"
    exe.write_text(_FAKE_HERMES.format(python=sys.executable, record=str(record), code=code), encoding="utf-8")
    exe.chmod(0o755)
    return exe, record


def _wait_for(path: Path, timeout: float = 15.0) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if path.is_file() and path.stat().st_size > 0:
            try:
                return json.loads(path.read_text(encoding="utf-8"))
            except ValueError:
                pass
        time.sleep(0.05)
    raise AssertionError(f"child did not write {path}")


def _wait_exit(spawner: Spawner, job_id: str, timeout: float = 15.0) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        res = spawner.result(job_id)
        if res.get("exit_code") is not None:
            return res
        time.sleep(0.05)
    raise AssertionError(f"no exit code recorded for {job_id}: {spawner.result(job_id)}")


SID = "20261109_141003_a1b2c3"
JOB = "01JC0000000000000000000ABC"


def test_continue_child_env_scrubbed(tmp_path, monkeypatch):
    """★ R22, SEC 4.4: auto-approve variables of the parent never reach the continuation child.

    Red proof: mutant = ``Spawner.child_env`` returns ``dict(self._environ)`` (inherit env) →
    ``AssertionError: assert 'HERMES_YOLO_MODE' not in {...}``.
    """
    exe, record = _fake_hermes(tmp_path)
    for name in SCRUBBED_ENV:
        monkeypatch.setenv(name, "1")
    monkeypatch.setenv("HERMES_PLUGIN_HOST_PROCESS", "1")
    monkeypatch.setenv("HERMES_SESSION_PLATFORM", "telegram")
    monkeypatch.setenv("CTXH_TEST_KEEP", "kept")
    spawner = Spawner(log_dir=tmp_path / "logs", profile=None, approvals_off=lambda: False, which=lambda _n: str(exe))
    res = spawner.spawn(SID, "go on", JOB, requested_by=None)
    assert res.started and res.pid
    child = _wait_for(record)
    env = child["env"]
    assert "HERMES_YOLO_MODE" not in env
    for name in SCRUBBED_ENV:
        assert name not in env, name
    assert "HERMES_PLUGIN_HOST_PROCESS" not in env
    assert "HERMES_SESSION_PLATFORM" not in env
    assert env.get("CTXH_TEST_KEEP") == "kept"
    assert "--yolo" not in child["argv"]
    assert not any(a.startswith("--accept-hooks") for a in child["argv"])
    _wait_exit(spawner, JOB)


def test_continue_refused_when_approvals_off(tmp_path):
    """★ R22: approvals off (``approvals.mode: off`` or ``single_query_mode: approve``) ⇒ ``continue_refused``
    with reason ``approvals_off``; nothing is started and nothing is written.

    Red proof: mutant = the ``if self._approvals_off():`` check in ``Spawner.spawn`` disabled → the child is
    started: ``AssertionError: must not start`` (raised by the recording ``popen``).
    """
    started: list = []

    def popen(*a, **kw):
        started.append(a)
        raise AssertionError("must not start")

    for config in (
        {"approvals": {"mode": False}},  # YAML `off` arrives as False
        {"approvals": {"mode": "off"}},
        {"approvals": {"single_query_mode": "approve"}},
    ):
        spawner = Spawner(
            log_dir=tmp_path / "logs",
            profile=None,
            approvals_off=lambda c=config: hostcompat.approvals_off(c),
            allow_remote=lambda: True,
            which=lambda _n: "/usr/bin/hermes",
            popen=popen,
        )
        with pytest.raises(ContinueRefused) as exc:
            spawner.spawn(SID, "go on", JOB, requested_by="laptop")
        assert exc.value.details.get("reason") == "approvals_off"
    assert started == []
    assert not (tmp_path / "logs").exists()


def test_continue_allowed_with_default_approvals(tmp_path):
    """Default approvals (manual, single_query_mode deny) do not block the continuation."""
    calls: list = []

    class _Proc:
        pid = 4242

        def wait(self):
            return 0

    def popen(argv, **kw):
        calls.append((argv, kw))
        return _Proc()

    spawner = Spawner(
        log_dir=tmp_path / "logs",
        profile=None,
        approvals_off=lambda: hostcompat.approvals_off({"approvals": {"mode": "manual"}}),
        which=lambda _n: "/usr/bin/hermes",
        popen=popen,
    )
    res = spawner.spawn(SID, "go on", JOB, requested_by=None)
    assert res.started and res.pid == 4242 and calls


def test_continue_only_for_signed_peers(tmp_path):
    """Unverified origin never continues; a paired device's request needs ``allow_remote_continue`` (default off);
    its prompt is capped at 2000 chars and prefixed (DESIGN §11.5, R10, T16)."""
    calls: list = []

    class _Proc:
        pid = 77

        def wait(self):
            return 0

    def popen(argv, **kw):
        calls.append(argv)
        return _Proc()

    closed = Spawner(
        log_dir=tmp_path / "logs", profile=None, approvals_off=lambda: False, which=lambda _n: "/x/hermes", popen=popen
    )
    with pytest.raises(ContinueRefused) as exc:
        closed.spawn(SID, "do it", JOB, requested_by=None, verified=False)
    assert exc.value.details.get("reason") == "unverified"
    with pytest.raises(ContinueRefused) as exc:
        closed.spawn(SID, "do it", JOB, requested_by="laptop")
    assert exc.value.details.get("reason") == "not_allowed"
    assert calls == []
    spawner = Spawner(
        log_dir=tmp_path / "logs",
        profile=None,
        approvals_off=lambda: False,
        allow_remote=lambda: True,
        which=lambda _n: "/x/hermes",
        popen=popen,
    )

    long_prompt = "x" * 5000
    spawner.spawn(SID, long_prompt, JOB, requested_by="lap\x00top\n")
    query = calls[-1][-1]
    assert query.startswith("--query=[continuation requested by laptop] ")
    body = query[len("--query=[continuation requested by laptop] ") :]
    assert body == "x" * continuation.PROMPT_MAX


def test_continue_spawn_argv_and_profile(tmp_path):
    """``hermes [-p profile] chat --resume <id> --query=<prompt>``; detached (own session), stdin not a TTY,
    output in ``logs/continue-<job>.log``, exit code from the trailer (DESIGN §11.5)."""
    exe, record = _fake_hermes(tmp_path, code=3)
    spawner = Spawner(log_dir=tmp_path / "logs", profile="work", approvals_off=lambda: False, which=lambda _n: str(exe))
    workdir = tmp_path / "work"
    workdir.mkdir()
    res = spawner.spawn(SID, "-p evil", JOB, requested_by=None, cwd=str(workdir))
    child = _wait_for(record)
    assert child["argv"] == ["-p", "work", "chat", "--resume", SID, "--query=-p evil"]
    assert child["sid"] == child["pid"], "child must lead its own session (start_new_session)"
    assert child["sid"] != os.getsid(0)
    assert child["stdin_tty"] is False
    assert child["cwd"] == str(workdir)
    done = _wait_exit(spawner, JOB)
    assert done["exit_code"] == 3 and done["running"] is False
    log = Path(res.log)
    assert log.name == f"continue-{JOB}.log"
    assert "fake hermes ran" in log.read_text(encoding="utf-8")
    assert "exited with code 3" in log.read_text(encoding="utf-8")
    assert stat.S_IMODE(log.stat().st_mode) == 0o600
    assert stat.S_IMODE(log.parent.stat().st_mode) == 0o700

    default = Spawner(log_dir=tmp_path / "l2", profile="default", approvals_off=lambda: False, which=lambda _n: "/h")
    assert default.hermes_argv() == ["/h"]
    assert Spawner(
        log_dir=tmp_path / "l3", profile=None, approvals_off=lambda: False, which=lambda _n: "/h"
    ).hermes_argv() == ["/h"]


def test_continue_default_prompt_and_idempotent(tmp_path):
    """Empty prompt ⇒ ``Continue where we left off.``; a second spawn of the same job returns the recorded child
    (DESIGN §3.7 ``target.continue``: started once)."""
    calls: list = []

    class _Proc:
        pid = 99

        def wait(self):
            return 0

    def popen(argv, **kw):
        calls.append(argv)
        return _Proc()

    spawner = Spawner(
        log_dir=tmp_path / "logs", profile=None, approvals_off=lambda: False, which=lambda _n: "/h", popen=popen
    )
    first = spawner.spawn(SID, None, JOB, requested_by=None)
    second = spawner.spawn(SID, "other", JOB, requested_by=None)
    assert calls[0][-1] == "--query=" + continuation.DEFAULT_PROMPT
    assert len(calls) == 1
    assert second.pid == first.pid and second.log == first.log


def test_continue_hermes_argv_fallbacks(tmp_path):
    """``which("hermes")`` first, else ``sys.argv[0]`` when it is named hermes[.exe], else capability_missing."""
    sp = Spawner(
        log_dir=tmp_path, profile=None, approvals_off=lambda: False, which=lambda _n: None, argv0="/opt/x/hermes"
    )
    assert sp.hermes_argv() == ["/opt/x/hermes"]
    sp = Spawner(
        log_dir=tmp_path, profile=None, approvals_off=lambda: False, which=lambda _n: None, argv0="C:/h/HERMES.EXE"
    )
    assert sp.hermes_argv()[0].lower().endswith("hermes.exe")
    sp = Spawner(
        log_dir=tmp_path, profile=None, approvals_off=lambda: False, which=lambda _n: None, argv0="/usr/bin/python3"
    )
    with pytest.raises(CapabilityMissing):
        sp.hermes_argv()


def test_continue_rejects_option_like_session_ids(tmp_path):
    """A session id is never passed through when it could be read as an option or carries control characters."""
    sp = Spawner(
        log_dir=tmp_path, profile=None, approvals_off=lambda: False, which=lambda _n: "/h", popen=lambda *a, **k: None
    )
    for bad in ("-p", "--yolo", "a b", "x\n", "", "a" * 200):
        with pytest.raises(HandoverError):
            sp.spawn(bad, "go", JOB, requested_by=None)


def test_continue_result_unknown_job(tmp_path):
    sp = Spawner(log_dir=tmp_path / "logs", profile=None, approvals_off=lambda: False)
    assert sp.result(JOB) == {"started": False}


def test_inject_after_adoption_is_best_effort():
    class Ctx:
        def __init__(self, result):
            self.result = result
            self.calls = []

        def inject_message(self, content, role="user", *, session_key=None, origin=None):
            self.calls.append((content, role, session_key))
            if isinstance(self.result, BaseException):
                raise self.result
            return self.result

    ok = Ctx(True)
    assert inject_after_adoption(ok, "agent:main:local:dm:x", "go") is True
    assert ok.calls == [("go", "user", "agent:main:local:dm:x")]
    assert inject_after_adoption(Ctx(False), "k", "go") is False
    assert inject_after_adoption(Ctx(RuntimeError("no gateway")), "k", "go") is False
    assert inject_after_adoption(object(), "k", "go") is False
