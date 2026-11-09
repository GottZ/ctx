"""Layer 3 — user flows around a handover (TESTPLAN §4 E14, E15, E17, E19, E20, E21).

Configuration changes (``plugins.isolation``, ``allow_remote_continue``, approvals, compression mode, plugin
enable state) are made with ``hermes config set`` / ``hermes plugins …`` and undone by restoring ``config.yaml``
and ``.env`` byte-exact, followed by a restart of the affected components through ``labctl.py``.

Runtime estimate: 8–12 min (E14 ≈ 2 min with two restarts, E15 ≈ 1 min, E17 ≈ 1 min, E19 ≈ 3 min with two
pushes and serve restarts, E20 ≈ 2–3 min with gateway restarts).
"""

from __future__ import annotations

import re
import time

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)


def _pull(lab, ev, sid: str, name: str = "take") -> tuple[str, str]:
    res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", timeout=600)
    ev.cli(name, res)
    assert res.rc == 0 and res.job_id, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write(f"{name}-job.json", status)
    assert status["job"].get("state") == "done", status
    return res.job_id, str((status["job"].get("ack") or {}).get("session_id") or "")


@pytest.mark.e2e_timeout(900)
def test_e14_host_isolation(lab, ev):
    """E14 (L15, §15.3): with ``plugins.isolation: host`` on B, ``/handover take`` through B's gateway works, the
    progress is visible through ``/handover status``, ``doctor`` names the missing CLI and the buffered request
    bodies, and ``hermes handover`` does not exist in a CLI process."""
    src = lab.make_source("a", "e14proj", repo=False, turns=("host isolation",))
    with lab.preserved("b", restart=("gw-b", "serve-b")):
        lab.config_set("b", "plugins.isolation", "host")
        lab.restart("gw-b", "serve-b")
        scratch = lab.new_session("b", lab.work("b"))
        doctor, _ = lab.slash("b", scratch, "/handover doctor", timeout=300)
        ev.write("doctor.txt", doctor)
        t0 = time.time()
        out, events = lab.slash("b", scratch, f"/handover take {_lab.SERVER} {src.sid} --yes", timeout=300)
        ev.write("take.json", {"output": out, "events": [e.get("type") for e in events]})
        job = lab.wait_new_job("b", t0, timeout=60)
        jid = str(job["job_id"])
        samples = []
        state = None
        deadline = time.monotonic() + 600
        while time.monotonic() < deadline:
            text, _ = lab.slash("b", scratch, f"/handover status {jid}", timeout=120)
            state = (lab.job_row("b", jid) or {}).get("state")
            samples.append({"state": state, "status": text})
            if state in _lab.STOP_STATES | {"suspended", "in_doubt"}:
                break
            time.sleep(2)
        ev.write("status-samples.json", samples)
        cli = lab.hermes("b", "handover", "help", timeout=180)
        ev.write("cli.txt", f"rc={cli.returncode}\n{cli.stdout}\n{cli.stderr}")
    assert "is running" in out, out
    assert state == "done", samples[-1:]
    assert any(jid[-6:].upper() in s["status"].upper() or jid in s["status"] for s in samples), samples[-1:]
    assert "plugin isolation" in doctor and "CLI" in doctor and "buffers request bodies" in doctor, doctor
    assert cli.returncode != 0 or "ctx_handover moves" not in cli.stdout, cli.stdout[-800:]


_UNAWARE = re.compile(
    r"(?:not|never) (?:be )?(?:told|notified|informed)|(?:does not|doesn't|will not|won't) (?:learn|know|hear)"
    r"|(?:is|stays) (?:not|un)aware|(?:learns|knows|hears) nothing"
)


def _assert_reclaim_notice(text: str, peer: str) -> None:
    """R35: after a finished handover, ``reclaim`` says explicitly that the copy on ``peer`` stays active and does not
    learn of the reclaim (pull, or the target is not reachable)."""
    lines = [line.lower() for line in text.splitlines() if peer.lower() in line.lower()]
    assert any("stays active" in line or "remains active" in line for line in lines), (
        f"R35: reclaim does not say that the copy on {peer} stays active: {text!r}"
    )
    assert _UNAWARE.search(text.lower()), f"R35: reclaim does not say that the copy on {peer} learns nothing: {text!r}"


