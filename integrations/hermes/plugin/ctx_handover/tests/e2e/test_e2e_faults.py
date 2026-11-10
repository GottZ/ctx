"""Layer 3 — faults during a handover (TESTPLAN §4 E5, E5b, E10, E12).

The plugin has no fault hook, so faults are made at lab level: a component stopped through ``labctl.py stop``,
SIGSTOP/SIGCONT of a component's PID, a delayed fake-model reply. The moment of injection is found by polling
the conductor's ledger row (read-only SQLite, every 5–20 ms). Pushes run from A's CLI (``hermes handover to
laptopgw --session …``) in the background, so the test can act while the job runs; every such process is ended
by PID in ``finally``. A's settings are restored byte-exact afterwards (``Lab.preserved``).

Runtime estimate: 12–18 min (E5 ≈ 2 min, E5b ≈ 3–4 min with 120 MiB of workspace data, E10 two tests ≈ 3 min
each with a 75-s target stall, E12 ≈ 1 min with a 25-s model delay).
"""

from __future__ import annotations

import os
import sqlite3
import time

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)

MiB = 1024 * 1024
BIG_FILES = 3
BIG_FILE_MIB = 40
TRANSFER_STATES = {"exporting", "context_staged", "packing", "data_staged"}


def _big_source(lab, name: str) -> _lab.Source:
    """A session on A whose repository carries 120 MiB of untracked random data (a transfer of several seconds)."""
    src = lab.make_source("a", name, turns=("a long transfer",))
    for i in range(BIG_FILES):
        src.rb.write(f"data/blob{i}.bin", os.urandom(BIG_FILE_MIB * MiB))
    return src


def _push_bg(lab, ev, sid: str, *extra: str):
    """Push from A's CLI in the background; retried when the plugin was skipped in that process (version-probe
    flake, PATH.md "Stolpersteine")."""
    log = ev.root / "push.log"
    for _attempt in range(3):
        t0 = time.time()
        proc = lab.hermes_bg(
            "a", "handover", "to", _lab.LAPTOP_GW, "--session", sid, "--yes", "--json", *extra, log=log
        )
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
        return proc, str(job["job_id"]), t0
    raise _lab.LabError("the background push never started a job (plugin skipped three times)")


def _hidden_since(lab, inst: str, since: float) -> list[dict]:
    path = lab.home(inst) / "state.db"
    conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=10)
    conn.row_factory = sqlite3.Row
    try:
        rows = conn.execute("SELECT id, title, hidden, started_at FROM sessions WHERE started_at >= ?", (since,))
        return [dict(r) for r in rows.fetchall() if r["hidden"]]
    finally:
        conn.close()


def _settle(lab, ev, jid: str | None) -> None:
    """Clean-up, not under test: a job this test left unfinished is cancelled and resumed through A's gateway, so
    its conductor aborts and B's participant slot is freed (B allows two unfinished jobs per peer; a cancel alone
    leaves a suspended job and its slot as they are, ``reports/L3-findings.md`` L3-F07). Waits ≤ 120 s."""
    if not jid or (lab.job_row("a", jid) or {}).get("state") in {"done", "aborted"}:
        return
    ev.cli("settle-cancel", lab.handover("a", "cancel", jid, timeout=180))
    scratch = lab.new_session("a", lab.work("a"))
    out, _ = lab.slash("a", scratch, f"/handover resume {jid}", timeout=180)
    ev.write("settle-resume.txt", out)
    _lab.wait_for(
        lambda: (lab.job_row("b", jid, "target") or {}).get("state") in {None, "aborted", "expired", "committed"},
        120,
        poll=1.0,
    )
    ev.write("settle-state.json", {"a": lab.job_row("a", jid), "b": lab.job_row("b", jid, "target")})


def _block_bytes(lab, inst: str) -> int:
    rows = lab.ledger_rows(inst, "SELECT COALESCE(SUM(size), 0) AS n FROM blocks")
    return int(rows[0]["n"]) if rows else 0


def _short_timeouts(lab) -> None:
    lab.setting("a", "lease_ttl_s", "30")
    lab.setting("a", "stall_timeout_min", "1")


