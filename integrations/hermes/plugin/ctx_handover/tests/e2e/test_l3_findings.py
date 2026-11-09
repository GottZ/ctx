"""Layer 3 — regression tests for the findings of the two-gateway rehearsal (``reports/L3-findings.md`` L3-F01…F06).

Each test checks the target behaviour the finding names (with its source); on the production state ``abcbd8a7``
they are expected red — phase 3 shows which ones the integration fixed. Every test restores what it changes.

Runtime estimate: 10–14 min (F06 runs two bundle exports bounded by ``stall_timeout_min: 1``, ≈ 3 min each while
V24 is open; the others one handover each).
"""

from __future__ import annotations

import json
import os
import time

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)


@pytest.mark.e2e_timeout(600)
@pytest.mark.parametrize("variant", ["repo", "session_only"])
def test_l3_f01_default_workspace_root_inside_hermes_home_fails_before_the_lock(lab, ev, variant):
    """L3-F01: with the default ``workspace_root``/``allow_roots`` (``~/hermes-handover``) and ``HOME`` inside
    ``HERMES_HOME`` (this lab; the official Docker image: ``HOME`` = ``HERMES_HOME`` = ``/opt/data``), receiving must
    not fail only after the source was locked. ``repo``: the session's repository is a workspace item;
    ``session_only``: the cwd lies in A's Hermes home (not sent, L3-F02), so only the commit step c2 creates the cwd
    under the default root — the case of the finding.

    Soll: the handover succeeds, or it is refused before anything is locked — README §Use ("Before anything is
    locked the plugin shows what would move"), README §Settings ("Every key has a working default"), R11 ("Ziel-cwd
    vor dem Import anlegen"), DESIGN §8.6 (the target decides destinations in ``negotiate``, before LOCKING) and
    §3.4 point 3 / I2."""
    if variant == "repo":
        src = lab.make_source("a", f"f01{variant}", turns=("default receive folder",))
        sid = src.sid
    else:
        cwd = lab.work("a") / f"f01{variant}{lab.run_id}"
        cwd.mkdir(parents=True, exist_ok=True)
        sid = lab.new_session("a", cwd)
        assert lab.turn("a", sid, "default receive folder, session only").ok
    with lab.preserved("b"):
        lab.setting("b", "workspace_root", "~/hermes-handover")
        lab.setting("b", "allow_roots", json.dumps(["~/hermes-handover"]))
        res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", timeout=600)
    ev.cli("take-default-root", res)
    job = lab.job_row("b", res.job_id) if res.job_id else None
    events = lab.events("b", res.job_id) if res.job_id else []
    source = lab.source_row("a", res.job_id) if res.job_id else None
    ev.write("job.json", {"job": job, "events": events[-40:], "source_a": source})
    if res.rc == 0 and job and job.get("state") == "done":
        return
    reached = {str(e.get("state")) for e in events}
    assert not reached & {"locked", "committing"}, f"refused only after the source was locked: {sorted(reached)}"
    assert not (source or {}).get("prepared_at"), source
    assert lab.marks("a", sid)["ended"] is False


@pytest.mark.e2e_timeout(600)
def test_l3_f02_workspace_inside_the_hermes_home_is_named_not_silently_dropped(lab, ev):
    """L3-F02: a session whose cwd lies inside the Hermes home (``lab/a/work/…``; on Docker every workspace does)
    must not leave silently with "0 items, 0 B".

    Soll: the user sees before the lock what stays behind (README §Use "Before anything is locked the plugin shows
    what would move"), and what was left out is named in report and note (R23, DESIGN §8.5/§11.3 "Not
    transferred")."""
    cwd = lab.work("a") / f"f02proj{lab.run_id}"
    lab.make_repo(cwd)
    sid = lab.new_session("a", cwd)
    assert lab.turn("a", sid, "cwd inside the hermes home").ok
    with lab.db("a", write=True) as db:
        db.set_session_title(sid, f"f02 {lab.run_id}")
    res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", timeout=600)
    ev.cli("take", res)
    assert res.rc == 0, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write("job-b.json", status)
    sid_b = str((status["job"].get("ack") or {}).get("session_id") or "")
    note = str((lab.arrival("b", sid_b) or {}).get("note") or "")
    ev.write("note.txt", note)
    assert "0 items, 0 B" not in res.stdout + res.stderr, "pre-send summary says nothing moves and names nothing"
    assert str(cwd) in res.stdout + res.stderr, "the pre-send/final text does not name the folder left behind"
    assert str(cwd) in note, "the note does not name the folder left behind"


