"""Layer 3 — integrator round 3 (``feat/ctx-handover`` ``ee82897c``) on the real stand.

R2-H1 (review round 2, H1; DECISIONS R1): the first turn of a freshly started gateway must meet a loaded veto index.
Before round 3, ``ensure_started`` returned as soon as the ``VetoIndex`` refresher thread was started — before it had
read the ledger once — and the ``pre_llm_call`` hook of the first turn could let a turn on a handed-over session
through (reviewer harness: 15 and 18 of 20 fresh processes, ``tests/unit/A/test_r3_a_h1_first_turn.py``). Round 3
reads the ledger on the calling thread in ``VetoIndex.start`` (``engine/lock.py``) and starts the index before
identity, store and janitor. The unit test covers spawned processes; this test checks the process that
matters, a real ``hermes gateway run`` restarted through ``labctl.py`` and hit at once over its WebSocket.

Runtime estimate: 3–6 min (one push, then 20 restarts of ``gw-a`` at ≈ 3 s to ``ready`` plus the stop, one turn each).
"""

from __future__ import annotations

import json
import os
import subprocess
import time
from pathlib import Path

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)

MIN_RUNS = 5
"""Fewest counted repetitions the R2-H1 test accepts (lab plan: at least 5)."""
DEFAULT_RUNS = 20
"""Counted repetitions by default (``CTXH_E2E_R2H1_RUNS``), as many as the unit harness's fresh processes."""
LOOP_BUDGET_S = 600.0
"""No new repetition starts after this many seconds of the loop (the test's hard bound is its ``e2e_timeout``)."""
READY_TIMEOUT_S = 180.0
POLL_S = 0.05
TURN_TIMEOUT_S = 120.0
SKIPPED_LINE = b"Plugin 'ctx_handover' skipped"


def _runs() -> int:
    raw = os.environ.get("CTXH_E2E_R2H1_RUNS", "").strip()
    try:
        return max(MIN_RUNS, int(raw)) if raw else DEFAULT_RUNS
    except ValueError:
        return DEFAULT_RUNS


def _ms(seconds: float) -> float:
    return round(seconds * 1000.0, 1)


def _log_since(path: Path, offset: int) -> bytes:
    if not path.exists():
        return b""
    with open(path, "rb") as fh:
        fh.seek(offset if path.stat().st_size >= offset else 0)
        return fh.read()


def _wait_ready(lab, proc: subprocess.Popen, old_pid: int | None) -> dict | None:
    """Poll gw-a's control socket every ``POLL_S`` until the new process answers ``identify`` with ``state: ready``
    (``labctl.py start`` polls every 0.3 s and returns after that); None when it does not within ``READY_TIMEOUT_S``
    or labctl ended without a running gateway."""
    deadline = time.monotonic() + READY_TIMEOUT_S
    while time.monotonic() < deadline:
        desc = lab.ctl.identify("a")
        pid = (desc or {}).get("pid")
        if desc and desc.get("state") == "ready" and pid and pid != old_pid:
            return desc
        if proc.poll() is not None and not lab.ctl.read_pid("gw-a"):
            return None
        time.sleep(POLL_S)
    return None


def _turn_now(lab, sid: str, text: str, t_ready: float) -> tuple[_lab.Turn, dict]:
    """``session.resume`` + ``prompt.submit`` on gw-a right away (``Lab.turn`` with timestamps relative to ready)."""
    times: dict = {}
    gw = lab.gw("a")
    times["ready_to_ws_ms"] = _ms(time.monotonic() - t_ready)
    try:
        resumed = gw.rpc("session.resume", {"session_id": sid}, timeout=60)
        times["ready_to_submit_ms"] = _ms(time.monotonic() - t_ready)
        submit = gw.rpc("prompt.submit", {"session_id": sid, "text": text, "input_id": f"e2e-{_lab.uid(12)}"})
        done = gw.wait(lambda e: e.get("type") in {"message.complete", "error"}, TURN_TIMEOUT_S)
        times["ready_to_done_ms"] = _ms(time.monotonic() - t_ready)
        gw.linger(0.3)
        return _lab.Turn(resumed, submit, done, list(gw.events)), times
    finally:
        gw.close()


def _refusal_rows(lab, sid: str, probe: str) -> dict:
    """The core's withheld placeholder of ``probe`` in the lineage of ``sid`` (``display_metadata.withheld_prompt``)
    and the row stored right after it (the veto reply)."""
    rows: dict = {}
    for seg in lab.lineage("a", lab.tip("a", sid)):
        for m in lab.messages("a", seg):
            rows[m.get("id")] = m
    seq = [m for _id, m in sorted(rows.items(), key=lambda kv: int(kv[0] or 0)) if m.get("role") != "session_meta"]
    for i, m in enumerate(seq):
        if m.get("role") == "user" and probe in str(m.get("display_metadata") or ""):
            nxt = seq[i + 1] if i + 1 < len(seq) else {}
            return {
                "placeholder": {"id": m.get("id"), "content": _lab.message_text(m)[:200]},
                "reply": {"id": nxt.get("id"), "role": nxt.get("role"), "content": _lab.message_text(nxt)[:400]},
            }
    return {}