@pytest.mark.e2e_timeout(600)
def test_e05_abort_mid_transfer(lab, ev):
    """E5: B's dashboard (``serve-b``, the listener A pushes to) is stopped while the workspace is moving, then the
    user cancels: A's session is usable within ``lease_ttl_s`` + 5 s of the cancel, the primary lock was never
    applied, and nothing hidden is left on B.

    Deviations: the stop happens at the first observed state of the transfer phase (EXPORTING … DATA_STAGED;
    the observed state is recorded) instead of exactly at DATA_STAGED — with 120 MiB the window is a few seconds.
    ``lease_ttl_s`` is set to 30 and ``stall_timeout_min`` to 1 on A for the test (restored). "After TTL sweep"
    on B is not awaited (``staging_ttl_hours`` has hour granularity, default 24): the test asserts that B holds no
    hidden import created during the job and that B's target job is not committed."""
    lease_ttl = 30
    proc = None
    with lab.preserved("a"):
        _short_timeouts(lab)
        src = _big_source(lab, "e05proj")
        marks_before = lab.marks("a", src.sid)
        try:
            proc, jid, t0 = _push_bg(lab, ev, src.sid)
            seen = lab.wait_job("a", jid, TRANSFER_STATES | {"target_preparing", "aborted", "done"}, poll=0.02)
            ev.write("injection.json", {"job": jid, "seen": seen})
            assert seen in TRANSFER_STATES, f"scaffold: injection window missed (first observed state {seen})"
            lab.stop("serve-b")
            parked = lab.wait_job("a", jid, {"suspended", "aborted", "done"}, timeout=240)
            ev.write("after-stop.json", {"state": parked, "row": lab.job_row("a", jid)})
            cancel = lab.handover("a", "cancel", jid, timeout=180)
            ev.cli("cancel", cancel)
            t_cancel = time.monotonic()
            turn = lab.turn("a", src.sid, "E05 is the session usable again?", timeout=lease_ttl + 60)
            usable_after = time.monotonic() - t_cancel
            ev.write("turn-a.json", {"turn": turn.brief(), "usable_after_s": usable_after})
            final = lab.job_row("a", jid)
            ev.write("job-a-final.json", {"row": final, "source": lab.source_row("a", jid)})
        finally:
            _lab.kill_tree(proc)
            if not lab.running("serve-b"):
                lab.start("serve-b")
            _settle(lab, ev, locals().get("jid"))
    assert parked in {"suspended", "aborted"}, parked
    assert cancel.rc == 0, cancel.brief()
    assert turn.ok and "handed over" not in turn.text, turn.brief()
    assert usable_after <= lease_ttl + 5 + 5, f"A usable only after {usable_after:.1f}s (lease_ttl_s={lease_ttl})"
    assert lab.marks("a", src.sid) == marks_before
    source = lab.source_row("a", jid) or {}
    assert not source.get("prepared_at"), source
    hidden = _hidden_since(lab, "b", t0)
    ev.write("hidden-b.json", hidden)
    assert not hidden, hidden
    target = lab.job_row("b", jid, "target") or {}
    ev.write("target-b.json", target)
    assert target.get("state") not in {"committing", "committed"}, target


@pytest.mark.e2e_timeout(900)
def test_e05b_lid_close_and_resume(lab, ev):
    """E5b: the receiving side goes away after all workspace data is staged (lid closed), the job is SUSPENDED;
    after the lid opens, ``/handover resume`` (through A's gateway) finishes it: the need-set of the resumed run is
    below 10 % of the data, the pack is reused, and the result is as in E1 (hashes and repository identical).

    Deviations: "lid close" is ``labctl.py stop serve-b`` at the first observed TARGET_PREPARING/PREPARED (a SIGSTOP
    would hold each of the conductor's six attempts for the 60-s read timeout, ≈ 6 min, before SUSPENDED); "lid
    open" is ``labctl.py start serve-b``. The need-set is measured as the bytes added to B's block store during
    the resumed run divided by the 120 MiB of random data; "pack reused" as exactly one ``pack`` op on A."""
    proc = None
    with lab.preserved("a"):
        _short_timeouts(lab)
        src = _big_source(lab, "e05bproj")
        hashes_a = lab.hashes("a", src.sid)
        repo_a = _lab.repo_state(src.repo)
        try:
            proc, jid, _t0 = _push_bg(lab, ev, src.sid)
            seen = lab.wait_job(
                "a", jid, {"target_preparing", "target_prepared", "source_preparing", "aborted", "done"}, poll=0.02
            )
            ev.write("injection.json", {"job": jid, "seen": seen})
            assert seen in {"target_preparing", "target_prepared"}, f"scaffold: window missed ({seen})"
            lab.stop("serve-b")
            parked = lab.wait_job("a", jid, {"suspended", "aborted", "done"}, timeout=300)
            ev.write("parked.json", {"state": parked, "row": lab.job_row("a", jid)})
            assert parked == "suspended", parked
            proc.wait(120)
            lab.start("serve-b")
            bytes_before = _block_bytes(lab, "b")
            scratch = lab.new_session("a", lab.work("a"))
            out, events = lab.slash("a", scratch, f"/handover resume {jid}")
            ev.write("resume.json", {"output": out, "events": [e.get("type") for e in events]})
            final = lab.wait_job("a", jid, {"done", "aborted", "suspended", "in_doubt"}, timeout=600)
            bytes_after = _block_bytes(lab, "b")
        finally:
            _lab.kill_tree(proc)
            if not lab.running("serve-b"):
                lab.start("serve-b")
            _settle(lab, ev, locals().get("jid"))
    status = lab.job_status("a", jid)
    ev.write("job-a.json", status)
    assert final == "done", status
    need = (bytes_after - bytes_before) / float(BIG_FILES * BIG_FILE_MIB * MiB)
    pack_ops = lab.ledger_rows("a", "SELECT op, digest, state FROM ops WHERE job_id = ? AND op = 'pack'", (jid,))
    ev.write(
        "need-set.json", {"bytes_before": bytes_before, "bytes_after": bytes_after, "need": need, "pack_ops": pack_ops}
    )
    assert need < 0.10, f"need-set {need:.1%} of the data was sent again"
    assert len(pack_ops) == 1, pack_ops
    sid_b = str((status["job"].get("ack") or {}).get("session_id") or "")
    hashes_b = lab.hashes("b", sid_b)
    for key in ("h_all", "h_rows", "h_model", "display_norm"):
        assert hashes_b[key] == hashes_a[key], key
    dest = lab.row("b", sid_b).get("cwd")
    repo_b = _lab.repo_state(dest)
    for key in ("head", "status", "index", "refs", "stash", "tree"):
        assert repo_b[key] == repo_a[key], key


