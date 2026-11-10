"""Layer 3 — integrator round 4 (``feat/ctx-handover`` ``ad5a5cfa``) on the paired two-gateway stand of
``conftest.lab``: R3-H1 and R3-H2 of ``review-code/REVIEW-3.md`` end to end.

* ``test_r4_h1_pull_abort_after_a_long_confirmation_reports_released_only_when_it_is`` — R3-H1 (DECISIONS R37,
  contract V37): a pull whose CLI confirmation stays open while A's never-locked source row gets older than A's
  ``staging_ttl``; the sweep of A's listener ends that row, then the confirmation is answered. "Released"/"aborted"
  may only come from a fresh, positive release of the hold that exists then; A's session is usable afterwards. Fast
  variant (normal case): the row's age is set by backdating one ledger column (``_backdate_source_row``).
* ``test_r4_h1_realtime_confirmation_outlives_the_staging_ttl`` — the same in real time (the confirmation is held
  ≈ 61 min); opt-in ``CTXH_E2E_R4H1_REALTIME=1``, otherwise skipped with that reason.
* ``test_r4_h2_continuation_refused_when_the_gateway_cannot_be_identified`` — R3-H2 (R22, R37): the process that
  serves the plugin routes (``hermes serve``) runs in a PID namespace of its own, as Hermes' docker-compose runs
  dashboard and gateway apart; the gateway runs with ``HERMES_YOLO_MODE=1``. A ``--continue`` push from B is refused
  with ``approvals_unchecked`` and no turn runs.

Expected red on ``88068475`` (the product state before round 4; read, not run) — see each test's docstring. Code
places are cited as ``engine/<file>:<line>`` at ``ad5a5cfa``.

Lab means: the fake model, ``labctl.py`` for every stand component (fixed tokens), the ``GW_ENV`` knob of
``lab/patches/labctl-L3.md`` for the gateway's extra environment; a pseudo-terminal of this file for the interactive
CLI confirmation (``_PtyCli``); one ``hermes serve`` started by this file through ``unshare --pid --fork --mount-proc
--kill-child`` with ``labctl.env_for`` (labctl has no namespace knob, and no other lab file may change); for the fast
R3-H1 variant one compare-and-set ``UPDATE`` of A's ledger (table ``jobs``, column ``updated_at`` of the job's source
row — lab state, not product code; ``_backdate_source_row``). Session sources are repositories of their own
(``RepoBuilder`` under ``lab/ws-e2e/<x>``, L3-F19). Settings are restored byte-exact (``Lab.preserved``), components
restarted through ``labctl.py``, every process a test starts is ended by PID in ``finally``; evidence under
``lab/evidence/e2e-<date>/<test>/``.

Runtime estimate: R3-H1 fast ≈ 3–5 min; R3-H1 real time ≈ 63–68 min (A's ``staging_ttl_hours`` cannot go below 1,
so the confirmation is held for 3600 s plus at most one 10-s sweep; only with ``CTXH_E2E_R4H1_REALTIME=1``); R3-H2
≈ 3–5 min.
"""

from __future__ import annotations

import contextlib
import json
import os
import pty
import re
import select
import shutil
import sqlite3
import subprocess
import threading
import time
import urllib.error
import urllib.request
from collections.abc import Callable, Mapping
from pathlib import Path

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)

MiB = 1024 * 1024
BIG_FILES = 3
BIG_FILE_MIB = 40
TTL_HOURS = 1
"""``staging_ttl_hours`` on A: the smallest value the schema accepts (``engine/settings.py:71``, ``:113-114``,
``:207-208`` "must be at least 1"; ``engine/jobs.py:142`` hours × 3600)."""
TTL_S = TTL_HOURS * 3600
SWEEP_MARGIN_S = 240.0
"""Wait beyond A's source row's ``updated_at`` + ``staging_ttl`` for the sweep (``engine/jobs.py:81``
``MAINTENANCE_S = 10``, loop ``:707-720``) — a precondition window, not a product bound."""
REALTIME_ENV = "CTXH_E2E_R4H1_REALTIME"
"""Opt-in of the real-time R3-H1 test (value ``1``); without it only the fast variant runs."""
BACKDATE_MARGIN_S = 600.0
"""Fast R3-H1: how far beyond ``staging_ttl`` A's source row's ``updated_at`` is moved into the past."""
FAST_SWEEP_WAIT_S = 120.0
"""Fast R3-H1: wait for A's sweep after the backdate (one tick every ``MAINTENANCE_S = 10`` s, ``engine/jobs.py:81``)
— a precondition window, not a product bound."""
ASK_TIMEOUT_S = 420.0
TRANSFER_STATES = frozenset({"exporting", "context_staged", "packing", "data_staged"})
CANCEL_WINDOW = TRANSFER_STATES | {
    "locked",
    "target_preparing",
    "target_prepared",
    "source_preparing",
    "delta_applying",
    "target_resetting",
    "source_prepared",
}
"""Conductor states after the source lock and before the commit point (``engine/model.py`` ``ConductorState``): a
cancel there aborts the job (only reached when the late lock is accepted, as on ``88068475``)."""
AFTER_ANSWER_END = frozenset(
    {"aborting", "aborted", "done", "suspended", "in_doubt", "committing", "committed", "retiring", "retired"}
)
GIVEN_BACK = frozenset({"released", "reclaimed"})
"""Source states that give the session back (``engine/local_source.py`` ``_GIVEN_BACK``)."""
ACTIVE_SOURCE_STATES = frozenset({"locking", "locked", "preparing", "prepared", "retiring", "retired", "exported"})
"""Source states that hold a lineage (``engine/ledger.py`` ``ACTIVE_SOURCE_STATES``)."""
YOLO = "HERMES_YOLO_MODE"
QUIET_S = 15.0
HELLO_PATH = "/api/plugins/ctx_handover/v1/hello"
SKIPPED_LINE = b"Plugin 'ctx_handover' skipped"
ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][A-Za-z0-9]")
"""Terminal control sequences a CLI may write to a pseudo-terminal (removed before the ``--json`` line is parsed)."""


# ---------------------------------------------------------------------------------------------------------
# Helpers (this file only; ``_lab``/``conftest`` belong to the lab runner)
# ---------------------------------------------------------------------------------------------------------


def _cleanup(ev, name: str, fn: Callable[[], object]) -> None:
    """Run one clean-up step; its failure is recorded, never raised over the test's own result."""
    try:
        fn()
    except Exception as exc:  # evidence only: clean-up must not mask the assertion that failed
        ev.write(f"cleanup-error-{name}.txt", repr(exc))