@pytest.mark.e2e_timeout(600)
def test_e15_reclaim(lab, ev):
    """E15 (§7.7, R35): after a pull, a vetoed attempt on A leaves a placeholder pair; ``reclaim`` makes A's session
    active again (marks undone, original title), rewinds the placeholder rows (model history as before the
    handover), and leaves B's copy untouched. Last check (R35, replaces the notification of the target, L3-F13): the
    ``reclaim`` output (``--json`` text) says explicitly that the copy on B stays active and does not learn of it."""
    src = lab.make_source("a", "e15proj", repo=False, turns=("reclaim me", "and again"))
    before = lab.hashes("a", src.sid)
    _jid, sid_b = _pull(lab, ev, src.sid)
    copy_b = lab.hashes("b", sid_b)
    probe = f"E15-VETO-{_lab.uid()}"
    # Desktop path (WebSocket): ``hermes chat --resume -q`` would end the session as ``cli_close`` on exit
    veto = lab.turn("a", src.sid, probe)
    ev.write("veto.json", veto.brief())
    assert "handed over to" in veto.text, veto.brief()
    placeholders = [
        m
        for m in lab.messages("a", src.sid)
        if probe in str(m.get("display_metadata") or "") or "Message withheld" in _lab.message_text(m)
    ]
    ev.write("placeholders.json", placeholders)
    rec = lab.handover("a", "reclaim", src.sid, timeout=300)
    ev.cli("reclaim", rec)
    assert rec.rc == 0, rec.brief()
    assert "active here again" in rec.text.lower() or "reclaimed" in rec.text.lower(), rec.text
    marks = lab.marks("a", src.sid)
    ev.write("marks-a.json", marks)
    assert not marks["ended"] and not marks["archived"] and not marks["hidden"], marks
    assert marks["title"] == src.title, marks
    after = lab.hashes("a", src.sid)
    ev.write("hashes-a.json", {"before": before, "after": after})
    for key in ("h_rows", "h_model", "model_norm"):
        assert after[key] == before[key], key
    rows = {m.get("id"): m for m in lab.messages("a", src.sid)}
    assert placeholders and all(not rows.get(p.get("id"), {}).get("active") for p in placeholders), placeholders
    assert lab.hashes("b", sid_b) == copy_b
    assert lab.marks("b", sid_b)["ended"] is False
    _assert_reclaim_notice(rec.text, _lab.LAPTOP)


@pytest.mark.e2e_timeout(600)
def test_e15_reclaim_while_the_target_is_unreachable_says_so(lab, ev):
    """E15 (§7.7, R35), second case: after a finished push A→B, B's listener (``serve-b``) is stopped, then
    ``hermes handover reclaim <sid>`` runs on A in plain CLI form (no ``--json``). The output says explicitly that the
    copy on B stays active and does not learn of it; ``serve-b`` is started again in ``finally``."""
    src = lab.make_source("a", "e15uproj", repo=False, turns=("reclaim while the laptop is away",))
    pushed = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--yes", timeout=600)
    ev.cli("push", pushed)
    assert pushed.rc == 0 and pushed.job_id, pushed.brief()
    assert (lab.job_row("a", pushed.job_id) or {}).get("state") == "done", lab.job_row("a", pushed.job_id)
    try:
        lab.stop("serve-b")
        t0 = time.monotonic()
        res = lab.hermes("a", "handover", "reclaim", src.sid, timeout=300)
        elapsed = time.monotonic() - t0
    finally:
        if not lab.running("serve-b"):
            lab.start("serve-b")
    text = res.stdout + res.stderr
    ev.write("reclaim-cli.json", {"rc": res.returncode, "elapsed_s": round(elapsed, 2), "output": text})
    assert res.returncode == 0, text
    assert "active here again" in text.lower() or "reclaimed" in text.lower(), text
    assert lab.marks("a", src.sid)["ended"] is False
    _assert_reclaim_notice(text, _lab.LAPTOP_GW)