def _messages_text(rec: dict) -> str:
    return json.dumps((rec.get("body") or {}).get("messages") or [], ensure_ascii=False)


def _vetoed(text: str) -> bool:
    return "handed over to" in text and "Your message was not sent" in text and "/handover reclaim" in text


def _repetition(lab, ev, sid: str, tag: str, i: int) -> dict:
    """One restart of gw-a through labctl and an immediate turn on the locked source (test docstring)."""
    old_pid = lab.pid("gw-a")
    lab.stop("gw-a")
    log = lab.log_path("gw-a")
    offset = log.stat().st_size if log.exists() else 0
    probe = f"R2H1-{i:02d}-{_lab.uid(8)}"
    mark = lab.llm_mark()
    rep: dict = {"i": i, "probe": probe, "old_pid": old_pid}
    proc = None
    try:
        t0 = time.monotonic()
        proc = lab.labctl_bg(
            "start", "gw-a", log=ev.root / f"labctl-start-gw-a-{i:02d}.txt", extra_env={"GW_TOKEN": lab.tokens["a"]}
        )
        desc = _wait_ready(lab, proc, old_pid)
        t_ready, t_ready_wall = time.monotonic(), time.time()
        if desc is None:
            raise _lab.LabError(f"scaffold: gw-a did not become ready within {READY_TIMEOUT_S:.0f} s (repetition {i})")
        new_pid = int(desc["pid"])
        rep.update(new_pid=new_pid, start_to_ready_s=round(t_ready - t0, 2))
        turn, times = _turn_now(lab, sid, probe, t_ready)
        rep.update(times)
        try:
            rep["labctl_rc"] = proc.wait(timeout=READY_TIMEOUT_S + 30)
        except subprocess.TimeoutExpired:
            rep["labctl_rc"] = None
    finally:
        _lab.kill_tree(proc)
    rep["pidfile_pid"] = lab.pid("gw-a")
    records = lab.llm_since(mark)
    ours = {r.get("seq") for r in records if probe in _messages_text(r) or tag in _messages_text(r)}
    new = {r.get("seq") for r in records if str((r.get("caller") or {}).get("pid")) == str(new_pid)}
    rep["llm"] = [
        {
            "seq": r.get("seq"),
            "kind": r.get("kind"),
            "caller_pid": (r.get("caller") or {}).get("pid"),
            "after_ready_ms": _ms(float(r.get("t") or 0.0) - t_ready_wall),
            "of_session": r.get("seq") in ours,
        }
        for r in records
    ]
    rep["reached"] = sorted(ours, key=str)
    rep["reached_from_new_gateway"] = sorted(ours & new, key=str)
    rep["other_from_new_gateway"] = sorted(new - ours, key=str)
    rep["turn"] = {"ok": turn.ok, "text": turn.text[:600], "resume_error": (turn.resume or {}).get("error")}
    rep["db"] = _refusal_rows(lab, sid, probe)
    reply = rep["db"].get("reply") or {}
    stored = reply.get("role") == "assistant" and "Your message was not sent" in str(reply.get("content"))
    rep["refused"] = _vetoed(turn.text) and stored
    rep["passed_lock"] = bool(ours)
    rep["plugin_skipped"] = SKIPPED_LINE in _log_since(log, offset)
    rep["plugin_help"] = None
    if not rep["refused"]:
        # post hoc, the race is over: did this gateway load the plugin at all? (no veto without it, README guarantee 5)
        out, _ = lab.slash("a", lab.new_session("a", lab.work("a")), "/handover help", timeout=60, linger=0.2)
        rep["plugin_help"] = "ctx_handover moves" in out
    rep["flake"] = not rep["refused"] and (rep["plugin_skipped"] or rep["plugin_help"] is False)
    rep["counted"] = not rep["flake"]
    return rep