def _state(lab, inst: str, jid: str | None, role: str = "conductor") -> str | None:
    return (lab.job_row(inst, jid, role) or {}).get("state") if jid else None


def _source_state(lab, inst: str, jid: str | None) -> str | None:
    """State of the job's source handover (``source_handovers``), the row that holds the lineage; None without one."""
    return (lab.source_row(inst, jid) or {}).get("state") if jid else None


def _ops(lab, inst: str, jid: str) -> list[dict]:
    sql = "SELECT op, digest, op_id, state, result, error, updated_at FROM ops WHERE job_id = ? ORDER BY updated_at"
    return lab.ledger_rows(inst, sql, (jid,))


def _proc_env(pid: int | None) -> dict[str, str]:
    if not pid:
        return {}
    try:
        raw = Path(f"/proc/{pid}/environ").read_bytes()
    except OSError:
        return {}
    out = {}
    for part in raw.split(b"\0"):
        key, sep, value = part.partition(b"=")
        if sep:
            out[key.decode(errors="replace")] = value.decode(errors="replace")
    return out


def _pidns(pid: int | None) -> str | None:
    """The PID namespace of ``pid`` (``/proc/<pid>/ns/pid`` link text), None when unreadable."""
    if not pid:
        return None
    try:
        return os.readlink(f"/proc/{pid}/ns/pid")
    except OSError:
        return None


def _hello(base: str, token: str, *, timeout: float = 10.0) -> dict:
    """Unsigned ``GET <base>/api/plugins/ctx_handover/v1/hello`` with the dashboard token: 200 once the plugin's routes
    are mounted; the first request also starts that process's listener and its maintenance sweep
    (``engine/jobs.py:707-720``)."""
    request = urllib.request.Request(  # noqa: S310 - fixed http://127.0.0.1 lab URL
        f"{base}{HELLO_PATH}", headers={"X-Hermes-Session-Token": token}
    )
    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as resp:  # noqa: S310 - loopback lab URL
            status, body = resp.status, resp.read(4096)
    except urllib.error.HTTPError as exc:
        status, body = exc.code, exc.read(4096)
    except OSError as exc:
        status, body = None, repr(exc).encode()
    return {"status": status, "elapsed_s": round(time.monotonic() - t0, 2), "body": body.decode("utf-8", "replace")}


def _log_since(path: Path, offset: int) -> bytes:
    if not path.exists():
        return b""
    with open(path, "rb") as fh:
        fh.seek(offset if path.stat().st_size >= offset else 0)
        return fh.read()


def _start_with_env(lab, comp: str, env: Mapping[str, str]) -> None:
    """``labctl.py start <comp>`` with the home's fixed token plus ``env`` through the knob ``GW_ENV``/``SERVE_ENV``
    (``lab/patches/labctl-L3.md``); retried like ``Lab.start`` when the plugin was skipped in that process."""
    gateway = comp.startswith("gw-")
    inst = comp.split("-", 1)[1]
    port = _lab.PORTS.get(comp)
    if port and not _lab.wait_for(lambda: _lab.port_free(port), 90, poll=0.5):
        raise _lab.LabError(f"port {port} of {comp} is still in use after 90 s")
    extra = {
        "GW_TOKEN" if gateway else "SERVE_TOKEN": lab.tokens[inst],
        "GW_ENV" if gateway else "SERVE_ENV": json.dumps(dict(env)),
    }
    for attempt in range(3):
        log = lab.log_path(comp)
        offset = log.stat().st_size if log.exists() else 0
        res = lab.labctl("start", comp, extra_env=extra, timeout=300)
        if not lab.running(comp):
            raise _lab.LabError(f"{comp} did not come up with {sorted(env)}: {res.stdout[-1200:]} {res.stderr[-600:]}")
        # the same plugin-loaded probe Lab.start uses (version-probe flake, PATH.md "Stolpersteine")
        if lab._plugin_loaded(comp, log, offset) or attempt == 2:
            return
        lab.labctl("stop", comp, timeout=300)


def _turn_reaches_model(lab, ev, tag: str, inst: str, sid: str) -> tuple[_lab.Turn, int]:
    probe = f"{tag.upper()}-FREE-{_lab.uid()}"
    mark = lab.llm_mark()
    turn = lab.turn(inst, sid, probe, timeout=180)
    reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
    ev.write(f"{tag}-turn.json", {"turn": turn.brief(), "reached": len(reached)})
    return turn, len(reached)


def _big_source(lab, name: str) -> _lab.Source:
    """A session on A whose repository carries 120 MiB of untracked random data (a transfer of several seconds)."""
    src = lab.make_source("a", name, turns=("a long confirmation",))
    for i in range(BIG_FILES):
        src.rb.write(f"data/blob{i}.bin", os.urandom(BIG_FILE_MIB * MiB))
    return src