@pytest.mark.e2e_timeout(420)
def test_e17_offer_flow(lab, ev):
    """E17 (BX H2/H4, O6): ``/handover to laptop`` on A, where the laptop only dials in, leaves an offer with a
    refinement (made on A, where the session lives); ``take --offer`` on B claims it and the note carries that
    refinement; the offer is never claimed automatically before."""
    note = f"E17-NOTE-{_lab.uid()}"
    src = lab.make_source("a", "e17proj", repo=False, turns=("offer this",))
    with lab.llm_rules([_lab.refine_rule(note)]):
        offer = lab.handover("a", "to", _lab.LAPTOP, "--session", src.sid, timeout=300)
    ev.cli("offer", offer)
    offers = lab.ledger_rows("a", "SELECT * FROM offers WHERE session_id = ?", (src.sid,))
    ev.write("offers-a.json", offers)
    marks = lab.marks("a", src.sid)
    oid = str(offers[0]["offer_id"]) if offers else ""
    take = lab.handover("b", "take", _lab.SERVER, "--offer", oid or "missing", "--yes", timeout=600)
    ev.cli("take-offer", take)
    status = lab.job_status("b", take.job_id) if take.job_id else {"job": {}}
    ev.write("job-b.json", status)
    sid_b = str((status["job"].get("ack") or {}).get("session_id") or "")
    arrival = lab.arrival("b", sid_b) or {} if sid_b else {}
    claimed = lab.ledger_rows("a", "SELECT claimed_job FROM offers WHERE offer_id = ?", (oid,))
    ev.write("after-take.json", {"arrival": arrival, "claimed": claimed})
    assert offer.rc == 0 and "Offer" in offer.text, offer.brief()
    assert len(offers) == 1 and not offers[0].get("claimed_job"), offers
    assert marks["ended"] is False, marks
    assert take.rc == 0 and status["job"].get("state") == "done", take.brief()
    assert claimed and claimed[0]["claimed_job"], claimed
    assert note in str(offers[0].get("refinement") or ""), offers
    assert note in str(arrival.get("note")), arrival


@pytest.mark.e2e_timeout(900)
def test_e19_push_and_continue(lab, ev):
    """E19 (R10, R22, L5): ``/handover to server --continue "<prompt>"`` from B with ``allow_remote_continue: true`` on
    A: exactly one single-query turn runs on A (it reaches the fake model with the prefixed prompt and the note),
    and the session is adopted by A's gateway. With approvals switched off on A the handover succeeds and the
    continuation is refused (``continue_refused``), no turn runs.

    "Approvals off" is set as ``approvals.single_query_mode: approve`` (DESIGN §11.5 names it next to
    ``approvals.mode: off``; the word ``off`` would be written as a YAML boolean by ``hermes config set``)."""
    pids: list[int] = []
    with lab.preserved("a", restart=("serve-a", "gw-a")):
        lab.setting("a", "allow_remote_continue", "true")
        lab.restart("serve-a")
        src = lab.make_source("b", "e19proj", repo=False, turns=("closing time",))
        prompt = f"E19-CONTINUE-{_lab.uid()}"
        mark = lab.llm_mark()
        try:
            res = lab.handover("b", "to", _lab.SERVER, "--session", src.sid, "--continue", prompt, "--yes", timeout=600)
            ev.cli("push-continue", res)
            assert res.rc == 0, res.brief()
            status = lab.job_status("b", res.job_id)
            ev.write("job-b.json", status)
            cont = status["job"].get("continuation") or {}
            if cont.get("pid"):
                pids.append(int(cont["pid"]))
            sid_a = str((status["job"].get("ack") or {}).get("session_id") or "")
            hit = _lab.wait_for(lambda: [r for r in lab.llm_since(mark) if prompt in _lab.last_user_message(r)], 180)
            time.sleep(5)
            turns = [r for r in lab.llm_since(mark) if r.get("kind") == "main" and prompt in _lab.last_user_message(r)]
            adopted = _lab.wait_for(lambda: lab.row("a", sid_a).get("session_key"), 60)
            ev.write("continuation.json", {"cont": cont, "turns": _lab.compact_records(turns), "adopted": adopted})
            assert cont.get("started") and hit, cont
            assert len(turns) == 1, len(turns)
            user = _lab.last_user_message(turns[0])
            assert "[continuation requested by" in user and _lab.NOTE_MARK in user, user[-2000:]
            assert adopted, lab.row("a", sid_a)

            lab.config_set("a", "approvals.single_query_mode", "approve")
            lab.restart("serve-a")
            src2 = lab.make_source("b", "e19bproj", repo=False, turns=("approvals off",))
            prompt2 = f"E19-REFUSED-{_lab.uid()}"
            mark2 = lab.llm_mark()
            res2 = lab.handover(
                "b", "to", _lab.SERVER, "--session", src2.sid, "--continue", prompt2, "--yes", timeout=600
            )
            ev.cli("push-continue-approvals-off", res2)
            status2 = lab.job_status("b", res2.job_id) if res2.job_id else {"job": {}}
            ev.write("job-b-2.json", status2)
            cont2 = status2["job"].get("continuation") or {}
            if cont2.get("pid"):
                pids.append(int(cont2["pid"]))
            time.sleep(10)
            late = [r for r in lab.llm_since(mark2) if prompt2 in _lab.last_user_message(r)]
        finally:
            for pid in pids:
                _lab.kill_pid(pid)
    assert res2.rc == 0 and status2["job"].get("state") == "done", res2.brief()
    err = cont2.get("error") or {}
    assert err.get("code") == "continue_refused" or "continue_refused" in str(cont2), cont2
    assert "allow" in res2.text.lower() or "approval" in res2.text.lower(), res2.text
    assert not late, "a continuation turn ran although approvals are off"


