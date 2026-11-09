"""Layer 3 — findings of the external code review (``review-code/REVIEW.md`` §2) end to end on the paired two-gateway
stand of ``conftest.lab``: B1, B2, H1, H2, H6, H7, H9, H11, H14 (all nine can be made in the lab; none is skipped).

Every test asserts the target behaviour after the fix — the review's "Vorschlag" plus ``design/DECISIONS.md`` — and is
expected red on ``2f5fd1d5`` (product state of ``feat/ctx-handover-e2e`` at ``1a0b76ac``). Each docstring names the
review id, the production place the review gives (line numbers re-read on ``2f5fd1d5``), why the test is red there
(read, not run) and the source of each assertion. A precondition the lab has to reach before the fault can show (a
janitor run, a re-lock, a window between two states) is checked and raises ``LabError`` — a scaffold problem, never
a product verdict.

Lab means as in ``test_e2e_faults.py``: pushes from A's CLI in the background (``hermes handover to laptopgw``), faults
by ``labctl.py stop``/``start`` or SIGSTOP/SIGCONT of a component's PID, a SIGKILL of the background CLI's process group
(B1), a fresh ``gw-a`` process for a janitor run (B1, H6: ``jobs.ensure_started`` runs the janitor once per process,
in background threads that only a long-lived process lets finish), a loopback HTTP catcher on a port of its own
(H1), and ``HERMES_YOLO_MODE=1`` for ``gw-a``/``serve-a`` through the new ``labctl.py`` knobs ``GW_ENV``/``SERVE_ENV``
(H2, ``lab/patches/labctl-L3.md``). Session workspaces are own repositories built with ``RepoBuilder`` under
``lab/ws-e2e/<x>``; plain directories appear only as ``--include`` extras (H9, H11), never as a session's cwd (L3-F19
would hand over the surrounding ctx checkout on ``2f5fd1d5``).

Settings a test changes are restored byte-exact (``Lab.preserved``) and components restarted through ``labctl.py``;
processes a test starts are ended by PID in ``finally``; evidence under ``lab/evidence/e2e-<date>/<test>/``.

Runtime estimate: 50–70 min (B1 ≈ 8 min with a 65-s stale wait and two gateway restarts, H6/H7 ≈ 5 min each with a
frozen target past the stale or idle window, B2 two variants with two handovers each, the others one handover each).
"""

from __future__ import annotations

import base64
import contextlib
import http.server
import json
import os
import posixpath
import secrets
import signal
import stat
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
GiB = 1024 * MiB
BIG_FILES = 3
BIG_FILE_MIB = 40
TRANSFER_STATES = frozenset({"exporting", "context_staged", "packing", "data_staged"})
WINDOW = TRANSFER_STATES | {"target_preparing"}
"""Conductor states with the source locked and not prepared, the conductor working on the transfer or waiting for the
target's prepare (``engine/conductor.py`` ``SUSPENDABLE`` without LOCKING/LOCKED/TARGET_PREPARED)."""
LATER = frozenset(
    {"target_prepared", "source_preparing", "source_prepared", "committing", "in_doubt", "done", "aborted", "suspended"}
)
SUSPENDABLE = WINDOW | {"locking", "locked", "target_prepared"}
"""``engine/conductor.py`` ``SUSPENDABLE``: a dead conductor's job there is suspended by the janitor (``recover``)."""
ACTIVE_SOURCE_STATES = frozenset({"locking", "locked", "preparing", "prepared", "retiring", "retired", "exported"})
"""Source states that hold a lineage (``engine/ledger.py`` ``ACTIVE_SOURCE_STATES``)."""
GIVEN_BACK = frozenset({"released", "reclaimed"})
"""Source states that give the session back (``engine/conductor.py`` ``SOURCE_GIVEN_BACK``)."""
REPO_KEYS = ("head", "branch", "status", "index", "refs", "stash", "tree")
YOLO = "HERMES_YOLO_MODE"
CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
SIZE_REFUSALS = frozenset({"payload_too_large", "insufficient_storage", "policy_denied"})
"""Error codes of a size refusal (``engine/errors.py``); git's ``gitapply._precheck`` raises the first two."""


# ---------------------------------------------------------------------------------------------------------
# Helpers (this file only)
# ---------------------------------------------------------------------------------------------------------


def _ack(status: dict) -> dict:
    ack = status.get("job", {}).get("ack")
    return ack if isinstance(ack, dict) else {}


def _big_source(lab, name: str) -> _lab.Source:
    """A session on A whose repository carries 120 MiB of untracked random data (a transfer of several seconds)."""
    src = lab.make_source("a", name, turns=("a long transfer",))
    for i in range(BIG_FILES):
        src.rb.write(f"data/blob{i}.bin", os.urandom(BIG_FILE_MIB * MiB))
    return src


def _push_bg(lab, ev, sid: str, tag: str) -> tuple[subprocess.Popen, str]:
    """``hermes handover to laptopgw --session <sid> --yes`` from A's CLI in the background (own process group);
    retried when the plugin was skipped in that process (version-probe flake, PATH.md "Stolpersteine")."""
    log = ev.root / f"push-{tag}.log"
    for _attempt in range(3):
        t0 = time.time()
        proc = lab.hermes_bg("a", "handover", "to", _lab.LAPTOP_GW, "--session", sid, "--yes", "--json", log=log)
        try:
            job = lab.wait_new_job("a", t0, timeout=180)
        except _lab.LabError:
            _lab.kill_tree(proc)
            if "is not a `hermes` command" in log.read_text(encoding="utf-8", errors="replace"):
                continue
            raise
        except BaseException:
            _lab.kill_tree(proc)
            raise
        return proc, str(job["job_id"])
    raise _lab.LabError("the background push never started a job (plugin skipped three times)")


def _end(proc: subprocess.Popen | None, wait: float) -> None:
    """Let a background CLI end on its own within ``wait`` seconds, else end it by PID."""
    if proc is None:
        return
    try:
        proc.wait(wait)
    except subprocess.TimeoutExpired:
        _lab.kill_tree(proc)


def _kill_now(proc: subprocess.Popen | None) -> None:
    """SIGKILL to a background CLI's process group: no handler, no ``finally`` of the CLI runs (``kill -9``)."""
    if proc is None or proc.poll() is not None:
        return
    with contextlib.suppress(OSError):
        os.killpg(proc.pid, signal.SIGKILL)
    with contextlib.suppress(subprocess.TimeoutExpired):
        proc.wait(30)


def _state(lab, inst: str, jid: str | None, role: str = "conductor") -> str | None:
    return (lab.job_row(inst, jid, role) or {}).get("state") if jid else None


def _source_state(lab, inst: str, jid: str | None) -> str | None:
    """State of the job's source handover (``source_handovers``), the row that holds the lineage."""
    return (lab.source_row(inst, jid) or {}).get("state") if jid else None


def _owner_age(lab, inst: str, jid: str, role: str) -> float:
    """Seconds since the job row of ``role`` was last claimed or heartbeated by its owner (``jobs.owner_heartbeat_at``,
    the column ``Ledger.stale_jobs`` compares)."""
    row = lab.job_row(inst, jid, role) or {}
    beat = row.get("owner_heartbeat_at") or row.get("updated_at") or 0
    return time.time() - float(beat)