@pytest.mark.e2e_timeout(600)
def test_l3_f03_gateway_final_message_names_target_session_and_model(lab, ev):
    """L3-F03: the final notice of a push started in the gateway (``/handover to <peer> --yes``) names the target
    session, its folder and the model.

    Soll: R14 ("Die Abschlussmeldung nennt Zielsession, Modell, Offline-Tauglichkeit und den nächsten Schritt"),
    README §Use ("The final message names the session on the other side, its folder, the model and whether it works
    offline"), DESIGN §11.6."""
    src = lab.make_source("a", "f03proj", turns=("final message",))
    t0 = time.time()
    gw = lab.gw("a")
    try:
        gw.rpc("session.resume", {"session_id": src.sid})
        reply = gw.rpc(
            "slash.exec", {"session_id": src.sid, "command": f"/handover to {_lab.LAPTOP_GW} --yes"}, timeout=300
        )
        jid = str(lab.wait_new_job("a", t0, timeout=60)["job_id"])
        deadline = time.monotonic() + 420
        while time.monotonic() < deadline:
            gw.linger(1.0)
            if (lab.job_row("a", jid) or {}).get("state") in _lab.STOP_STATES | {"suspended", "in_doubt"}:
                gw.linger(4.0)
                break
        events = list(gw.events)
    finally:
        gw.close()
    notices = [
        (e.get("payload") or {})
        for e in events
        if e.get("type") == "notification.show" and "plugin-message:ctx_handover" in json.dumps(e)
    ]
    status = lab.job_status("a", jid)
    ev.write("notices.json", {"reply": reply, "notices": notices[-10:], "status": status})
    ack = status["job"].get("ack") or {}
    assert status["job"].get("state") == "done", status
    final = json.dumps(notices[-1] if notices else {}, ensure_ascii=False)
    assert f"session {ack.get('session_id')}" in final, final
    assert f"model {ack.get('model')}" in final, final
    path, _mode = _lab.dest_of(status["job"])
    assert path and path in final, (path, final)


@pytest.mark.e2e_timeout(600)
def test_l3_f04_participant_side_status_finds_the_job(lab, ev):
    """L3-F04: after a push A→B, ``hermes handover status <job>`` on the receiving side B finds the job and shows the
    note; the overview without a job lists the arrival. After a pull, the source side finds the job too.

    Soll: README §Use ("On the receiving side ``/handover status <job>`` shows the note the agent gets"), DESIGN §11.6
    ("``/handover status <job>`` on the target also show[s] the full note text … After a push, ``/handover status`` on
    the target lists the arrival")."""
    src = lab.make_source("a", "f04proj", repo=False, turns=("status on both sides",))
    pushed = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--yes", timeout=600)
    ev.cli("push", pushed)
    assert pushed.rc == 0 and pushed.job_id, pushed.brief()
    on_b = lab.handover("b", "status", pushed.job_id, timeout=180)
    ev.cli("status-b-job", on_b)
    overview = lab.handover("b", "status", timeout=180)
    ev.cli("status-b-overview", overview)
    src2 = lab.make_source("a", "f04bproj", repo=False, turns=("pulled",))
    pulled = lab.handover("b", "take", _lab.SERVER, src2.sid, "--yes", timeout=600)
    ev.cli("pull", pulled)
    on_a = lab.handover("a", "status", pulled.job_id or "missing", timeout=180)
    ev.cli("status-a-job", on_a)
    assert on_b.rc == 0 and on_b.error_code is None, on_b.brief()
    assert _lab.NOTE_MARK in on_b.text or "Moved from" in on_b.text, on_b.text[-1500:]
    short = pushed.job_id[-6:]
    assert pushed.job_id in overview.text or short in overview.text.upper(), overview.text[-1500:]
    assert on_a.rc == 0 and on_a.error_code is None, on_a.brief()


@pytest.mark.e2e_timeout(300)
def test_l3_f05_name_conflict_is_refused_before_the_code_is_redeemed(lab, ev):
    """L3-F05: pasting a pairing string with ``--name`` equal to an existing peer is refused **before** the code is
    redeemed: the listener keeps no inbound peer the dialer does not know, and the code stays usable.

    Soll: DESIGN §12.3 (pairing is two-sided or not at all), README §Set up step 4."""
    hint = f"probe{_lab.uid(4)}"
    made = lab.handover("b", "pair", "--name", hint, timeout=180)
    ev.cli("pair-code-b", made)
    match = _lab.PAIR_RE.search(made.text)
    assert match, made.brief()
    clash = lab.handover("a", "pair", match.group(1), "--name", _lab.LAPTOP, timeout=180)
    ev.cli("redeem-a-clash", clash)
    peers_b = lab.ledger_rows("b", "SELECT name, direction, revoked_at FROM peers")
    ev.write("peers-b.json", peers_b)
    try:
        assert clash.rc != 0, clash.brief()
        orphan = [p for p in peers_b if p["name"] == hint]
        assert not orphan, f"B keeps an inbound peer A does not know: {orphan}"
    finally:
        if any(p["name"] == hint for p in peers_b):
            ev.cli("cleanup-revoke", lab.handover("b", "peers", "revoke", hint, timeout=120))