@pytest.mark.e2e_timeout(900)
def test_e20_rotation_after_retire(lab, ev):
    """E20 (SK B1, R1): ``/compress`` with rotation (``compression.in_place: false``) on the retired source creates a
    child that is still vetoed; with the plugin disabled, every segment of the lineage is still ended, archived,
    hidden and renamed (Hermes' own marks).

    Observed (run 5): Hermes refuses a rotating compaction of the ended source — ``session.mutate compress`` answers
    ``internal error``, gateway log ``RuntimeError: Compression parent already ended`` — so no child exists. The test
    then records that refusal (the lineage stays one segment, the transcript unchanged) instead of the child's veto,
    and checks the marks with the plugin disabled on that lineage."""
    with lab.preserved("a", restart=("gw-a",)):
        try:
            # the compression mode is frozen into a session's policy when the gateway creates it, so the source
            # session is created after the switch (run 4: a session created before it compacted in place)
            lab.config_set("a", "compression.in_place", "false")
            lab.restart("gw-a")
            src = lab.make_source("a", "e20proj", repo=False, turns=("one", "two", "three", "four", "five"))
            _pull(lab, ev, src.sid)
            before = lab.hashes("a", src.sid)
            out = lab.compress("a", src.sid)
            ev.write("compress.json", out)
            child = lab.tip("a", src.sid)
            lineage = lab.lineage("a", child)
            ev.write("lineage.json", {"child": child, "lineage": lineage})
            rotated = child != src.sid and src.sid in lineage
            reached, veto = [], None
            if rotated:
                probe = f"E20-VETO-{_lab.uid()}"
                mark = lab.llm_mark()
                veto = lab.turn("a", child, probe)
                ev.write("veto-child.json", veto.brief())
                reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
            after = lab.hashes("a", src.sid)
            disabled = lab.hermes("a", "plugins", "disable", "ctx_handover", timeout=300)
            ev.write("disable.txt", disabled.stdout + disabled.stderr)
            lab.restart("gw-a")
            marks = {s: lab.marks("a", s) for s in lineage}
            ev.write("marks-plugin-disabled.json", marks)
        finally:
            lab.hermes("a", "plugins", "enable", "ctx_handover", timeout=300)
    if rotated:
        assert not reached, "the child of the rotation reached the model"
        assert veto is not None and "handed over to" in veto.text, veto.brief() if veto else None
    else:
        assert out["reply"].get("error"), out
        assert after["h_all"] == before["h_all"], "a refused compaction changed the transcript"
    for seg, m in marks.items():
        assert m["ended"] and m["archived"] and m["hidden"], (seg, m)
        assert re.search(r"\(handed over to ", str(m["title"] or "")), (seg, m)


def test_e21_messaging_surface():
    """E21 (R24): ``/handover pair`` from a messaging adapter is refused."""
    pytest.skip(
        "not reproducible in the lab: neither home has a messaging adapter, and gw.py drives only the local "
        "platform (handler kwargs platform='local', LAB L2). Covered at unit level by "
        "tests/unit/F/test_commands.py::test_admin_verbs_refused_on_messaging_platform"
    )