@pytest.mark.e2e_timeout(1200)
def test_r2h1_first_turn_of_a_restarted_gateway_meets_the_lock(lab, ev):
    """R2-H1 (DECISIONS R1, DESIGN §7.6): right after a restart, gw-a never lets a turn on a handed-over session reach
    the model.

    Steps:

    1. A session on A (repository of its own, ``r2h1<run id>``) is pushed to B (``hermes handover to laptopgw
       --session <sid> --yes`` on A, job ``done``); its lineage is locked (ended ``handed_over``, archived, hidden).
    2. ``CTXH_E2E_R2H1_RUNS`` times (default 20, at least 5): ``labctl.py stop gw-a``, then ``labctl.py start gw-a``
       in the background while the test polls gw-a's control socket every 50 ms; at the first ``identify`` with
       ``state: ready`` from the new process it opens the WebSocket at once and sends ``session.resume`` +
       ``prompt.submit`` with a fresh probe text on the locked source — no wait, no ``/handover`` call and no plugin
       probe before the turn (``Lab.start`` runs ``/handover help`` first and would warm the plugin).
    3. Evidence per repetition (``r2h1-runs.json``): old/new gateway pid, start → ready in s, ready → WebSocket open /
       ``prompt.submit`` sent / turn done in ms, every fake-model record since the restart (seq, kind, ``caller.pid``,
       ms after ready), the turn's text and the stored refusal rows; totals in ``r2h1-summary.json``.

    Soll (README "What the lock on the source guarantees" (5): while the plugin is loaded every new turn on the lineage
    is refused, and the first message after Hermes starts waits up to 5 s for the plugin's ledger), per counted
    repetition:

    * no fake-model record since the restart (the window starts after ``labctl.py stop``) carries the probe or the
      session's tag ``[r2h1<run id>]``, whoever called; each record lists its ``caller.pid``, so a hit is attributed to
      the new gateway (``reached_from_new_gateway``) and other requests of that process are named
      (``other_from_new_gateway``, evidence only: not this session's). The count of turns that passed the lock is 0 of
      the counted repetitions (reference: the reviewer harness 15 and 18 of 20 before the fix, the integrator 0 of 100
      after it, commit ``ee82897c``);
    * the turn ends with the veto text ("handed over to … Your message was not sent … /handover reclaim"), and the
      session holds the core's withheld placeholder of the probe followed by an assistant row with that text.

    Lab flake (recorded, not counted, repeated at most ``runs // 2`` times): a gateway that came up without the plugin
    (version probe of a source install under host load: ``Plugin 'ctx_handover' skipped`` in ``run/gw-a.log``, or
    ``/handover help`` unknown in that process afterwards) has no veto by design (guarantee (5) holds while the plugin
    is loaded; marks (1)–(4) hold without it). Hard bounds: ``e2e_timeout`` 1200 s; no repetition starts after 600 s
    of the loop (then at least 5 counted ones are required); ``ready`` within 180 s; a turn within 120 s. ``finally``:
    each background ``labctl.py`` is waited for or killed, and ``gw-a`` is restarted through ``Lab.restart`` (fixed
    token, plugin check) so the stand runs as before.

    Red proof (not run): the same test on the e2e branch's state before round 3 (``edd53876``), where
    ``VetoIndex.start`` only started the refresher thread."""
    runs = _runs()
    name = f"r2h1{lab.run_id}"
    src = lab.make_source("a", name, dirty=False, turns=("locked source",))
    pushed = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--yes", timeout=600)
    ev.cli("push", pushed)
    assert pushed.rc == 0 and pushed.job_id, pushed.brief()
    assert (lab.job_row("a", pushed.job_id) or {}).get("state") == "done", lab.job_row("a", pushed.job_id)
    marks = {s: lab.marks("a", s) for s in lab.lineage("a", src.sid)}
    ev.write("source-marks.json", marks)
    for seg, m in marks.items():
        assert m["ended"] and m["end_reason"] == "handed_over" and m["archived"] and m["hidden"], (seg, m)

    reps: list[dict] = []
    t_loop = time.monotonic()
    try:
        while (
            sum(r["counted"] for r in reps) < runs
            and sum(r["flake"] for r in reps) < runs // 2
            and time.monotonic() - t_loop < LOOP_BUDGET_S
        ):
            reps.append(_repetition(lab, ev, src.sid, f"[{name}]", len(reps) + 1))
    finally:
        ev.write("r2h1-runs.json", reps)
        lab.restart("gw-a")
    counted = [r for r in reps if r["counted"]]
    passed = [r["i"] for r in counted if r["passed_lock"]]
    unrefused = [r["i"] for r in counted if not r["refused"]]
    gaps = [r["ready_to_submit_ms"] for r in counted]
    others = {r["i"]: r["other_from_new_gateway"] for r in reps if r["other_from_new_gateway"]}
    summary = {
        "runs_wanted": runs,
        "repetitions": len(reps),
        "counted": len(counted),
        "flakes": [r["i"] for r in reps if r["flake"]],
        "passed_the_lock": f"{len(passed)}/{len(counted)}",
        "passed_runs": passed,
        "reached_from_new_gateway": {r["i"]: r["reached_from_new_gateway"] for r in counted if r["passed_lock"]},
        "other_requests_from_new_gateway": others,
        "unrefused_runs": unrefused,
        "ready_to_submit_ms": {"min": min(gaps, default=None), "max": max(gaps, default=None), "all": gaps},
        "loop_s": round(time.monotonic() - t_loop, 1),
    }
    ev.write("r2h1-summary.json", summary)
    assert len(counted) >= MIN_RUNS, f"scaffold: only {len(counted)} repetitions with the plugin loaded: {summary}"
    assert not passed, f"{len(passed)}/{len(counted)} first turns of a restarted gw-a reached the model: {summary}"
    assert not unrefused, f"turns without the veto text or its stored refusal rows: {unrefused} ({summary})"