@pytest.mark.e2e_timeout(600)
def test_l3_f06_slash_export_writes_a_bundle(lab, ev):
    """L3-F06 (a): ``/handover export <path> --yes`` in A's gateway writes one ``.ctxh.tar`` and the job ends ``done``.

    Soll: BRIEF D5/D13, README §Use ("``/handover export`` writes one ``.ctxh.tar``"), L2-contracts V24. Known cause
    V24 (``reports/L2-red.md`` C); ``stall_timeout_min`` is set to 1 on A for the test (restored, gw-a restarted)."""
    src = lab.make_source("a", "f06aproj", turns=("export in the gateway",))
    folder = lab.handover_root("a") / f"f06a-{lab.run_id}"
    folder.mkdir(parents=True, exist_ok=True)
    bundle = folder / "f06.ctxh.tar"
    with lab.preserved("a", restart=("gw-a",)):
        lab.setting("a", "stall_timeout_min", "1")
        lab.restart("gw-a")
        t0 = time.time()
        out, _ = lab.slash("a", src.sid, f"/handover export {bundle} --yes", timeout=300)
        jid = str(lab.wait_new_job("a", t0, timeout=60)["job_id"])
        final = lab.wait_job("a", jid, _lab.STOP_STATES | {"in_doubt", "suspended"}, timeout=420, poll=1.0)
        status = lab.job_status("a", jid)
    ev.write("export.json", {"output": out, "final": final, "status": status, "folder": sorted(os.listdir(folder))})
    try:
        assert bundle.is_file(), sorted(p.name for p in folder.iterdir())
        assert final == "done", status["job"]
    finally:
        lab.handover("a", "reclaim", src.sid, "--force", timeout=180)


@pytest.mark.e2e_timeout(600)
def test_l3_f06_in_doubt_wait_is_bounded_by_stall_timeout(lab, ev):
    """L3-F06 (b): a job that cannot finish its commit stops waiting after ``stall_timeout_min`` (here 1 min), not after
    the commit retries plus one in-doubt period each (182 s observed on ``abcbd8a7``).

    Soll: DESIGN §3.2 (IN_DOUBT polls with backoff; §14.4 ``stall_timeout_min`` "Suspend a stalled handover after
    (minutes)"), L2-contracts V24. Reproduced with the CLI bundle export (the commit fails with V24's ``bundle
    commit before prepare``); bound asserted: 60 s + 45 s for planning, packing and polling."""
    src = lab.make_source("a", "f06bproj", turns=("bounded wait",))
    bundle = lab.handover_root("a") / f"f06b-{lab.run_id}.ctxh.tar"
    with lab.preserved("a"):
        lab.setting("a", "stall_timeout_min", "1")
        res = lab.handover("a", "export", str(bundle), "--session", src.sid, "--yes", timeout=480)
    ev.cli("export-cli", res)
    events = lab.events("a", res.job_id) if res.job_id else []
    ev.write("events.json", events)
    try:
        if res.rc == 0:
            return
        first_commit = next((e["ts"] for e in events if e.get("state") == "committing"), None)
        last = events[-1]["ts"] if events else None
        waited = (last - first_commit) if first_commit and last else None
        ev.write("waited.json", {"waited_s": waited, "elapsed_s": res.elapsed})
        assert waited is not None and waited <= 60 + 45, f"waited {waited} s after the commit started"
    finally:
        lab.handover("a", "reclaim", src.sid, "--force", timeout=180)


@pytest.mark.e2e_timeout(600)
def test_l3_f06_reclaim_accepts_the_session_option(lab, ev):
    """L3-F06 (c): ``hermes handover reclaim --session <id>`` is accepted (README §Use: "``--session <id>`` names the
    session" for every CLI verb; observed on ``abcbd8a7``: "--session is not an option of 'reclaim'")."""
    src = lab.make_source("a", "f06cproj", repo=False, turns=("reclaim with --session",))
    pulled = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
    ev.cli("pull", pulled)
    assert pulled.rc == 0, pulled.brief()
    rec = lab.handover("a", "reclaim", "--session", src.sid, timeout=180)
    ev.cli("reclaim-session-option", rec)
    if rec.rc != 0:
        ev.cli("cleanup-reclaim", lab.handover("a", "reclaim", src.sid, timeout=180))
    assert "is not an option" not in rec.text, rec.text
    assert rec.rc == 0, rec.brief()
    assert lab.marks("a", src.sid)["ended"] is False