def _conductor_job(lab, inst: str, after: float, mode: str, timeout: float = 120.0) -> dict | None:
    """The newest conductor row of ``mode`` created at or after ``after`` (ledger, read-only)."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        rows = lab.ledger_rows(
            inst,
            "SELECT * FROM jobs WHERE role = 'conductor' AND mode = ? AND created_at >= ? "
            "ORDER BY created_at DESC LIMIT 1",
            (mode, after),
        )
        if rows:
            return rows[0]
        time.sleep(0.1)
    return None


def _swept(lab, jid: str) -> bool:
    """A's source row of ``jid`` was ended by the sweep before any lock (``engine/participant.py:424-429``: transition
    to ``released`` with reason ``expired``) and no ``source_handovers`` row exists (nothing was ever held)."""
    if _state(lab, "a", jid, "source") != "released" or lab.source_row("a", jid) is not None:
        return False
    for e in lab.events("a", jid, "source"):
        if e.get("kind") == "transition" and e.get("state") == "released" and "expired" in str(e.get("data")):
            return True
    return False


def _settle_pull(lab, ev, jid: str | None) -> None:
    """Clean-up, not under test: a pull of B this test left unfinished is cancelled and resumed through B's gateway, so
    it holds no participant slot of A for the following tests. Waits ≤ 120 s."""
    if not jid or _state(lab, "b", jid) in _lab.STOP_STATES:
        return
    ev.cli("settle-cancel", lab.handover("b", "cancel", jid, timeout=180))
    scratch = lab.new_session("b", lab.work("b"))
    out, _ = lab.slash("b", scratch, f"/handover resume {jid}", timeout=180)
    ev.write("settle-resume.txt", out)
    _lab.wait_for(lambda: _state(lab, "b", jid) in _lab.STOP_STATES, 120, poll=1.0)
    ev.write("settle-state.json", {"b": lab.job_row("b", jid), "a": lab.job_row("a", jid, "source")})


class _PtyCli:
    """``hermes <args>`` of a lab home under a pseudo-terminal (stdin, stdout and stderr on the slave): the plugin's
    CLI asks its confirmation only when stdin and stdout are terminals (``engine/commands.py:1904-1925``,
    ``_tty_asker``); ``Lab.hermes`` runs with ``stdin=DEVNULL``, where an unanswered confirmation cancels at once.
    The output is copied to ``log`` as it arrives; ``close`` ends the process group by PID (SIGTERM, then SIGKILL)."""

    def __init__(self, lab, inst: str, args: list[str], log: Path) -> None:
        self.args = list(args)
        self._buf = bytearray()
        self._lock = threading.Lock()
        self._log = open(log, "ab")  # noqa: SIM115 - closed in close()
        self.master, slave = pty.openpty()
        env = _lab._child_env(lab.ctl.env_for(inst, TERM="dumb"))
        try:
            self.proc = subprocess.Popen(  # noqa: S603 - the lab's hermes executable, argv built by the test
                [str(lab.ctl.HERMES), *self.args],
                env=env,
                cwd=str(lab.work(inst)),
                stdin=slave,
                stdout=slave,
                stderr=slave,
                start_new_session=True,
                close_fds=True,
            )
        finally:
            os.close(slave)
        self._thread = threading.Thread(target=self._read, name="e2e-r4-pty", daemon=True)
        self._thread.start()

    def _read(self) -> None:
        while True:
            try:
                ready, _, _ = select.select([self.master], [], [], 0.5)
            except (OSError, ValueError):
                return
            if not ready:
                continue
            try:
                data = os.read(self.master, 65536)
            except OSError:  # EIO once the child closed the terminal
                return
            if not data:
                return
            with self._lock:
                self._buf += data
            with contextlib.suppress(OSError, ValueError):
                self._log.write(data)
                self._log.flush()

    def text(self) -> str:
        with self._lock:
            return self._buf.decode("utf-8", errors="replace")

    def json_doc(self) -> dict:
        """The ``--json`` document of the run (last JSON line of the output, control sequences removed)."""
        return _lab.parse_json_tail(ANSI.sub("", self.text()).replace("\r", ""))

    def wait_text(self, pred: Callable[[str], bool], timeout: float) -> bool:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if pred(self.text()):
                return True
            if self.proc.poll() is not None:
                self._thread.join(5)
                return pred(self.text())
            time.sleep(0.2)
        return pred(self.text())

    def send(self, text: str) -> None:
        os.write(self.master, text.encode())

    def wait(self, timeout: float) -> int | None:
        try:
            rc = self.proc.wait(timeout)
        except subprocess.TimeoutExpired:
            return None
        self._thread.join(10)
        return rc

    def close(self) -> None:
        _lab.kill_tree(self.proc, grace=20.0)
        self._thread.join(10)
        with contextlib.suppress(OSError):
            os.close(self.master)
        with contextlib.suppress(OSError, ValueError):
            self._log.close()


def _pty_cli(lab, ev, inst: str, args: list[str], tag: str, ready: Callable[[str], bool]) -> _PtyCli:
    """Start ``_PtyCli`` and wait (≤ ``ASK_TIMEOUT_S``) until its output satisfies ``ready``; retried twice when the
    plugin was skipped in that process (version-probe flake, "'handover' is not a `hermes` command", PATH.md)."""
    for attempt in range(3):
        cli = _PtyCli(lab, inst, args, ev.root / f"{tag}-pty-{attempt}.log")
        if cli.wait_text(ready, ASK_TIMEOUT_S):
            return cli
        out = cli.text()
        cli.close()
        if "is not a `hermes` command" in out and attempt < 2:
            continue
        raise _lab.LabError(f"scaffold: no interactive confirmation within {ASK_TIMEOUT_S:.0f} s: {out[-3000:]}")
    raise _lab.LabError("unreachable")


def _ns_child(pid: int) -> int | None:
    """The forked child of ``unshare --fork`` (PID 1 of the new namespace) as this namespace sees it."""
    try:
        kids = Path(f"/proc/{pid}/task/{pid}/children").read_text().split()
    except OSError:
        return None
    return int(kids[0]) if kids else None


class _NsServe:
    """``hermes serve`` of home ``inst`` in a PID namespace of its own (``unshare --pid --fork --mount-proc
    --kill-child``), on the stand's port and token, with the lab environment of ``labctl.env_for`` — the separate
    dashboard container of Hermes' docker-compose. It sees the gateway's ``gateway.pid``/``gateway_state.json`` and
    the held runtime lock (shared files), but not its process. Not registered with ``labctl.py`` (no pid file): the
    test ends it with ``stop`` (process group SIGTERM, then SIGKILL; ``--kill-child`` takes the namespace along when
    ``unshare`` dies); its output is appended to ``run/serve-<x>.log``, so ``conftest.ev`` keeps it."""

    def __init__(self, lab, ev, inst: str) -> None:
        self.lab, self.ev, self.inst = lab, ev, inst
        self.proc: subprocess.Popen | None = None
        self.child: int | None = None
        self.port = _lab.PORTS[f"serve-{inst}"]
        self.url = _lab.SERVE_URL[inst]

    def start(self) -> dict:
        unshare = shutil.which("unshare")
        if not unshare:
            raise _lab.LabError("scaffold: unshare (util-linux) is not available on the lab host")
        argv = [unshare, "--pid", "--fork", "--mount-proc", "--kill-child"]
        argv += [str(self.lab.ctl.HERMES), "serve", "--host", "127.0.0.1", "--port", str(self.port)]
        token = self.lab.tokens[self.inst]
        env = _lab._child_env(self.lab.ctl.env_for(self.inst, HERMES_DASHBOARD_SESSION_TOKEN=token))
        log = self.lab.log_path(f"serve-{self.inst}")
        hello: dict = {}
        for attempt in range(3):
            if not _lab.wait_for(lambda: _lab.port_free(self.port), 90, poll=0.5):
                raise _lab.LabError(f"scaffold: port {self.port} is still in use after 90 s")
            offset = log.stat().st_size if log.exists() else 0
            with open(log, "ab") as fh:  # the child holds its own descriptor
                stamp = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
                fh.write(f"\n=== {stamp} start {argv} (test_l3_round4: own PID namespace)\n".encode())
                fh.flush()
                self.proc = subprocess.Popen(  # noqa: S603 - fixed unshare + the lab's hermes, argv built here
                    argv,
                    env=env,
                    cwd=str(self.lab.work(self.inst)),
                    stdin=subprocess.DEVNULL,
                    stdout=fh,
                    stderr=subprocess.STDOUT,
                    start_new_session=True,
                )

            def up(offset: int = offset) -> bool:
                nonlocal hello
                hello = _hello(self.url, token, timeout=5.0)
                return hello["status"] == 200 or self.proc.poll() is not None or SKIPPED_LINE in _log_since(log, offset)

            _lab.wait_for(up, 180, poll=0.5)
            self.child = _ns_child(self.proc.pid)
            if hello.get("status") == 200 and self.child:
                return {"attempt": attempt, "unshare_pid": self.proc.pid, "child": self.child, "hello": hello}
            self.ev.write(f"ns-serve-attempt-{attempt}.json", {"hello": hello, "rc": self.proc.poll()})
            self.stop()
        raise _lab.LabError(f"scaffold: hermes serve in its own PID namespace did not serve ctx_handover: {hello}")

    def stop(self) -> None:
        child = self.child
        _lab.kill_tree(self.proc, grace=20.0)
        _lab.kill_pid(child, grace=10.0)
        self.proc = None
        self.child = None
        if not _lab.wait_for(lambda: _lab.port_free(self.port), 60, poll=0.5):
            raise _lab.LabError(f"scaffold: port {self.port} still in use after the namespaced serve ended")


_IDENTITY_PROBE = """
import json, os, sys
from pathlib import Path
from gateway.status import live_gateway_pid_for_home, read_runtime_status, runtime_status_is_stale
home = Path(sys.argv[1])
record = read_runtime_status(home / "gateway_state.json")
print(json.dumps({
    "probe_pid": os.getpid(),
    "live_gateway_pid": live_gateway_pid_for_home(home),
    "record_pid": (record or {}).get("pid"),
    "record_state": (record or {}).get("gateway_state"),
    "record_stale": runtime_status_is_stale(record),
    "record_argv": (record or {}).get("argv"),
}))
"""
"""Hermes' own identity check (``gateway.status.live_gateway_pid_for_home`` — the call ``engine/hostcompat.py:578-615``
``_gateway_view`` makes; read-only, ``cleanup_stale=False``) and the freshness test of the runtime record, run in the
namespace of the separate serve."""


def _identity_inside(lab, ns_pid: int, inst: str) -> dict:
    """``_IDENTITY_PROBE`` through ``nsenter --target <ns_pid> --pid --mount`` with the lab environment of ``inst``."""
    nsenter = shutil.which("nsenter")
    if not nsenter:
        raise _lab.LabError("scaffold: nsenter (util-linux) is not available on the lab host")
    env = _lab._child_env(lab.ctl.env_for(inst))
    argv = [nsenter, "--target", str(ns_pid), "--pid", "--mount", "--", lab.py, "-c", _IDENTITY_PROBE]
    res = subprocess.run(  # noqa: S603 - fixed nsenter + the lab's interpreter, probe code of this file
        [*argv, str(lab.home(inst))],
        env=env,
        cwd=str(lab.work(inst)),
        capture_output=True,
        text=True,
        timeout=120,
        stdin=subprocess.DEVNULL,
        check=False,
    )
    doc = _lab.parse_json_tail(res.stdout)
    return {"rc": res.returncode, **doc, "stderr": res.stderr[-1500:]}


def _chat_processes(sid: str) -> list[dict]:
    """Live processes whose command line holds ``chat`` and the session id (a continuation child, R10)."""
    out = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit() or int(entry) == os.getpid():
            continue
        try:
            argv = [a.decode(errors="replace") for a in Path(f"/proc/{entry}/cmdline").read_bytes().split(b"\0") if a]
        except OSError:
            continue
        if "chat" in argv and any(sid in a for a in argv) and _lab.pid_alive(int(entry)):
            out.append({"pid": int(entry), "argv": argv[:14]})
    return out


def _messages_blob(rec: Mapping) -> str:
    return json.dumps((rec.get("body") or {}).get("messages") or [], ensure_ascii=False)


# ---------------------------------------------------------------------------------------------------------
# R3-H1: a pull aborted after a confirmation that outlived the source's staging
# ---------------------------------------------------------------------------------------------------------


def _backdate_source_row(lab, ev, jid: str) -> dict:
    """Lab manipulation of A's ledger *state*, not of product code (fast variant of R3-H1): one ``UPDATE`` of table
    ``jobs``, column ``updated_at``, on A's source row of ``jid`` (``role = 'source'``), to ``now - staging_ttl -
    BACKDATE_MARGIN_S``. ``updated_at`` is exactly what the sweep's staging rule reads for a source row that never
    locked anything (``engine/participant.py:246-254`` ``_unlocked_idle``, ``:424-429``); a real confirmation of
    ``staging_ttl`` gets it there by waiting. Compare-and-set on the value read just before (``rowcount`` 1), so a
    request that moved the row in between is a scaffold error, not a silent overwrite. The ledger is a shared SQLite
    file the plugin itself opens from several processes (CLI, gateway, serve; ``engine/ledger.py:291``
    ``busy_timeout``); this write uses a busy timeout too. Nothing else is changed (no ``ops``, ``events`` or
    ``source_handovers`` row)."""
    path = lab.data_dir("a") / "ledger.sqlite3"
    before = lab.job_row("a", jid, "source") or {}
    old = before.get("updated_at")
    new = time.time() - TTL_S - BACKDATE_MARGIN_S
    conn = sqlite3.connect(str(path), timeout=10.0, isolation_level=None)
    try:
        conn.execute("PRAGMA busy_timeout=10000")
        cur = conn.execute(
            "UPDATE jobs SET updated_at = ? WHERE job_id = ? AND role = 'source' AND updated_at = ?",
            (new, jid, old),
        )
        changed = cur.rowcount
    finally:
        conn.close()
    doc = {
        "ledger": str(path),
        "table": "jobs",
        "column": "updated_at",
        "where": {"job_id": jid, "role": "source", "updated_at": old},
        "old": old,
        "new": new,
        "moved_by_s": (float(old) - new) if old is not None else None,
        "rows": changed,
        "at": time.time(),
        "after": lab.job_row("a", jid, "source"),
    }
    ev.write("backdate.json", doc)
    if changed != 1:
        raise _lab.LabError(f"scaffold: A's source row changed before the backdate (no compare-and-set match): {doc}")
    return doc


def _r4_h1(lab, ev, *, realtime: bool) -> dict:
    """The scenario of the two R3-H1 tests up to the observations; ``realtime`` chooses how A's source row gets older
    than ``staging_ttl`` while B's confirmation is open (waiting ≈ 61 min, or ``_backdate_source_row``)."""
    cli: _PtyCli | None = None
    jid: str | None = None
    src: _lab.Source | None = None
    cancel: _lab.CliResult | None = None
    timeline: dict = {"variant": "realtime" if realtime else "fast"}
    with lab.preserved("a", restart=("gw-a", "serve-a")), lab.preserved("b"):
        lab.setting("a", "staging_ttl_hours", str(TTL_HOURS))
        lab.setting("b", "confirm_above_mb", "0")
        ev.write("config-a.yaml", lab.config_path("a").read_text(encoding="utf-8"))
        ev.write("config-b.yaml", lab.config_path("b").read_text(encoding="utf-8"))
        lab.restart("serve-a")
        hello = _hello(_lab.SERVE_URL["a"], lab.tokens["a"], timeout=30.0)
        ev.write("hello-serve-a.json", hello)
        if hello["status"] != 200:
            raise _lab.LabError(f"scaffold: serve-a's ctx_handover listener did not start: {hello}")
        src = _big_source(lab, f"r4h1proj{lab.run_id}")
        try:
            t0 = time.time()
            cli = _pty_cli(
                lab,
                ev,
                "b",
                ["handover", "take", _lab.SERVER, src.sid, "--json"],
                "take",
                lambda t: "Choice:" in t and "Yes, hand it over" in t,
            )
            timeline["asked"] = time.time()
            job = _conductor_job(lab, "b", t0, "pull", timeout=60)
            if job is None:
                raise _lab.LabError("scaffold: the pull asked, but B's ledger holds no pull conductor job")
            jid = str(job["job_id"])
            row_a = lab.job_row("a", jid, "source") or {}
            asked = {
                "job": jid,
                "b_state": _state(lab, "b", jid),
                "a_source_job": row_a,
                "a_source_handover": lab.source_row("a", jid),
            }
            ev.write("asked.json", asked)
            if not row_a or row_a.get("state") in ("released", "reclaimed") or asked["a_source_handover"] is not None:
                raise _lab.LabError(f"scaffold: A's source row is not an open, never-locked pull source: {asked}")
            if asked["b_state"] != "negotiated":
                raise _lab.LabError(f"scaffold: B's conductor is not waiting at the confirmation: {asked}")
            swept = False
            if realtime:
                due = float(row_a["updated_at"]) + TTL_S
                timeline["sweep_due"] = due
                while not swept and cli.proc.poll() is None:
                    swept = _swept(lab, jid)
                    # due from the row's last request (a request after the question would move it; none is expected)
                    last = float((lab.job_row("a", jid, "source") or {}).get("updated_at") or row_a["updated_at"])
                    due = max(due, last + TTL_S)
                    if swept or time.time() > due + SWEEP_MARGIN_S:
                        break
                    time.sleep(5.0)
                timeline["sweep_due_last"] = due
            else:
                timeline["backdated"] = _backdate_source_row(lab, ev, jid)["at"]
                deadline = time.monotonic() + FAST_SWEEP_WAIT_S
                while not swept and cli.proc.poll() is None and time.monotonic() < deadline:
                    swept = _swept(lab, jid)
                    if not swept:
                        time.sleep(1.0)
            timeline["swept_seen"] = time.time()
            after_sweep = {
                "swept": _swept(lab, jid),
                "cli_rc": cli.proc.poll(),
                "b_state": _state(lab, "b", jid),
                "a_source_job": lab.job_row("a", jid, "source"),
                "a_source_handover": lab.source_row("a", jid),
                "a_events": lab.events("a", jid, "source"),
                "a_ops": _ops(lab, "a", jid),
            }
            ev.write("after-sweep.json", after_sweep)
            if not swept or not after_sweep["swept"] or after_sweep["cli_rc"] is not None:
                raise _lab.LabError(f"scaffold: the sweep did not end A's source row while B waited: {after_sweep}")
            if after_sweep["b_state"] != "negotiated":
                raise _lab.LabError(f"scaffold: B's conductor left the confirmation by itself: {after_sweep}")
            cli.send("1\n")
            timeline["answered"] = time.time()
            seen: list[str] = []
            deadline = time.monotonic() + 600
            while time.monotonic() < deadline:
                st = _state(lab, "b", jid)
                if st and (not seen or seen[-1] != st):
                    seen.append(st)
                if st in CANCEL_WINDOW:
                    cancel = lab.handover("b", "cancel", jid, timeout=180)
                    ev.cli("cancel", cancel)
                    timeline["cancelled"] = time.time()
                    break
                if st in AFTER_ANSWER_END or cli.proc.poll() is not None:
                    break
                time.sleep(0.02)
            rc = cli.wait(900)
            final = lab.wait_job("b", jid, _lab.STOP_STATES | {"suspended", "in_doubt"}, timeout=300, poll=0.5)
            timeline["ended"] = time.time()
            out = cli.json_doc()
            source_a = lab.source_row("a", jid)
            job_a = lab.job_row("a", jid, "source") or {}
            marks = {s: lab.marks("a", s) for s in lab.lineage("a", src.sid)}
            tip_title = lab.marks("a", lab.tip("a", src.sid)).get("title")
            ev.write(
                "after-answer.json",
                {
                    "states_seen": seen,
                    "cancelled": cancel is not None,
                    "cli_rc": rc,
                    "final": final,
                    "cli_json": out,
                    "conductor_b": lab.job_row("b", jid),
                    "events_b": lab.events("b", jid),
                    "source_a": source_a,
                    "job_a": job_a,
                    "events_a": lab.events("a", jid, "source"),
                    "ops_a": _ops(lab, "a", jid),
                    "marks_a": marks,
                    "tip_title": tip_title,
                },
            )
            turn, reached = _turn_reaches_model(lab, ev, "r4h1", "a", lab.tip("a", src.sid))
        finally:
            ev.write("timeline.json", timeline)
            if cli is not None:
                _cleanup(ev, "pty", cli.close)
            _cleanup(ev, "settle", lambda: _settle_pull(lab, ev, jid))
            if src is not None and _source_state(lab, "a", jid) in ACTIVE_SOURCE_STATES:
                _cleanup(
                    ev,
                    "reclaim",
                    lambda: ev.cli("cleanup-reclaim-force", lab.handover("a", "reclaim", src.sid, "--force")),
                )
    return {
        "final": final,
        "seen": seen,
        "out": out,
        "source_a": source_a,
        "job_a": job_a,
        "marks": marks,
        "tip_title": tip_title,
        "title": src.title,
        "turn": turn,
        "reached": reached,
        "cancel": cancel,
    }