def _committing_push(lab, ev, name: str):
    src = lab.make_source("a", name, turns=("commit under stress",))
    pid_b = lab.pid("serve-b")
    proc, jid, _t0 = _push_bg(lab, ev, src.sid)
    seen = lab.wait_job(
        "a", jid, {"committing", "in_doubt", "committed", "retiring", "done", "aborted", "suspended"}, poll=0.005
    )
    return src, proc, jid, seen, pid_b


@pytest.mark.e2e_timeout(600)
def test_e10_long_commit_completes_and_source_stays_prepared(lab, ev):
    """E10 (SK H5), first half: the target stalls right after the commit started; the commit completes later; A
    keeps the primary lock (ended, archived, hidden, renamed) the whole time until the retire.

    Deviation: no fault hook makes the commit op take 90 s. Instead ``serve-b`` (which runs B's commit op) is
    SIGSTOPped the moment A's conductor journals COMMITTING and resumed after 75 s — longer than the dialer's
    60-s read timeout, so the commit request is lost from the conductor's view at least once (it retries; the
    commit op is idempotent). The connection itself is not torn down."""
    proc = None
    samples = []
    with lab.preserved("a"):
        lab.setting("a", "stall_timeout_min", "5")
        try:
            src, proc, jid, seen, pid_b = _committing_push(lab, ev, "e10aproj")
            with _lab.stopped(pid_b):
                ev.write("injection.json", {"job": jid, "seen": seen})
                assert seen == "committing", f"scaffold: window missed ({seen})"
                t_stop = time.monotonic()
                while time.monotonic() - t_stop < 75:
                    samples.append(
                        {
                            "t": round(time.monotonic() - t_stop, 1),
                            "state": (lab.job_row("a", jid) or {}).get("state"),
                            "marks": {s: lab.marks("a", s) for s in lab.lineage("a", src.sid)},
                        }
                    )
                    time.sleep(5)
            final = lab.wait_job("a", jid, {"done", "aborted", "suspended"}, timeout=360)
        finally:
            _lab.kill_tree(proc)
            _settle(lab, ev, locals().get("jid"))
    ev.write("samples.json", samples)
    status = lab.job_status("a", jid)
    ev.write("job-a.json", status)
    for sample in samples:
        assert sample["state"] in {"committing", "in_doubt"}, sample
        for seg, m in sample["marks"].items():
            assert m["ended"] and m["archived"] and m["hidden"] and "(handed over to" in str(m["title"]), (seg, sample)
    assert final == "done", status
    sid_b = str((status["job"].get("ack") or {}).get("session_id") or "")
    row_b = lab.row("b", sid_b)
    assert row_b and not row_b.get("hidden"), row_b
    assert (lab.source_row("a", jid) or {}).get("state") == "retired"