def _wait_owner_stale(lab, ev, jid: str, roles: tuple[str, ...], tag: str, *, timeout: float = 300.0) -> None:
    """Wait until every job row of ``roles`` on A is older than ``STALE_OWNER_S`` + 5 s (what the janitor treats as an
    owner that died, ``engine/ledger.py``); a precondition, so a miss is a ``LabError``."""
    from ctx_handover.engine.ledger import STALE_OWNER_S

    need = float(STALE_OWNER_S) + 5.0
    ok = _lab.wait_for(lambda: all(_owner_age(lab, "a", jid, r) > need for r in roles), timeout, poll=1.0)
    ages = {r: round(_owner_age(lab, "a", jid, r), 1) for r in ("conductor", "source")}
    ev.write(f"{tag}-owner-ages.json", {"need_s": need, "ages": ages, "ok": bool(ok)})
    if not ok:
        raise _lab.LabError(f"scaffold: the {roles} row(s) of {jid} never went stale within {timeout:.0f} s: {ages}")


def _janitor_in_a_new_gateway(lab, ev, jid: str, tag: str) -> str:
    """The janitor of DESIGN §3.6 in a new process of A's profile: ``gw-a`` restarted through ``labctl.py``, then
    ``/handover status <job>`` in a scratch session — the first engine use of that process runs
    ``jobs.ensure_started`` → ``janitor()``. Returns the scratch session (for later slash commands)."""
    lab.restart("gw-a")
    scratch = lab.new_session("a", lab.work("a"))
    out, _ = lab.slash("a", scratch, f"/handover status {jid}", timeout=180)
    ev.write(f"{tag}-janitor-trigger.txt", out)
    return scratch


def _settle(lab, ev, jid: str | None) -> None:
    """Clean-up, not under test: a job this test left unfinished is cancelled and resumed through A's gateway, so it
    holds no participant slot of B for the following tests. Waits ≤ 120 s."""
    if not jid or _state(lab, "a", jid) in {"done", "aborted"}:
        return
    ev.cli("settle-cancel", lab.handover("a", "cancel", jid, timeout=180))
    scratch = lab.new_session("a", lab.work("a"))
    out, _ = lab.slash("a", scratch, f"/handover resume {jid}", timeout=180)
    ev.write("settle-resume.txt", out)
    _lab.wait_for(lambda: _state(lab, "a", jid) in {"done", "aborted"}, 120, poll=1.0)
    ev.write("settle-state.json", {"a": lab.job_row("a", jid), "b": lab.job_row("b", jid, "target")})


def _cleanup(ev, name: str, fn: Callable[[], object]) -> None:
    """Run one clean-up step; its failure is recorded, never raised over the test's own result."""
    try:
        fn()
    except Exception as exc:  # evidence only: clean-up must not mask the assertion that failed
        ev.write(f"cleanup-error-{name}.txt", repr(exc))


def _turn_reaches_model(lab, ev, tag: str, inst: str, sid: str) -> tuple[_lab.Turn, int]:
    probe = f"{tag.upper()}-FREE-{_lab.uid()}"
    mark = lab.llm_mark()
    turn = lab.turn(inst, sid, probe, timeout=180)
    reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
    ev.write(f"{tag}-turn.json", {"turn": turn.brief(), "reached": len(reached)})
    return turn, len(reached)


def _assert_locked(lab, ev, tag: str, inst: str, sid: str) -> None:
    """R1 / DESIGN §7.4 after a ``done`` handover: every segment ended ``handed_over``, archived, hidden."""
    marks = {s: lab.marks(inst, s) for s in lab.lineage(inst, sid)}
    ev.write(f"{tag}-source-marks.json", marks)
    for seg, m in marks.items():
        assert m["ended"] and m["end_reason"] == "handed_over" and m["archived"] and m["hidden"], (seg, m)


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


def _hello(base: str, token: str, *, timeout: float = 30.0) -> dict:
    """Unsigned ``GET <base>/api/plugins/ctx_handover/v1/hello`` with the dashboard token (``engine/wire.py``
    ``hello(signed=False)``); the first ctx_handover request of a ``hermes serve`` process also starts its listener
    and the listener's maintenance sweep (``jobs.listener_server`` → ``_start_maintenance``)."""
    request = urllib.request.Request(  # noqa: S310 - fixed http://127.0.0.1 lab URL
        f"{base}/api/plugins/ctx_handover/v1/hello", headers={"X-Hermes-Session-Token": token}
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


def _cwd_of(lab, inst: str, sid: str) -> Path | None:
    """The stored cwd of a session as an absolute path, or None (never ``Path("")``, which is pytest's own cwd)."""
    cwd = str(lab.row(inst, sid).get("cwd") or "") if sid else ""
    return Path(cwd) if cwd and Path(cwd).is_absolute() else None


def _dest(job: Mapping, hint: str) -> Path | None:
    path, _mode = _lab.dest_of(job, hint)
    return Path(path) if path and Path(path).is_absolute() else None


def _find_dir_with(root: Path, marker: str) -> Path | None:
    """The directory below ``root`` that holds a regular file named ``marker`` (no symlink is followed)."""
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d != ".git"]
        if marker in filenames and os.path.isfile(os.path.join(dirpath, marker)):
            return Path(dirpath)
    return None


def _links_below(root: Path) -> list[dict]:
    """Every symlink below ``root`` with its target text and where it resolves (``os.path.realpath``)."""
    out = []
    base = root.resolve()
    for dirpath, dirnames, filenames in os.walk(root):
        for name in dirnames + filenames:
            path = Path(dirpath) / name
            if path.is_symlink():
                real = Path(os.path.realpath(path))
                out.append(
                    {
                        "link": str(path.relative_to(root)),
                        "target": os.readlink(path),
                        "real": str(real),
                        "inside": real == base or real.is_relative_to(base),
                    }
                )
    return out


class _ByteWatch:
    """Polls ``root`` every 0.2 s and keeps the largest number of bytes held at once in regular files whose name
    contains ``needle`` (the tree's files, in partial directories and at their place). Ended by ``close()``."""

    def __init__(self, root: Path, needle: str) -> None:
        self.root, self.needle = Path(root), needle
        self.peak = 0
        self.samples = 0
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name="e2e-bytewatch", daemon=True)
        self._thread.start()

    def measure(self) -> int:
        total = 0
        for dirpath, dirnames, filenames in os.walk(self.root):
            dirnames[:] = [d for d in dirnames if d != ".git"]
            for name in filenames:
                if self.needle not in name:
                    continue
                with contextlib.suppress(OSError):
                    st = os.lstat(os.path.join(dirpath, name))
                    if stat.S_ISREG(st.st_mode):
                        total += st.st_size
        return total

    def _run(self) -> None:
        while not self._stop.is_set():
            with contextlib.suppress(OSError):
                self.peak = max(self.peak, self.measure())
                self.samples += 1
            self._stop.wait(0.2)

    def close(self) -> None:
        self._stop.set()
        self._thread.join(10)


class _Catcher:
    """A loopback HTTP server on its own port (47200–47299: outside the lab's 47100–47112, below 47300) that records
    every request (request line, headers, body) and answers 404 like a host without ctx_handover. ``close()`` shuts
    it down and joins its thread."""

    def __init__(self) -> None:
        records: list[dict] = []
        lock = threading.Lock()

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def _answer(self) -> None:
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(min(length, MiB)) if length > 0 else b""
                with lock:
                    records.append(
                        {
                            "line": self.requestline,
                            "headers": [[k, v] for k, v in self.headers.items()],
                            "body": body.decode("utf-8", "replace"),
                        }
                    )
                payload = b'{"error": "not found"}'
                self.send_response(404)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            do_GET = do_POST = do_PUT = _answer

            def log_message(self, *args: object) -> None:  # no stderr noise in the test output
                return

        self.records = records
        self.server: http.server.ThreadingHTTPServer | None = None
        for port in range(47200, 47300):
            if not _lab.port_free(port):
                continue
            try:
                self.server = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
            except OSError:
                continue
            break
        if self.server is None:
            raise _lab.LabError("no free catcher port in 47200-47299")
        self.server.daemon_threads = True
        self.port = int(self.server.server_address[1])
        self.url = f"http://127.0.0.1:{self.port}"
        self._thread = threading.Thread(
            target=self.server.serve_forever, kwargs={"poll_interval": 0.2}, name="e2e-catcher", daemon=True
        )
        self._thread.start()

    def close(self) -> None:
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
        self._thread.join(10)