def _assert_r4_h1(r: dict) -> None:
    """The Soll of R3-H1 (docstring of the fast test), the same for both variants."""
    final, seen, out, source_a, job_a = r["final"], r["seen"], r["out"], r["source_a"], r["job_a"]
    assert final == "aborted", (final, seen, out)
    assert source_a is None or source_a.get("state") in GIVEN_BACK, (
        f"B reports the pull aborted (source released), but A's source handover is {source_a.get('state')!r}"
    )
    assert job_a.get("state") in GIVEN_BACK, job_a
    for seg, m in r["marks"].items():
        assert not m["ended"] and not m["hidden"] and not m["archived"], (seg, m)
    assert r["tip_title"] == r["title"], (r["tip_title"], r["title"])
    turn = r["turn"]
    assert turn.ok and r["reached"] and "/handover reclaim" not in turn.text, turn.brief()
    assert r["cancel"] is None, f"the late lock was accepted: the job reached {seen} and had to be cancelled"
    assert out.get("state") == "aborted" and out.get("exit_code") != 0, out
    assert (out.get("error") or {}).get("code") == "invalid_state", out
    assert "already ended (released) before it locked the session; start the handover again" in str(out.get("text")), (
        out.get("text")
    )


@pytest.mark.e2e_timeout(1800)
def test_r4_h1_pull_abort_after_a_long_confirmation_reports_released_only_when_it_is(lab, ev):
    """R3-H1 (REVIEW-3 §3, High, class B1; DECISIONS R37, contract V37): a pull aborted after its source row was
    ended by the staging sweep reports "released" only when the source holds nothing. Fast variant (the normal case;
    the same scenario in real time: ``test_r4_h1_realtime_…``, opt-in ``CTXH_E2E_R4H1_REALTIME=1``).

    Steps: A's ``staging_ttl_hours`` = 1 (``serve-a`` restarted, its listener started with an unsigned hello); B's
    ``confirm_above_mb`` = 0 (reason (c), ``engine/plan.py:1806-1808``: every plan with bytes asks). A session on A with
    its own repository and 120 MiB of untracked data. On B ``hermes handover take server <sid> --json`` without
    ``--yes`` under a pseudo-terminal: the conductor plans (route #7 opens A's source row at FACTS, no lock) and asks
    "Choice:" (``engine/commands.py:628-672``, ``:1904-1925``); the M3 keepalive keeps B's conductor alive meanwhile
    (``engine/conductor.py:522-546``) and nothing touches A's row. While B's CLI still waits, the test moves A's source
    row ``staging_ttl`` + 600 s into the past (``_backdate_source_row``: table ``jobs``, column ``updated_at``, the
    column the staging rule reads) and waits ≤ 120 s for the next tick of A's sweep (``engine/jobs.py:81``
    ``MAINTENANCE_S = 10``, loop ``:707-720``), which ends that row (``engine/participant.py:424-429``:
    ``start_release(job, "expired")``) — precondition, checked from A's ledger (row ``released`` with a transition
    ``expired``, no ``source_handovers`` row) while B's CLI still waits. Then the answer "1" (Yes, hand it over). If
    B's conductor reaches a state after the lock (``CANCEL_WINDOW``), ``hermes handover cancel <job>`` on B aborts it
    there (the "Abbruch" of the review); if it ends on its own, nothing is cancelled.

    Why backdating and not a clock: the seams exist (``engine/ledger.py:249`` ``Ledger(clock=…)``,
    ``engine/participant.py:117`` ``ParticipantRegistry(clock=…)``), but the sweep runs in A's ``hermes serve``, which
    builds its ledger with the default ``time.time`` (``engine/jobs.py:259``) and its maintenance loop passes
    ``prof.ledger.now()`` (``:714-717``); no setting or environment variable reaches them, and the plugin in the lab is
    the unpatched copy. A process-wide clock shift (e.g. ``libfaketime``) would also move every signed timestamp A
    sends and receives. Backdating the one column is what a real confirmation of ≥ 1 h does to that row, and nothing
    else on A changes during that hour in the real-time run (B's keepalive does not touch A's row; A's janitor leaves
    a never-locked source row alone, ``engine/participant.py:447-485`` ``recover``: only ``locking``/``locked``/
    ``preparing``/``releasing`` are released).

    Soll — R37 ("„released“ … entstehen ausschließlich aus einer frischen, ausdrücklich positiven Operation der
    aktuellen Haltung"), REVIEW-3 Vorschlag ("Sweep-Release → später Lock → … → Abbruch ⇒ Ledger ``released``,
    Session sichtbar"), BRIEF D8 ("Fehlschlag → Quelle unverändert"):

    * B's conductor ends ``aborted`` — which it reaches only after a release that reports ``released``/``reclaimed``
      (``engine/conductor.py:1756-1829``), i.e. B's claim that A gave the session back;
    * A agrees: its source job row is ``released`` and A holds no lineage for the job (no ``source_handovers`` row, or
      one in ``released``/``reclaimed``) — ``engine/local_source.py:827-873`` (``start_release``: a stored release
      answers only the hold it belongs to, ``_release_hold`` ``:875-882``; a stored ``released`` next to an active
      hold is released again, ``:868-872``);
    * A's session is not ended, hidden or archived and keeps its title; a turn there reaches the model without the
      veto text (``/handover reclaim``);
    * the late lock itself is refused (``engine/local_source.py:504-518``): B's conductor never gets past
      ``locking`` (no cancel was needed), its ``--json`` reports state ``aborted`` with ``error.code`` ``invalid_state``
      and the text names "already ended (released) before it locked the session; start the handover again"
      (``engine/commands.py:848-853`` → ``engine/surfaces.py:1550-1551``).

    Expected red on ``88068475`` (read, not run): ``LocalSource.lock`` has no ended-check and ``source_lock`` inserts a
    new ``source_handovers`` row without a ``relock`` event, so the hold count stays 0 (``engine/ledger.py``
    ``source_generation``); export and pack of the 120 MiB start and the test cancels in that window; the abort's
    ``start_release`` (``88068475:engine/local_source.py:803-834``) is keyed ``_digest("release", 0)`` — the key of
    the sweep's operation — and returns its stored ``done {'state': 'released'}``; B ends ``aborted`` ("Cancelled.
    Nothing was changed.") while A's ``source_handovers`` row stays ``locked`` (the leases hold the session until they
    run out) — the assertion on A's source state fails first.

    Abweichungen vom gewünschten Szenario: (1) the confirmation is held only until the sweep after the backdate (tens
    of seconds), not ≥ 1 h; what an hour would change is the row's age, and that is set directly (above). A's sweep
    with a 1-h TTL also ends unfinished leftovers older than 1 h of earlier tests while this test runs (``staging_ttl``
    for targets, ``engine/participant.py:435-437``; in the fast variant for ≈ 1–2 min). (2) The review's consequence is
    an abort after the source's prepare (``prepared``, archived, hidden); a cancel from outside cannot be placed
    between the source's prepare and the commit, so the red path cancels in the transfer window and shows ``locked``
    instead. (3) The open confirmation is the CLI's TTY prompt (``_tty_asker``), not ``ask_choice``: the gateway's
    clarify times out after 90 s (``engine/conductor.py:527-529``).

    Clean-up, not under test: B's unfinished job is cancelled and resumed (``_settle_pull``); a source that still
    holds A's session is reclaimed with ``--force``; settings of both homes restored, ``gw-a`` and ``serve-a``
    restarted. Hard bound 1800 s."""
    _assert_r4_h1(_r4_h1(lab, ev, realtime=False))