@pytest.mark.e2e_timeout(600)
def test_e10_reclaim_force_while_commit_is_in_doubt(lab, ev):
    """E10 (I2), second half: while the commit outcome is unknown, a plain ``reclaim`` is refused with a hint to
    ``--force``; ``reclaim --force`` works, warns that the target may already be active, and lifts every mark; the
    job ends (not stuck) once the target answers again, with a warning that the copies are independent.

    Deviation as in the first half: SIGSTOP of ``serve-b`` at COMMITTING instead of a 90-s commit fault hook."""
    proc = None
    with lab.preserved("a"):
        lab.setting("a", "stall_timeout_min", "5")
        try:
            src, proc, jid, seen, pid_b = _committing_push(lab, ev, "e10bproj")
            with _lab.stopped(pid_b):
                ev.write("injection.json", {"job": jid, "seen": seen})
                assert seen == "committing", f"scaffold: window missed ({seen})"
                time.sleep(3)
                plain = lab.handover("a", "reclaim", src.sid, timeout=180)
                ev.cli("reclaim-plain", plain)
                marks_plain = {s: lab.marks("a", s) for s in lab.lineage("a", src.sid)}
                forced = lab.handover("a", "reclaim", src.sid, "--force", timeout=180)
                ev.cli("reclaim-force", forced)
                marks = {s: lab.marks("a", s) for s in lab.lineage("a", src.sid)}
                ev.write("marks-after-reclaim.json", marks)
            final = lab.wait_job("a", jid, {"done", "aborted", "suspended", "in_doubt"}, timeout=420)
        finally:
            _lab.kill_tree(proc)
            _settle(lab, ev, locals().get("jid"))
    status = lab.job_status("a", jid)
    ev.write("job-a.json", status)
    assert "--force" in plain.text, plain.brief()
    assert all(m["ended"] and m["hidden"] for m in marks_plain.values()), marks_plain
    assert forced.rc == 0, forced.brief()
    assert "warning" in forced.text.lower() and "active" in forced.text.lower(), forced.text
    for seg, m in marks.items():
        assert not m["ended"] and not m["archived"] and not m["hidden"], (seg, m)
        assert m["title"] == src.title or seg != src.sid, (seg, m)
    assert final in {"done", "aborted"}, status
    warnings = " ".join(str(w) for w in status["job"].get("warnings") or [])
    if final == "done":
        assert "independent" in warnings, status["job"]
    # last on purpose: the refusal is a failed command for scripts (DESIGN §14.2 exit code 3 busy/conflict)
    assert plain.rc != 0 and not plain.data.get("ok"), plain.brief()


@pytest.mark.e2e_timeout(420)
def test_e12_busy_source_and_wait(lab, ev):
    """E12 (R10, BX M6): while a turn runs on A's session, ``take`` from B answers ``session_busy`` (exit code 3);
    with ``--wait`` the handover starts after that turn and B's copy ends with the turn's reply."""
    src = lab.make_source("a", "e12proj", repo=False, turns=("busy source",))
    slow = f"E12-SLOW-{_lab.uid()}"
    done_text = f"E12-SLOW-DONE-{_lab.uid()}"
    rule = {"id": slow, "when": {"kind": "main", "last_user_contains": slow}, "reply": {"text": done_text, "delay": 25}}
    thread = None
    with lab.llm_rules([rule]):
        mark = lab.llm_mark()
        thread, box = lab.turn_bg("a", src.sid, f"please think long {slow}", timeout=120)
        running = _lab.wait_for(lambda: any(slow in r.get("last_user", "") for r in lab.llm_since(mark)), 60)
        assert running, "scaffold: the slow turn never reached the model"
        busy = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=180)
        ev.cli("take-busy", busy)
        waited = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", "--wait", timeout=420)
        ev.cli("take-wait", waited)
        thread.join(120)
    ev.write(
        "slow-turn.json", {"turn": box.get("turn").brief() if box.get("turn") else None, "error": box.get("error")}
    )
    # exit code 3 and the session_busy text of DESIGN §14.1; the --json document carries no error code when the
    # runner (not the command) ends the job (recorded in the evidence, not asserted)
    assert busy.rc == 3, busy.brief()
    assert "A turn is running in this session" in busy.text and "--wait" in busy.text, busy.text
    assert waited.rc == 0, waited.brief()
    status = lab.job_status("b", waited.job_id)
    ev.write("job-b.json", status)
    sid_b = str((status["job"].get("ack") or {}).get("session_id") or "")
    model = [m for m in lab.messages("b", sid_b, inactive=False) if m.get("role") == "assistant"]
    assert model and done_text in _lab.message_text(model[-1]), [_lab.message_text(m)[:80] for m in model[-3:]]