def _pair_string(url: str, fingerprint: str, name: str) -> str:
    """A ``ctxh-pair:`` paste string (``engine/pairing.py`` ``pair_string``: base64url of the canonical JSON ``{v, url,
    code, fp, name, profile}``) with a fresh, never issued code."""
    code = (
        "CTXH-"
        + "".join(secrets.choice(CROCKFORD) for _ in range(6))
        + "-"
        + "".join(secrets.choice(CROCKFORD) for _ in range(16))
    )
    doc = {"v": 1, "url": url, "code": code, "fp": fingerprint, "name": name, "profile": None}
    raw = json.dumps(doc, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return "ctxh-pair:" + base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _env_value(path: Path, key: str) -> str | None:
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.startswith(f"{key}="):
            return line.split("=", 1)[1]
    return None


def _ignore_locally(repo: Path, rel: str, content: str) -> None:
    """``rel`` ignored only in this clone (``.git/info/exclude``; no tracked file changes), holding ``content``."""
    info = repo / ".git" / "info"
    info.mkdir(parents=True, exist_ok=True)
    with open(info / "exclude", "a", encoding="utf-8") as fh:
        fh.write(f"/{rel}\n")
    (repo / rel).write_text(content, encoding="utf-8")


def _lexically_inside(link: str, target: str) -> bool:
    """The lexical rule of DESIGN §10.3 (``engine/policy.py`` ``_link_target_inside``): a relative target that,
    joined to the link's folder and normalised without resolving any link, stays below the tree's root."""
    if not target or target.startswith("/"):
        return False
    joined = posixpath.normpath(posixpath.join(posixpath.dirname(link) or ".", target))
    return joined == "." or not (joined == ".." or joined.startswith("../"))


def _printable(digest: Mapping[bytes, object]) -> dict[str, object]:
    return {k.decode("utf-8", errors="surrogateescape"): v for k, v in sorted(digest.items())}


# ---------------------------------------------------------------------------------------------------------
# B1: abort after resume
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(1800)
def test_rv_b1_abort_after_resume_frees_the_source(lab, ev):
    """Review B1 (blocker, g1 B1): an abort after ``resume`` reports the source "released" from a cached operation
    while the source stays locked.

    Steps: (1) push A→B of a 120-MiB session from A's CLI in the background; at the first observed transfer state the
    CLI's process group gets SIGKILL (the review's entry "Konduktor stirbt … CLI gekillt"); (2) after
    ``STALE_OWNER_S`` + 5 s the janitor of a new ``gw-a`` process suspends the job and releases the source (operation
    ``release``) — precondition, checked; (3) ``serve-b`` stopped, ``/handover resume <job>`` in A's gateway: the
    resumed conductor locks the source again and is suspended again because the target is away — precondition,
    checked; (4) ``serve-b`` started, ``/handover cancel <job>`` in A's gateway (the abort of a suspended job, L3-F07:
    ``target.abort``, then ``source.release``).

    Soll — REVIEW B1 Vorschlag ("Regressionstest: Janitor-Release → Re-Lock → … → Abbruch ⇒ Ledger ``released``,
    Session sichtbar"), DESIGN §3.2 (ABORTED only when the release reports ``released``/``reclaimed``), BRIEF D8
    ("Fehlschlag → Quelle unverändert"): the job ends ``aborted``; A's source handover of the job is ``released`` or
    ``reclaimed``; the session is neither ended nor hidden and a turn there reaches the model without the veto; a new
    push of the same session ends ``done`` (no ``already_handing_over``).

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/local_source.py:720-740`` (``start_release`` always
    under the digest ``"release"``), ``engine/ops.py:182-191`` (a ``done`` operation is returned unchanged, only
    FAILED restarts), ``engine/ledger.py:1068-1088`` (``source_lock`` locks a ``released`` source of the same job
    again), ``engine/conductor.py:1707-1739`` (``_aborting`` accepts ``{"state": "released"}``).

    Expected red on ``2f5fd1d5`` (read): step (4)'s ``source.release`` gets the stored result of step (2)'s
    ``release`` operation, so the job ends ``aborted`` while ``source_handovers`` still says ``locked`` — the
    source-state assertion fails; the new push would be refused ``already_handing_over`` (``Ledger._conflicting``).

    A's ``lease_ttl_s`` 30 and ``stall_timeout_min`` 1 for the test (restored, ``gw-a`` restarted). Clean-up, not under
    test: ``reclaim --force`` when the aborted job's source still holds the session."""
    proc = None
    jid = None
    src = None
    with lab.preserved("a", restart=("gw-a",)):
        lab.setting("a", "lease_ttl_s", "30")
        lab.setting("a", "stall_timeout_min", "1")
        src = _big_source(lab, f"rvb1proj{lab.run_id}")
        try:
            proc, jid = _push_bg(lab, ev, src.sid, "b1")
            seen = lab.wait_job("a", jid, WINDOW | LATER, poll=0.02)
            if seen in WINDOW:
                _kill_now(proc)
            killed_in = _state(lab, "a", jid)
            ev.write("kill.json", {"job": jid, "seen": seen, "state_after_kill": killed_in})
            if seen not in WINDOW or killed_in not in SUSPENDABLE:
                raise _lab.LabError(f"scaffold: kill window missed (seen {seen}, after the kill {killed_in})")
            proc = None
            _wait_owner_stale(lab, ev, jid, ("conductor", "source"), "b1")
            scratch = _janitor_in_a_new_gateway(lab, ev, jid, "b1")

            def janitored() -> bool:
                return _state(lab, "a", jid) == "suspended" and _source_state(lab, "a", jid) in GIVEN_BACK

            ok = _lab.wait_for(janitored, 120, poll=0.5)
            ev.write(
                "after-janitor.json",
                {"ok": bool(ok), "conductor": lab.job_row("a", jid), "source": lab.source_row("a", jid)},
            )
            if not ok:
                raise _lab.LabError("precondition of B1 not reached: the janitor did not suspend and release the job")
            time.sleep(2)  # the janitor's recover thread of this job has ended (JobRunner._spawn reuses a live one)
            lab.stop("serve-b")
            t_resume = time.time()
            out, _ = lab.slash("a", scratch, f"/handover resume {jid}", timeout=180)
            ev.write("resume.txt", out)

            def after_resume() -> list[str]:
                return [
                    str(e.get("state"))
                    for e in lab.events("a", jid)
                    if e.get("kind") == "transition" and float(e.get("ts") or 0) >= t_resume
                ]

            _lab.wait_for(lambda: after_resume()[-1:] == ["suspended"], 420, poll=1.0)
            relock = {"states": after_resume(), "source": _source_state(lab, "a", jid)}
            ev.write("relock.json", relock)
            if "locked" not in relock["states"] or relock["states"][-1:] != ["suspended"]:
                raise _lab.LabError(f"precondition of B1 not reached: no re-lock and second suspension: {relock}")
            if relock["source"] not in ACTIVE_SOURCE_STATES:
                raise _lab.LabError(f"precondition of B1 not reached: the resumed job holds no source: {relock}")
            lab.start("serve-b")
            out, _ = lab.slash("a", scratch, f"/handover cancel {jid}", timeout=180)
            ev.write("cancel.txt", out)
            final = lab.wait_job("a", jid, {"aborted", "done"}, timeout=180, poll=0.5)
            source = lab.source_row("a", jid) or {}
            marks = lab.marks("a", src.sid)
            ev.write("after-cancel.json", {"final": final, "conductor": lab.job_row("a", jid), "source": source})
            turn, reached = _turn_reaches_model(lab, ev, "b1", "a", src.sid)
            following = lab.handover(
                "a", "to", _lab.LAPTOP_GW, "--session", lab.tip("a", src.sid), "--yes", timeout=900
            )
            ev.cli("next-push", following)
            next_state = _state(lab, "a", following.job_id)
        finally:
            _cleanup(ev, "kill", lambda: _kill_now(proc))
            _cleanup(ev, "serve-b", lambda: None if lab.running("serve-b") else lab.start("serve-b"))
            _cleanup(ev, "settle", lambda: _settle(lab, ev, jid))
            if jid and _state(lab, "a", jid) == "aborted" and _source_state(lab, "a", jid) in ACTIVE_SOURCE_STATES:
                _cleanup(
                    ev,
                    "reclaim",
                    lambda: ev.cli("cleanup-reclaim-force", lab.handover("a", "reclaim", src.sid, "--force")),
                )
    assert final == "aborted", (final, out)
    assert source.get("state") in GIVEN_BACK, (
        f"the job ended aborted, but A's source handover is still {source.get('state')!r} (cached release)"
    )
    assert not marks["ended"] and not marks["hidden"], marks
    assert turn.ok and reached and "/handover reclaim" not in turn.text, turn.brief()
    assert following.rc == 0 and following.error_code != "already_handing_over", following.brief()
    assert next_state == "done", following.brief()


# ---------------------------------------------------------------------------------------------------------
# B2: ignored files at the destination
# ---------------------------------------------------------------------------------------------------------

TARGET_DATA = "local data of the destination: ignored here, never handed over\n"
SOURCE_DATA = "from the source: untracked and not ignored there\n"


@pytest.mark.e2e_timeout(1200)
@pytest.mark.parametrize("variant", ["return", "update"])
def test_rv_b2_ignored_file_at_the_destination_is_never_overwritten(lab, ev, variant):
    """Review B2 (blocker, g5 B1): update and return mode overwrite files the destination ignores.

    ``return``: A's repository ignores ``local-<id>.cfg`` through ``.git/info/exclude`` and holds local data there
    (set up before the handover, so the folder is unchanged since then); B pulls the session, writes the same path in
    its copy as an untracked, not ignored file and pushes the session back — A's folder is the recorded origin
    (return mode, R4; ``test_l3_acceptance.py::test_acc2_*`` shows that this way lands in place). ``update``: a first
    push A→B of a clean repository lands at ``D`` (fresh); ``D`` ignores ``local-<id>.cfg`` through its
    ``.git/info/exclude`` and holds local data there (``D`` stays clean for ``git status``); a second session in A's
    same folder, with the path untracked and not ignored there, is pushed — B's ``in`` mapping points it to ``D``
    (update mode, ``engine/materialize.py`` ``assess``).

    Soll — REVIEW B2 Vorschlag ("Existiert dort etwas, das der Index nicht führt ⇒ ``ConflictError(kind=dirty)`` ⇒
    frisches Verzeichnis"), BRIEF D10 ("Am Ziel nie uncommittete lokale Änderungen überschreiben (Konflikt → frisches
    Verzeichnis)"), DESIGN §9.5 / I5: the handover ends ``done``; the ignored file at the destination keeps its bytes
    and the destination's work tree is unchanged; the repository lands in another folder, the job says why (a
    recorded conflict, or a warning naming the destination), and that folder holds the source's version of the file.

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/gitapply.py:1155-1160`` (update c1 ``read-tree -u -m
    H0 W``), ``:1167-1172`` (return c1), precondition ``engine/gitops.py:1104-1121`` (``status --untracked-files=all``
    without ignored files), inverse ``engine/gitapply.py:1228-1234``, ``:1242-1247``.

    Expected red on ``2f5fd1d5`` (read): ``assess`` sees a clean / unchanged destination and chooses update or return
    mode; c1's ``read-tree -u -m`` replaces the ignored file with the source's content (git ≥ 2.35 overwrites ignored
    files; the review's probe on git 2.56: rc 0, content replaced) — the byte check of the ignored file fails."""
    rel = f"local-{lab.run_id}.cfg"
    if variant == "return":
        src = lab.make_source("a", f"rvb2ret{lab.run_id}", turns=("ignored local data at home",))
        _ignore_locally(src.repo, rel, TARGET_DATA)
        took = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
        ev.cli("take", took)
        assert took.rc == 0 and took.job_id, took.brief()
        status = lab.job_status("b", took.job_id)
        ev.write("take-status.json", status)
        sid_b = str(_ack(status).get("session_id") or "")
        copy = _cwd_of(lab, "b", sid_b)
        if copy is None or not copy.is_dir() or (copy / rel).exists():
            raise _lab.LabError(f"scaffold: B's copy missing, or the ignored file travelled: {copy}")
        (copy / rel).write_text(SOURCE_DATA, encoding="utf-8")
        target_dir = src.repo
        before = lab.repo(target_dir).worktree_digest()
        res = lab.handover("b", "to", _lab.SERVER, "--session", lab.tip("b", sid_b), "--yes", timeout=600)
        ev.cli("push-back", res)
        status = lab.job_status("b", res.job_id) if res.job_id else {"job": {}}
    else:
        src = lab.make_source("a", f"rvb2upd{lab.run_id}", dirty=False, turns=("first push",))
        first = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--yes", timeout=600)
        ev.cli("first-push", first)
        assert first.rc == 0 and first.job_id, first.brief()
        first_status = lab.job_status("a", first.job_id)
        ev.write("first-push-status.json", first_status)
        target_dir = _dest(first_status["job"], src.repo.name)
        if target_dir is None or not (target_dir / ".git").is_dir() or lab.repo(target_dir).status().strip(b"\0"):
            raise _lab.LabError(f"scaffold: the first push left no clean repository on B: {target_dir}")
        _ignore_locally(target_dir, rel, TARGET_DATA)
        before = lab.repo(target_dir).worktree_digest()
        (src.repo / rel).write_text(SOURCE_DATA, encoding="utf-8")
        sid2 = lab.new_session("a", src.repo)
        assert lab.turn("a", sid2, f"second session in the same folder [{lab.run_id}]").ok
        with lab.db("a", write=True) as db:
            db.set_session_title(sid2, f"rvb2upd second {lab.run_id}")
        res = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", sid2, "--yes", timeout=600)
        ev.cli("second-push", res)
        status = lab.job_status("a", res.job_id) if res.job_id else {"job": {}}
    ev.write("status.json", status)
    job = status["job"]
    landed = _dest(job, target_dir.name)
    after = lab.repo(target_dir).worktree_digest()
    report = json.dumps(
        {"conflicts": job.get("conflicts"), "warnings": job.get("warnings"), "ack": _ack(status).get("warnings")},
        ensure_ascii=False,
    )
    ev.write(
        "destination.json",
        {"destination": str(target_dir), "landed": str(landed), "report": report, "before": _printable(before)},
    )
    ev.write("destination-after.json", _printable(after))
    assert res.rc == 0 and job.get("state") == "done", res.brief()
    assert (target_dir / rel).read_text(encoding="utf-8") == TARGET_DATA, (
        f"{variant}: the ignored {rel} at {target_dir} was overwritten by the source's untracked file"
    )
    assert after == before, f"{variant}: the destination's work tree changed"
    assert landed is not None and landed.resolve() != target_dir.resolve(), (str(landed), str(target_dir))
    assert job.get("conflicts") or str(target_dir) in report, report
    assert (landed / rel).read_text(encoding="utf-8") == SOURCE_DATA, str(landed)


# ---------------------------------------------------------------------------------------------------------
# H1: the gate credential and a foreign paste string
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(300)
def test_rv_h1_pairing_sends_no_gate_credential_to_a_foreign_url(lab, ev):
    """Review H1 (g3 H1): pairing sends the dashboard credential to the URL of a paste string before its key is
    checked.

    B holds A's dashboard token (``CTX_HANDOVER_PEER_TOKEN`` in B's ``.env``, the paired stand). A paste string whose
    URL is a loopback HTTP catcher started by this test (own port in 47200–47299) and whose fingerprint belongs to no
    key there is pasted on B: ``hermes handover pair <paste> --name <new>``.

    Soll — REVIEW H1 Vorschlag ("Ein Credential nie an eine andere Origin senden als an die, für die es gespeichert
    wurde … Test: Paste-String mit fremder URL ⇒ kein Gate-Header im ersten Request"), R17 (the gate credential opens
    the whole dashboard over there): no request the catcher receives carries B's stored secret (headers or body) or a
    gate header (``X-Hermes-Session-Token``, ``Authorization``); the pairing is refused and B stores no peer under the
    new name. Not asserted: that the catcher is contacted at all (a fix may refuse earlier); the count is evidence.

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/pairing.py:360-372`` (provisional peer with ``gate``,
    ``client.hello(signed=False)`` to ``pair["url"]``, fingerprint compared only at ``:373-381``),
    ``engine/wire.py:366`` (``headers.update(self._gate())`` on every request), ``engine/surfaces.py:487-500`` (one
    ``CTX_HANDOVER_PEER_TOKEN`` for every peer).

    Expected red on ``2f5fd1d5`` (read): the unsigned ``GET …/v1/hello`` to the catcher carries
    ``X-Hermes-Session-Token: <B's stored token>`` — the leak assertion fails."""
    from ctx_handover.engine.pairing import parse_pair_string

    secret = _env_value(lab.env_path("b"), "CTX_HANDOVER_PEER_TOKEN")
    if not secret:
        raise _lab.LabError("scaffold: B holds no CTX_HANDOVER_PEER_TOKEN (conftest pairing stand)")
    name = f"h1x{_lab.uid(4)}"
    catcher = _Catcher()
    try:
        paste = _pair_string(catcher.url, secrets.token_hex(8), "elsewhere")
        parse_pair_string(paste)  # scaffold: the string is well-formed for the plugin
        res = lab.handover("b", "pair", paste, "--name", name, timeout=180)
    finally:
        catcher.close()
    records = list(catcher.records)
    peers = lab.ledger_rows("b", "SELECT name, direction, url, revoked_at FROM peers WHERE name = ?", (name,))
    redacted = json.loads(json.dumps(records).replace(secret, "<B-stored-gate-secret>"))
    leaked = [i for i, r in enumerate(records) if secret in json.dumps(r)]
    gated = [
        i
        for i, r in enumerate(records)
        if any(k.lower() in ("x-hermes-session-token", "authorization") for k, _v in r["headers"])
    ]
    ev.write(
        "catcher.json",
        {"url": catcher.url, "requests": redacted, "leaked": leaked, "gate_headers": gated, "peers": peers},
    )
    ev.write("pair.json", {"rc": res.rc, "data": res.data, "stderr": res.stderr[-3000:].replace(secret, "<secret>")})
    try:
        assert not leaked, f"B's stored dashboard secret reached the paste string's URL (requests {leaked})"
        assert not gated, f"a gate header reached the paste string's URL (requests {gated})"
        assert res.rc != 0, res.brief().replace(secret, "<secret>")
        assert not peers, peers
    finally:
        if peers:
            ev.cli("cleanup-revoke", lab.handover("b", "peers", "revoke", name, timeout=120))


# ---------------------------------------------------------------------------------------------------------
# H2: continuation on a YOLO gateway
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(900)
def test_rv_h2_yolo_target_refuses_the_continuation(lab, ev):
    """Review H2 (g6 H1): the continuation starts although the target gateway runs without approvals
    (``HERMES_YOLO_MODE=1`` / ``--yolo``) while its ``config.yaml`` keeps the defaults.

    A's ``gw-a`` (where the continued turn runs, upstream ``hermes_cli/gateway_agent.py:1-6``) and ``serve-a``
    (where the ``…/continue`` route and the spawner run) are started with ``HERMES_YOLO_MODE=1`` — the whole stack of
    that machine, as a container environment would set it — through the new ``labctl.py`` knobs
    ``GW_ENV``/``SERVE_ENV``; A allows ``allow_remote_continue``; B pushes a session with ``--continue "<prompt>"
    --yes``.

    Soll — R22 ("startet nicht, wenn am Ziel Approvals abgeschaltet sind"), catalogue rule 12, README "never when
    approvals are off there", REVIEW H2 Vorschlag ("``approvals_off`` um den Prozesszustand erweitern
    (``HERMES_YOLO_MODE``) … Test: Spawner mit gesetzter Variable ⇒ ``continue_refused``"), as in
    ``test_l3_acceptance.py::test_acc7_continue_refused_on_the_target``: the handover ends ``done`` and the session
    is active on A; no continuation starts, no turn with the prompt reaches the model within 15 s; the job records
    ``continue_refused`` with reason ``approvals_off`` and the message says "approvals are switched off".

    Not tested here: YOLO only on the gateway behind a separate ``serve`` — R33a names that a limit of v0.1 (the plugin
    cannot see it; README says the refusal does not hold then), so there is no target behaviour to assert.

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/hostcompat.py:458-469`` (``approvals_off`` reads only
    ``approvals.mode`` and ``single_query_mode`` from ``config.yaml``), wired in ``engine/jobs.py:423``, evaluated in
    ``engine/continuation.py:210``; upstream ``tools/approval.py:48`` (``_YOLO_MODE_FROZEN`` from the process).

    Expected red on ``2f5fd1d5`` (read): ``approvals_off`` is False with the default config, so the spawner starts
    ``hermes chat --resume <id> -q <prompt>`` and the turn reaches the model through ``gw-a`` — the ``started`` and
    ``continue_refused`` assertions fail. ``feat/ctx-handover`` ``32eaf2a2`` reads the variable (fix of the review).

    A's settings restored; ``gw-a`` and ``serve-a`` restarted without the variable through ``labctl.py``; a
    continuation process the job names is ended by PID."""
    pids: list[int] = []
    with lab.preserved("a", restart=("gw-a", "serve-a")):
        lab.setting("a", "allow_remote_continue", "true")
        ev.write("config-a.yaml", lab.config_path("a").read_text(encoding="utf-8"))
        lab.stop("gw-a", "serve-a")
        for comp in ("gw-a", "serve-a"):
            _start_with_env(lab, comp, {YOLO: "1"})
        seen = {comp: _proc_env(lab.pid(comp)).get(YOLO) for comp in ("gw-a", "serve-a")}
        ev.write("yolo-env.json", seen)
        if any(v != "1" for v in seen.values()):
            raise _lab.LabError(f"scaffold: {YOLO} is not set in the started processes: {seen}")
        src = lab.make_source("b", f"rvh2proj{lab.run_id}", turns=("closing on the laptop",))
        prompt = f"RVH2-CONTINUE-{_lab.uid()}"
        mark = lab.llm_mark()
        try:
            res = lab.handover("b", "to", _lab.SERVER, "--session", src.sid, "--continue", prompt, "--yes", timeout=600)
            ev.cli("push-continue", res)
            status = lab.job_status("b", res.job_id) if res.job_id else {"job": {}}
            ev.write("job-b.json", status)
            cont = status["job"].get("continuation") or {}
            if isinstance(cont, dict) and cont.get("pid"):
                pids.append(int(cont["pid"]))
            time.sleep(15)
            late = [r for r in lab.llm_since(mark) if prompt in _lab.last_user_message(r)]
            sid_a = str(_ack(status).get("session_id") or "")
            row_a = lab.row("a", sid_a) if sid_a else {}
            ev.write("after.json", {"cont": cont, "late": _lab.compact_records(late), "row_a": row_a})
        finally:
            for pid in pids:
                _lab.kill_pid(pid)
    assert res.rc == 0 and status["job"].get("state") == "done", res.brief()
    assert row_a and row_a.get("ended_at") is None and not row_a.get("hidden"), row_a
    assert not (isinstance(cont, dict) and cont.get("started")), cont
    assert "continue_refused" in json.dumps(cont) and "approvals_off" in json.dumps(cont), cont
    assert "approvals are switched off" in res.text.lower(), res.text[-2000:]
    assert not late, "a continuation turn ran on a gateway that runs without approvals"


# ---------------------------------------------------------------------------------------------------------
# H6, H7: housekeeping of another process during a running push
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(1200)
def test_rv_h6_janitor_keeps_the_source_of_a_running_push_locked(lab, ev):
    """Review H6 (g1 H1): the janitor of any process releases the source of a running push once its participant row
    saw no ``_epoch`` call for 60 s.

    A push A→B of a 120-MiB session from A's CLI in the background; at the first observed transfer state or
    TARGET_PREPARING ``serve-b`` is frozen (SIGSTOP): the conductor waits for the target, keeps beating its own row
    and refreshing the leases (keepalive), but on ``2f5fd1d5`` nothing touches the source's row. Once that row is
    older than ``STALE_OWNER_S`` + 5 s — or ``STALE_OWNER_S`` + 60 s after the freeze, when a fix keeps it fresh —
    a new ``gw-a`` process runs the janitor (``/handover status <job>`` after a restart through ``labctl.py``; the
    conductor's own row must still be fresh then — checked); the source's state is sampled for 20 s, then ``serve-b``
    gets SIGCONT. The conductor's retries against the frozen target (5 attempts with a 60-s read timeout each) outlast
    this window.

    Soll — REVIEW H6 Vorschlag ("Konduktor-Keepalive die lokale ``source``-Zeile des eigenen Jobs mit-heartbeaten …
    oder im Janitor ``params.local``-Zeilen überspringen, solange die Konduktor-Zeile desselben Jobs nicht stale
    ist"), DESIGN §3.6 (the janitor acts on jobs whose owner died; this one's conductor lives): the source stays
    locked while the job runs — no sample is ``released``/``reclaimed``/``suspended`` —, the push ends ``done`` and A's
    session is locked afterwards (R1).

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/local_source.py:459-472`` (``heartbeat`` refreshes
    only the turn leases), ``:197-202`` (owner heartbeat only through ``_epoch``), ``engine/ledger.py:850-862``
    (``stale_jobs`` looks at age only; ``STALE_OWNER_S = 60``), ``engine/jobs.py:1432-1447`` →
    ``engine/participant.py:420-428`` (release without looking at the live conductor).

    Expected red on ``2f5fd1d5`` (read): the janitor finds the source row stale and starts ``release`` (``owner_died``)
    — the samples show ``released``; after SIGCONT the conductor's next lease refresh raises ``LeaseLost`` and the job
    does not end ``done``."""
    proc = None
    jid = None
    samples: list[dict] = []
    src = _big_source(lab, f"rvh6proj{lab.run_id}")
    pid_b = lab.pid("serve-b")
    try:
        proc, jid = _push_bg(lab, ev, src.sid, "h6")
        seen = lab.wait_job("a", jid, WINDOW | LATER, poll=0.02)
        if seen not in WINDOW:
            raise _lab.LabError(f"scaffold: injection window missed (first observed state {seen})")
        with _lab.stopped(pid_b):
            from ctx_handover.engine.ledger import STALE_OWNER_S

            ev.write("freeze.json", {"job": jid, "seen": seen, "state": _state(lab, "a", jid)})
            # the janitor's window on 2f5fd1d5: the source row older than STALE_OWNER_S; a fix may keep it fresh
            # (REVIEW H6, first proposal), so the wait also ends STALE_OWNER_S + 60 s after the freeze
            need = float(STALE_OWNER_S) + 5.0
            _lab.wait_for(lambda: _owner_age(lab, "a", jid, "source") > need, float(STALE_OWNER_S) + 60.0, poll=1.0)
            ev.write("h6-owner-ages.json", {r: round(_owner_age(lab, "a", jid, r), 1) for r in ("conductor", "source")})
            _janitor_in_a_new_gateway(lab, ev, jid, "h6")
            fresh = _owner_age(lab, "a", jid, "conductor")
            ev.write("conductor-owner-age.json", {"age_s": round(fresh, 1)})
            if fresh >= STALE_OWNER_S:
                raise _lab.LabError(f"scaffold: the running conductor's row went stale ({fresh:.0f} s)")
            t0 = time.monotonic()
            while time.monotonic() - t0 < 20:
                samples.append(
                    {
                        "t": round(time.monotonic() - t0, 1),
                        "conductor": _state(lab, "a", jid),
                        "source": _source_state(lab, "a", jid),
                    }
                )
                time.sleep(0.5)
        final = lab.wait_job("a", jid, {"done", "aborted", "suspended"}, timeout=600, poll=0.5)
        _end(proc, 120)
        proc = None
    finally:
        _cleanup(ev, "kill", lambda: _lab.kill_tree(proc))
        _cleanup(ev, "settle", lambda: _settle(lab, ev, jid))
    ev.write("samples.json", samples)
    status = lab.job_status("a", jid)
    ev.write("job-a.json", status)
    given = [s for s in samples if s["source"] in GIVEN_BACK | {"suspended"}]
    assert not given, f"the janitor of another process gave the source of the running push away: {given[:3]}"
    assert final == "done", status["job"]
    _assert_locked(lab, ev, "h6", "a", src.sid)


@pytest.mark.e2e_timeout(1200)
def test_rv_h7_listener_sweep_keeps_the_source_of_a_running_push_locked(lab, ev):
    """Review H7 (g1 H2): the listener's sweep suspends the local source of a running push after
    ``participant_idle_timeout``.

    A's ``participant_idle_timeout_min`` is 1 (the smallest value; default 15) and ``serve-a`` restarted, then one
    unsigned ``hello`` starts its listener and with it the maintenance sweep (every ``MAINTENANCE_S`` = 10 s,
    ``engine/jobs.py``). A push A→B of a 120-MiB session runs from A's CLI; at the first observed transfer state or
    TARGET_PREPARING ``serve-b`` is frozen (SIGSTOP) until the source's job row is older than the idle timeout plus two
    sweep intervals plus 5 s (``jobs.updated_at`` moves only with transitions) or its source state changes, sampling
    every second; then SIGCONT.

    Soll — REVIEW H7 Vorschlag ("``sweep`` auf ferngesteuerte Teilnehmerzeilen (``params.local is not True``)
    beschränken bzw. Leerlauf an der Lease-Aktivität messen"), DESIGN §7.3 (the idle rule is for remote conductors):
    the source stays locked while the push runs — no sample is ``suspended``/``released``/``reclaimed`` —, the push
    ends ``done`` and A's session is locked afterwards (R1).

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/participant.py:373-392`` (``sweep`` suspends every
    source row in ``locking``/``locked`` whose ``updated_at`` is older than ``participant_idle_timeout``, the own
    ``params.local`` rows included).

    Expected red on ``2f5fd1d5`` (read): ``serve-a``'s sweep suspends the CLI's local source (``lock.suspend``) — the
    samples show ``suspended``; the conductor's next lease refresh raises ``LeaseLost: the source is suspended`` and
    the job does not end ``done``.

    A's setting restored and ``serve-a`` restarted through ``labctl.py``."""
    from ctx_handover.engine.jobs import MAINTENANCE_S

    need = 60.0 + 2 * float(MAINTENANCE_S) + 5.0
    proc = None
    jid = None
    samples: list[dict] = []
    with lab.preserved("a", restart=("serve-a",)):
        lab.setting("a", "participant_idle_timeout_min", "1")
        lab.restart("serve-a")
        hello = _hello(_lab.SERVE_URL["a"], lab.tokens["a"])
        ev.write("hello-serve-a.json", hello)
        if hello["status"] != 200:
            raise _lab.LabError(f"scaffold: serve-a's ctx_handover listener did not start: {hello}")
        src = _big_source(lab, f"rvh7proj{lab.run_id}")
        pid_b = lab.pid("serve-b")
        try:
            proc, jid = _push_bg(lab, ev, src.sid, "h7")
            seen = lab.wait_job("a", jid, WINDOW | LATER, poll=0.02)
            if seen not in WINDOW:
                raise _lab.LabError(f"scaffold: injection window missed (first observed state {seen})")
            idle = 0.0
            with _lab.stopped(pid_b):
                deadline = time.monotonic() + 300
                while time.monotonic() < deadline:
                    row = lab.job_row("a", jid, "source") or {}
                    idle = time.time() - float(row.get("updated_at") or time.time())
                    sample = {
                        "idle_s": round(idle, 1),
                        "conductor": _state(lab, "a", jid),
                        "source": _source_state(lab, "a", jid),
                    }
                    samples.append(sample)
                    if sample["source"] not in ("locking", "locked") or idle > need:
                        break
                    time.sleep(1.0)
            ev.write("samples.json", samples)
            changed = bool(samples) and samples[-1]["source"] not in ("locking", "locked")
            if not changed and idle <= need:
                raise _lab.LabError(f"scaffold: the source row never reached the idle window ({idle:.0f}/{need:.0f} s)")
            final = lab.wait_job("a", jid, {"done", "aborted", "suspended"}, timeout=600, poll=0.5)
            _end(proc, 120)
            proc = None
        finally:
            _cleanup(ev, "kill", lambda: _lab.kill_tree(proc))
            _cleanup(ev, "settle", lambda: _settle(lab, ev, jid))
    status = lab.job_status("a", jid)
    ev.write("job-a.json", status)
    given = [s for s in samples if s["source"] in GIVEN_BACK | {"suspended"}]
    assert not given, f"the listener's sweep took the source of the running push: {given[:3]}"
    assert final == "done", status["job"]
    _assert_locked(lab, ev, "h7", "a", src.sid)


# ---------------------------------------------------------------------------------------------------------
# H9, H11: file trees received from a peer
# ---------------------------------------------------------------------------------------------------------

ESCAPE_LINK = "escape-link"
ESCAPE_CHAIN = "escape-chain"


@pytest.mark.e2e_timeout(600)
def test_rv_h9_symlink_chain_does_not_lead_out_of_the_received_tree(lab, ev):
    """Review H9 (g5 H2): symlink chains in a file tree point out of its root.

    A's session (own repository) is pushed to B with ``--include <dir>``: a plain directory (an ``extra_dir`` item,
    sent as a file tree) holding ``data.txt``, ``a/b/c/u -> ../../..`` (the tree's root), ``escape-link ->
    a/b/c/u/../../../..`` (lexically ``.``, really four levels above the root) and ``escape-chain ->
    escape-link/escape-probe.txt``. All three pass the lexical check (scaffold, checked before the push).

    Soll — REVIEW H9 Vorschlag ("Ein Link-Ziel darf keine Komponente durchlaufen, die im Manifest selbst ein Symlink
    ist. Nach dem Anlegen ``realpath`` gegen die Partial-Wurzel prüfen, sonst entfernen und als ``symlink``
    melden"), DESIGN §10.3 ("only relative and inside the root"), T3: the push ends ``done`` and ``data.txt`` arrives
    byte-equal; no symlink in the received tree resolves outside it; the two escaping links are reported (their names
    appear in the job's status on A or B or in the command's output).

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/policy.py:1005-1023`` (``_link_target_inside`` is
    lexical only), used through ``engine/filetree.py:395``; links created at ``:486-491`` and ``:649``; the source's
    scan takes the same lexical check (``filetree.py:229-233``).

    Expected red on ``2f5fd1d5`` (read): both escaping links pass the lexical check on A and B and are created as
    given; ``realpath`` of ``escape-link`` in B's tree lies four levels above it — the "inside" assertion fails."""
    src = lab.make_source("a", f"rvh9proj{lab.run_id}", turns=("symlink chain in an extra folder",))
    tree = lab.ws("a") / f"rvh9tree{lab.run_id}"
    marker = f"rvh9-marker-{lab.run_id}.txt"
    payload = f"data next to a symlink chain {lab.run_id}\n"
    (tree / "a" / "b" / "c").mkdir(parents=True, exist_ok=True)
    (tree / "data.txt").write_text(payload, encoding="utf-8")
    (tree / marker).write_text("marker\n", encoding="utf-8")
    os.symlink("../../..", tree / "a" / "b" / "c" / "u")
    os.symlink("a/b/c/u/../../../..", tree / ESCAPE_LINK)
    os.symlink(f"{ESCAPE_LINK}/escape-probe.txt", tree / ESCAPE_CHAIN)
    lexical = {
        "a/b/c/u": _lexically_inside("a/b/c/u", "../../.."),
        ESCAPE_LINK: _lexically_inside(ESCAPE_LINK, "a/b/c/u/../../../.."),
        ESCAPE_CHAIN: _lexically_inside(ESCAPE_CHAIN, f"{ESCAPE_LINK}/escape-probe.txt"),
    }
    source_links = _links_below(tree)
    ev.write("source-tree.json", {"tree": str(tree), "lexical": lexical, "links": source_links})
    if not all(lexical.values()) or all(x["inside"] for x in source_links):
        raise _lab.LabError(f"scaffold: the chain is not lexically inside and really outside: {lexical}")
    res = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--include", str(tree), "--yes", timeout=600)
    ev.cli("push", res)
    status = lab.job_status("a", res.job_id) if res.job_id else {"job": {}}
    ev.write("status-a.json", status)
    on_b = lab.handover("b", "status", res.job_id, timeout=180) if res.job_id else None
    if on_b is not None:
        ev.cli("status-b", on_b)
    received = _dest(status["job"], tree.name)
    if received is None or not (received / marker).is_file():
        received = _find_dir_with(lab.handover_root("b"), marker)
    links = _links_below(received) if received is not None else []
    ev.write("received-tree.json", {"received": str(received), "links": links})
    report = json.dumps(status, ensure_ascii=False) + res.text + (on_b.text + json.dumps(on_b.data) if on_b else "")
    assert res.rc == 0 and status["job"].get("state") == "done", res.brief()
    assert received is not None and (received / "data.txt").read_text(encoding="utf-8") == payload, str(received)
    outside = [x for x in links if not x["inside"]]
    assert not outside, f"symlinks in the received tree resolve outside it: {outside}"
    assert ESCAPE_LINK in report and ESCAPE_CHAIN in report, "the links left out are not reported"


@pytest.mark.e2e_timeout(900)
def test_rv_h11_target_refuses_a_tree_over_its_byte_limit_before_writing(lab, ev):
    """Review H11 (g5 H4): a file tree on the target has no byte cap; block de-duplication multiplies what is
    written.

    B's ``max_total_gb`` is 0.02 (≈ 21.5 MB; ``serve-b`` restarted). A's session (own small repository) is pushed to B
    with ``--include <dir>``: 48 files of the same 1 MiB (48 MiB on disk, one block on the wire). A watcher sums, every
    0.2 s, the bytes held under B's handover root in files of that tree (partial directories included).

    Soll — REVIEW H11 Vorschlag ("Vor dem ersten Schreiben die Summe der Datei-``size`` gegen ``max_total_bytes`` und
    den freien Platz (×1,1) prüfen, wie ``_precheck`` es für git tut"), R28 ("ein Peer kann nie mehr ablegen, als der
    bestätigte Plan vorsieht"), BRIEF D8 ("Fehlschlag → Quelle unverändert"): the push does not end ``done`` and the
    CLI reports a failure; the refusal is a size refusal (``payload_too_large``/``insufficient_storage``/
    ``policy_denied``, the codes ``gitapply._precheck`` and the tree's own caps raise); at no sample did B hold as
    much as its limit in files of the tree, and none is left; A's session is not ended.

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/filetree.py:441-517`` (``prepare_fresh`` checks only
    ``max_files``; ``load_manifest`` ``:358-373``), ``_write_file`` ``:417-434``, ``QuotaSink``
    (``engine/local_target.py:138-160``) charges only newly stored blocks; git has the cap
    (``engine/gitapply.py:595-613``).

    Expected red on ``2f5fd1d5`` (read): B stores one block, passes the quota and writes all 48 MiB into the tree;
    the push ends ``done`` — the "not done" and peak-bytes assertions fail.

    B's setting restored and ``serve-b`` restarted through ``labctl.py``."""
    limit_gb = 0.02
    limit = int(limit_gb * GiB)
    files = 48
    needle = f"rvh11-{lab.run_id}-"
    src = lab.make_source("a", f"rvh11proj{lab.run_id}", turns=("a tree larger than the target allows",))
    tree = lab.ws("a") / f"rvh11tree{lab.run_id}"
    tree.mkdir(parents=True, exist_ok=True)
    block = os.urandom(MiB)
    for i in range(files):
        (tree / f"{needle}{i:03d}.bin").write_bytes(block)
    if files * MiB <= 2 * limit:
        raise _lab.LabError("scaffold: the tree is not clearly over the limit")
    watch = None
    with lab.preserved("b", restart=("serve-b",)):
        lab.setting("b", "max_total_gb", str(limit_gb))
        lab.restart("serve-b")
        watch = _ByteWatch(lab.handover_root("b"), needle)
        try:
            res = lab.handover(
                "a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--include", str(tree), "--yes", timeout=600
            )
        finally:
            watch.close()
        ev.cli("push", res)
        status = lab.job_status("a", res.job_id) if res.job_id else {"job": {}}
        ev.write("status-a.json", status)
    left = watch.measure()
    marks = lab.marks("a", src.sid)
    job = status["job"]
    last_error = job.get("last_error") if isinstance(job.get("last_error"), dict) else {}
    codes = {str(c) for c in (res.error_code, last_error.get("code")) if c}
    ev.write(
        "bytes.json",
        {
            "limit": limit,
            "tree": files * MiB,
            "peak": watch.peak,
            "left": left,
            "samples": watch.samples,
            "codes": sorted(codes),
        },
    )
    assert job.get("state") != "done" and res.rc != 0, res.brief()
    assert codes & SIZE_REFUSALS, (sorted(codes), res.text[-1500:])
    assert watch.peak < limit, f"B held {watch.peak} bytes of the tree at once (limit {limit})"
    assert left == 0, f"{left} bytes of the tree were left on B"
    assert marks["ended"] is False, marks


# ---------------------------------------------------------------------------------------------------------
# H14: a symlinked working folder
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(600)
def test_rv_h14_symlinked_working_folder_travels(lab, ev):
    """Review H14 (g8 H1; second instance of L3-F02): a cwd that is itself a symlink counts as "no cwd", the workspace
    is dropped without a word.

    A's session is created through A's gateway with its cwd = ``lab/ws-e2e/a/rvh14link<id>``, a relative symlink to the
    own repository ``rvh14real<id>`` next to it (Hermes stores a picked cwd lexically, upstream
    ``tui_gateway/session_workdir.py`` ``_workspace_cwd``: ``abspath``, no ``realpath`` — checked on the stored row;
    should the gateway store a resolved path, the session is built through ``SessionDB`` with the lexical cwd, as
    ``test_l3_findings.py::test_l3_f17_*`` builds one); B pulls it with ``--yes``.

    Soll — REVIEW H14 Vorschlag ("cwd über den Realpath prüfen (``is_dir(real_cwd)``)"), BRIEF D10 / promise (a)
    (the working material travels), R30 (never silently left out): the pull ends ``done``; B's session has a cwd that
    holds the repository with A's state (HEAD, branch, status, index, refs, stash, work tree without the ignored
    ``build/``).

    Production place (REVIEW, re-read on ``2f5fd1d5``): ``engine/collector.py:501`` (``cls.is_dir(cwd)``) with
    ``:411-413`` (``is_dir`` = ``lstat``, does not follow the link); R30 ``engine/plan.py:1120-1132`` needs a primary
    item; target ``engine/local_target.py:517-528`` puts the session into the empty ``workspace_root``.

    Expected red on ``2f5fd1d5`` (read): ``lstat`` of the link is no directory, the plan has 0 items ("0 items, 0 B"),
    the pull ends ``done`` and B's cwd holds no repository — the ``.git`` assertion fails."""
    real = lab.ws("a") / f"rvh14real{lab.run_id}"
    lab.make_repo(real)
    link = lab.ws("a") / f"rvh14link{lab.run_id}"
    os.symlink(real.name, link)
    repo_a = _lab.repo_state(real)
    sid = lab.new_session("a", link)
    stored, how = str(lab.row("a", sid).get("cwd") or ""), "gateway"
    if stored == str(link):
        assert lab.turn("a", sid, f"work in a symlinked folder [{lab.run_id}]").ok
        with lab.db("a", write=True) as db:
            db.set_session_title(sid, f"rvh14 {lab.run_id}")
    else:  # the gateway resolved the path: the lexical cwd of the Desktop/TUI pick goes in through SessionDB
        from ctx_handover.tests.fakes.sessiondb import SessionBuilder

        with lab.db("a", write=True) as db:
            builder = SessionBuilder(db)
            sid = builder.create(title=f"rvh14 {lab.run_id}", model="fake-model", cwd=str(link))
            builder.turns(sid, 2, size=200)
        stored, how = str(lab.row("a", sid).get("cwd") or ""), "sessiondb"
    ev.write("source.json", {"link": str(link), "real": str(real), "stored_cwd": stored, "how": how, "repo": repo_a})
    if stored != str(link):
        raise _lab.LabError(f"scaffold: the session's stored cwd is {stored!r}, not the symlink {link}")
    res = lab.handover("b", "take", _lab.SERVER, lab.tip("a", sid), "--yes", timeout=600)
    ev.cli("take", res)
    status = lab.job_status("b", res.job_id) if res.job_id else {"job": {}}
    ev.write("status-b.json", status)
    sid_b = str(_ack(status).get("session_id") or "")
    dest = _cwd_of(lab, "b", sid_b)
    listing = sorted(p.name for p in dest.iterdir()) if dest is not None and dest.is_dir() else None
    ev.write("dest.json", {"session": sid_b, "cwd": str(dest), "listing": listing})
    assert res.rc == 0 and status["job"].get("state") == "done", res.brief()
    assert dest is not None and (dest / ".git").is_dir(), (
        f"the repository behind the symlinked cwd did not arrive: {res.text[-1500:]}"
    )
    repo_b = _lab.repo_state(dest)
    ev.write("repo-b.json", repo_b)
    for key in REPO_KEYS:
        assert repo_b[key] == repo_a[key], key