@pytest.mark.skipif(
    os.environ.get(REALTIME_ENV) != "1",
    reason=f"real-time R3-H1 holds the confirmation ≈ 61 min; opt-in {REALTIME_ENV}=1 (the fast variant runs always)",
)
@pytest.mark.e2e_timeout(TTL_S + 1800)
def test_r4_h1_realtime_confirmation_outlives_the_staging_ttl(lab, ev):
    """R3-H1 in real time (opt-in ``CTXH_E2E_R4H1_REALTIME=1``, ≈ 63–68 min): the scenario, the Soll and the expected
    red of ``test_r4_h1_pull_abort_after_a_long_confirmation_reports_released_only_when_it_is``, except that A's
    source row ages by waiting: the confirmation is held until ``updated_at`` + 3600 s plus at most one 10-s sweep
    (``staging_ttl_hours`` cannot go below 1, ``engine/settings.py:113-114``, ``:207-208``; no shorter knob:
    ``participant_idle_timeout_min`` acts only on a locked source, ``engine/participant.py:413-420``), with
    ``SWEEP_MARGIN_S`` as the precondition window. No ledger write. Hard bound 3600 s + 1800 s."""
    _assert_r4_h1(_r4_h1(lab, ev, realtime=True))


# ---------------------------------------------------------------------------------------------------------
# R3-H2: a gateway the serving process cannot identify
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(1200)
def test_r4_h2_continuation_refused_when_the_gateway_cannot_be_identified(lab, ev):
    """R3-H2 (REVIEW-3 §3, High; R22, R37 "nicht identifizierbare Gegenstelle … verweigert sicherheitsrelevante Starts
    (fail-closed)"): the process that would start the continuation cannot identify the gateway that runs the turn —
    nothing starts, the reason is ``approvals_unchecked``.

    Setup: A allows ``allow_remote_continue``. ``gw-a`` is started through ``labctl.py`` with ``HERMES_YOLO_MODE=1``
    in its environment only (knob ``GW_ENV``; its command line has no ``--yolo``). ``serve-a`` — where the
    ``…/continue`` route and the spawner run (``engine/jobs.py:499-504``) — runs without the variable in a PID namespace
    of its own (``_NsServe``), as the dashboard container of Hermes' docker-compose does. Preconditions, checked:
    the variable is in ``gw-a``'s environment and not in the serve's; the two processes are in different PID
    namespaces; inside the serve's namespace Hermes' identity check finds no live gateway of A
    (``live_gateway_pid_for_home`` None) while A's runtime record is fresh and names a running gateway (the
    ``unverified`` branch of ``engine/hostcompat.py:607-612``). Then B pushes a session with ``--continue "<prompt>"
    --yes``.

    Soll — R22 ("startet nicht, wenn am Ziel Approvals abgeschaltet sind"), R37 (fail-closed for a counterpart that
    cannot be identified), REVIEW-3 Vorschlag ("„Nicht prüfbar“ verweigert eine ferngesteuerte Fortsetzung"):

    * the handover itself ends ``done`` and the session is active on A (R10: the continuation is optional);
    * B's job records the continuation as not started with ``continue_refused``, ``details.reason``
      ``approvals_unchecked`` and ``details.why`` ``gateway_unverified`` (``engine/hostcompat.py:650``,
      ``:675-683``; the spawner adds the way out, ``engine/continuation.py:218-222``);
    * B's message names the cause and the command to start the turn by hand there: "cannot identify the gateway that
      runs the turn" and ``hermes -p default chat --resume <A's session id>`` (``engine/surfaces.py:1337-1345``,
      ``:1348-1364``, ``:1471-1472``);
    * no continuation process exists (no live ``chat`` command line with A's session id), no fake-model request
      carries the prompt, and within 15 s after the handover no main request carries the session (its tag).

    Is this the meant case? Yes for the namespace setup: ``approvals_off`` raises ``ContinueRefused``
    (``approvals_unchecked``) exactly for ``ApprovalCheck.why`` in ``UNIDENTIFIED_WHY`` = ``gateway_unverified``
    (a fresh record of a running gateway whose process this process cannot identify — another container or PID
    namespace, another user) and ``gateway_unknown`` (gateway status unreadable), ``engine/hostcompat.py:539-542``.
    Abweichung vom gewünschten Szenario: the literal setup "YOLO only in ``gw-a``'s environment, ``serve-a`` without,
    both in one PID namespace" is *not* that case on ``ad5a5cfa``: the serve identifies ``gw-a`` (``gateway_elsewhere``,
    ``engine/hostcompat.py:645-646``), the variable in the gateway's environment is invisible to it, and
    ``approvals_off`` returns False with a warning log line (``:684-691``) — the continuation starts (R33a's named
    limit, README "leave allow_remote_continue off there"; no target behaviour to assert). The namespace is what
    makes the gateway unidentifiable, so the test separates the serve that way; the gateway's YOLO environment stays as
    asked (it is what an unattended turn would inherit). The ``--yolo`` command-line case (``gateway_flag`` on an
    unverified record, ``:650``, reason ``approvals_off``) cannot be built: ``labctl.py`` starts the gateway as
    ``hermes gateway run``.

    Expected red on ``88068475`` (read, not run): ``approvals_off`` was ``approval_check(config).off is True``
    (``88068475:engine/hostcompat.py:637-641``) and ``gateway_unverified`` has ``off`` None, so the spawner started
    ``hermes -p default chat --resume <id> --query=…`` from the namespaced serve; that client sees no gateway of the
    profile there and attaches or starts one (upstream ``hermes_cli/gateway_client.py`` → ``ensure_gateway_runtime(…,
    idle_exit=True)``) — the turn reaches the model, ``started`` is true and no ``approvals_unchecked`` is recorded.

    Side effect named: inside its namespace the dashboard may treat A's ``gateway.pid`` as stale (upstream
    housekeeping); ``gw-a`` is restarted through ``labctl.py`` afterwards and writes it anew. Clean-up: continuation
    processes ended by PID, the namespaced serve ended by process group, A's settings restored, ``gw-a`` and
    ``serve-a`` restarted through ``labctl.py`` without the variable. Hard bound 1200 s."""
    serve: _NsServe | None = None
    pids: list[int] = []
    name = f"r4h2proj{lab.run_id}"
    tag = f"[{name}]"
    with lab.preserved("a", restart=("gw-a", "serve-a")):
        lab.setting("a", "allow_remote_continue", "true")
        ev.write("config-a.yaml", lab.config_path("a").read_text(encoding="utf-8"))
        lab.stop("gw-a", "serve-a")
        try:
            _start_with_env(lab, "gw-a", {YOLO: "1"})
            gw_pid = lab.pid("gw-a")
            serve = _NsServe(lab, ev, "a")
            started = serve.start()
            facts = {
                "started": started,
                "gw_a": {"pid": gw_pid, "pidns": _pidns(gw_pid), YOLO: _proc_env(gw_pid).get(YOLO)},
                "serve_a": {"pid": serve.child, "pidns": _pidns(serve.child), YOLO: _proc_env(serve.child).get(YOLO)},
                "test_pidns": _pidns(os.getpid()),
                "identity_inside": _identity_inside(lab, serve.child, "a"),
            }
            ev.write("scaffold.json", facts)
            inside = facts["identity_inside"]
            if facts["gw_a"][YOLO] != "1" or facts["serve_a"][YOLO] is not None:
                raise _lab.LabError(f"scaffold: {YOLO} is not set only in gw-a: {facts}")
            if not facts["serve_a"]["pidns"] or facts["serve_a"]["pidns"] in (facts["gw_a"]["pidns"], None):
                raise _lab.LabError(f"scaffold: the serve does not run in a PID namespace of its own: {facts}")
            if inside.get("rc") != 0 or inside.get("live_gateway_pid") is not None:
                raise _lab.LabError(f"scaffold: the namespaced serve can identify A's gateway: {inside}")
            if inside.get("record_stale") is not False or inside.get("record_state") in (None, "stopped"):
                raise _lab.LabError(f"scaffold: A's runtime record names no running gateway: {inside}")
            if inside.get("record_pid") != gw_pid:
                raise _lab.LabError(f"scaffold: A's runtime record is not gw-a's (pid {gw_pid}): {inside}")
            src = lab.make_source("b", name, turns=("closing on the laptop",))
            prompt = f"R4H2-CONTINUE-{_lab.uid()}"
            mark = lab.llm_mark()
            res = lab.handover("b", "to", _lab.SERVER, "--session", src.sid, "--continue", prompt, "--yes", timeout=600)
            ev.cli("push-continue", res)
            status = lab.job_status("b", res.job_id) if res.job_id else {"job": {}}
            ev.write("job-b.json", status)
            cont = status["job"].get("continuation") or {}
            if isinstance(cont, dict) and cont.get("pid"):
                pids.append(int(cont["pid"]))
            ack = status["job"].get("ack") if isinstance(status["job"].get("ack"), dict) else {}
            sid_a = str(ack.get("session_id") or "")
            after = lab.llm_mark()
            time.sleep(QUIET_S)
            late = [r for r in lab.llm_since(mark) if prompt in _messages_blob(r)]
            quiet = [r for r in lab.llm_since(after) if r.get("kind") == "main" and tag in _messages_blob(r)]
            spawned = _chat_processes(sid_a) if sid_a else []
            pids += [p["pid"] for p in spawned]
            row_a = lab.row("a", sid_a) if sid_a else {}
            ev.write(
                "after.json",
                {
                    "cont": cont,
                    "sid_a": sid_a,
                    "row_a": row_a,
                    "late": _lab.compact_records(late),
                    "quiet": _lab.compact_records(quiet),
                    "spawned": spawned,
                    "identity_inside_after": _identity_inside(lab, serve.child, "a") if serve.child else None,
                },
            )
        finally:
            for pid in pids:
                _cleanup(ev, f"kill-{pid}", lambda pid=pid: _lab.kill_pid(pid))
            if serve is not None:
                _cleanup(ev, "ns-serve", serve.stop)
    err = cont.get("error") if isinstance(cont, dict) and isinstance(cont.get("error"), dict) else {}
    details = err.get("details") if isinstance(err.get("details"), dict) else {}
    assert res.rc == 0 and status["job"].get("state") == "done", res.brief()
    assert row_a and row_a.get("ended_at") is None and not row_a.get("hidden"), row_a
    assert not (isinstance(cont, dict) and cont.get("started")), cont
    assert err.get("code") == "continue_refused" and details.get("reason") == "approvals_unchecked", cont
    assert details.get("why") == "gateway_unverified", cont
    assert "cannot identify the gateway that runs the turn" in res.text, res.text[-2500:]
    assert f"hermes -p default chat --resume {sid_a}" in res.text, res.text[-2500:]
    assert not spawned, f"a continuation process runs for {sid_a}: {spawned}"
    assert not late, "a continuation turn reached the model although the gateway could not be identified"
    assert not quiet, f"model requests of the handed-over session after the handover: {_lab.compact_records(quiet)}"
