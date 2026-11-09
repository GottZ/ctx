"""Layer 3 — the open lab hypotheses L9, L11, L13–L27 of ``design/OPEN.md §1.2`` (TESTPLAN §6).

Each test records what the stand does (evidence directory) and asserts the design's assumption; a red test means
the design has to switch to the named fallback (OPEN.md §1.2, column "Ausweichpfad"). Hypotheses the lab cannot
reproduce are skipped with the exact reason and the test that covers them elsewhere.

Runtime estimate: 12–18 min (L15 and L23/L26 restart components; L26 runs two continuation turns).
"""

from __future__ import annotations

import json
import os
import shutil
import time
from pathlib import Path

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)


def _own_dir(lab, name: str):
    """A workspace folder of its own (a session whose cwd were ``lab/ws-e2e/a`` itself would carry every other
    scenario's workspace along)."""
    path = lab.ws("a") / f"{name}{lab.run_id}"
    path.mkdir(parents=True, exist_ok=True)
    return path


def _pulled(lab, ev, name: str, *, turns=("lab question",), repo: bool = False):
    src = lab.make_source("a", name, repo=repo, turns=turns)
    res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
    ev.cli("take", res)
    assert res.rc == 0 and res.job_id, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write("job-b.json", status)
    assert status["job"].get("state") == "done", status
    return src, res.job_id, str((status["job"].get("ack") or {}).get("session_id") or "")


def _asks(gw) -> list:
    """Server-to-client requests and question-like events a WebSocket client saw (``ask_choice`` reaching it)."""
    words = ("clarify", "choice", "question", "ask")
    return list(gw.requests) + [e for e in gw.events if any(w in str(e.get("type")) for w in words)]


def _listed(reply: dict) -> list[str]:
    result = reply.get("result") or {}
    items = result.get("sessions") or result.get("items") or (result if isinstance(result, list) else [])
    out = []
    for item in items:
        if isinstance(item, dict):
            out.append(str(item.get("id") or item.get("session_id") or (item.get("ref") or {}).get("session_id")))
    return out


@pytest.mark.e2e_timeout(420)
def test_lab_L09_hidden_import_then_unhide_behaves_like_a_normal_import(lab, ev):
    """L9: the target imports hidden and unhides as its last commit step; the result behaves like a normal import —
    listed by ``hermes sessions list`` and the gateway's ``session.list``, resumable over the WebSocket with the full
    display history, and a turn runs. Fallback if red: import only in the commit step."""
    _src, _jid, sid_b = _pulled(lab, ev, "l09")
    row = lab.row("b", sid_b)
    assert not row.get("hidden"), row
    cli = lab.hermes("b", "sessions", "list", timeout=180)
    ev.write("sessions-list.txt", cli.stdout + cli.stderr)
    listed = lab.gw_rpc("b", "session.list", {"limit": 100, "scope": "stored"})
    ev.write("session-list.json", listed)
    assert sid_b in cli.stdout or sid_b in _listed(listed), "the imported session is not listed"
    resumed = lab.gw_rpc("b", "session.resume", {"session_id": sid_b})
    assert not resumed.get("error"), resumed
    messages = (resumed.get("result") or {}).get("messages") or []
    assert len(messages) >= 2, resumed
    assert lab.turn("b", sid_b, "L09 does it continue?").ok


@pytest.mark.e2e_timeout(600)
def test_lab_L11_progress_edits_from_the_worker_thread(lab, ev):
    """L11: the ``post_message`` handle created in the slash handler is edited from the job's worker thread after the
    handler returned (same notice id, new text). Fallback if red: progress only through ``/handover status``."""
    src = lab.make_source("a", "l11", repo=False, turns=("progress",))
    scratch = lab.new_session("b", lab.work("b"))
    t0 = time.time()
    gw = lab.gw("b")
    try:
        gw.rpc("session.resume", {"session_id": scratch})
        reply = gw.rpc(
            "slash.exec",
            {"session_id": scratch, "command": f"/handover take {_lab.SERVER} {src.sid} --yes"},
            timeout=300,
        )
        at_reply = len(gw.events)
        jid = str(lab.wait_new_job("b", t0, timeout=60)["job_id"])
        deadline = time.monotonic() + 420
        while time.monotonic() < deadline:
            gw.linger(1.0)
            if (lab.job_row("b", jid) or {}).get("state") in _lab.STOP_STATES | {"suspended", "in_doubt"}:
                gw.linger(4.0)
                break
        events = list(gw.events)
    finally:
        gw.close()

    def notice(e: dict) -> bool:
        return e.get("type") == "notification.show" and "plugin-message:ctx_handover" in json.dumps(e)

    before = {str((e.get("payload") or {}).get("id")) for e in events[:at_reply] if notice(e)}
    after = [e for e in events[at_reply:] if notice(e)]
    ev.write("notices.json", {"reply": reply, "at_reply": at_reply, "before_ids": sorted(before), "after": after[-30:]})
    assert before, "the handler created no progress notice"
    edits = [e for e in after if str((e.get("payload") or {}).get("id")) in before]
    texts = {json.dumps(e.get("payload"), sort_keys=True) for e in edits}
    assert len(texts) >= 1 and edits, "no edit of the handler's notice arrived after the handler returned"


@pytest.mark.e2e_timeout(600)
def test_lab_L13_git_lfs_checkout_offline(lab, ev):
    """L13: ``git lfs checkout`` works without network from objects the plugin placed locally. Fallback if red: do not
    call it; pointers stay and the note names ``git lfs pull``."""
    if shutil.which("git-lfs") is None:
        pytest.skip(
            "git-lfs is not installed on this host (`git lfs version`: 'lfs' is not a git command), so neither side "
            "of the lab can smudge; LFS object transfer and oid checks are covered by "
            "tests/unit/E/test_gitapply.py::test_apply_lfs_objects_oid_verified_no_network"
        )
    import hashlib

    content = os.urandom(4096)
    oid = hashlib.sha256(content).hexdigest()
    cwd = lab.ws("a") / "l13lfs"
    rb = lab.make_repo(
        cwd, dirty=False, ignored=False, files={".gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n"}
    )
    pointer = f"version https://git-lfs.github.com/spec/v1\noid sha256:{oid}\nsize {len(content)}\n"
    rb.write("asset.bin", pointer)
    obj = cwd / ".git" / "lfs" / "objects" / oid[:2] / oid[2:4] / oid
    obj.parent.mkdir(parents=True, exist_ok=True)
    obj.write_bytes(content)
    rb.commit_all("lfs pointer")
    sid = lab.new_session("a", cwd)
    assert lab.turn("a", sid, "lfs").ok
    res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", timeout=600)
    ev.cli("take", res)
    assert res.rc == 0, res.brief()
    status = lab.job_status("b", res.job_id)
    sid_b = str((status["job"].get("ack") or {}).get("session_id") or "")
    dest = lab.row("b", sid_b).get("cwd")
    data = (Path(str(dest)) / "asset.bin").read_bytes() if dest else b""
    note = str((lab.arrival("b", sid_b) or {}).get("note") or "")
    assert data == content or "git lfs pull" in note, (data[:80], note[-500:])


@pytest.mark.e2e_timeout(420)
def test_lab_L14_paused_heartbeat_and_loop_resume_on_the_target(lab, ev):
    """L14: an active heartbeat and loop of the source arrive paused under the new id (``loop`` without its route)
    and ``/heartbeat resume`` / ``/loop resume`` on the target make them active. Fallback if red: transfer only the
    goal, list heartbeat/loop as not transferred.

    The two states are written with the public ``SessionDB.set_meta`` in the JSON shape of ``HeartbeatState`` and
    ``LoopState`` (hourly, next run an hour away, so the source gateway does not fire them during the test)."""
    src = lab.make_source("a", "l14", repo=False, turns=("schedules",))
    now = time.time()
    heartbeat = {"prompt": "check the build", "interval_seconds": 3600, "status": "active", "created_at": now}
    loop = {
        "prompt": "poll the queue",
        "status": "active",
        "mode": "interval",
        "interval_seconds": 3600.0,
        "created_at": now,
        "next_due_at": now + 3600,
        "route": {"platform": "local", "chat_id": src.sid},
    }
    with lab.db("a", write=True) as db:
        db.set_meta(f"heartbeat:{src.sid}", json.dumps(heartbeat))
        db.set_meta(f"loop:{src.sid}", json.dumps(loop))
    res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
    ev.cli("take", res)
    assert res.rc == 0, res.brief()
    sid_b = str((lab.job_status("b", res.job_id)["job"].get("ack") or {}).get("session_id") or "")
    hb_b = json.loads(lab.meta("b", f"heartbeat:{sid_b}") or "{}")
    loop_b = json.loads(lab.meta("b", f"loop:{sid_b}") or "{}")
    ev.write("arrived.json", {"heartbeat": hb_b, "loop": loop_b})
    assert hb_b.get("status") == "paused" and loop_b.get("status") == "paused", (hb_b, loop_b)
    assert not loop_b.get("route"), loop_b
    out_hb, _ = lab.slash("b", sid_b, "/heartbeat resume")
    out_loop, _ = lab.slash("b", sid_b, "/loop resume")
    hb_after = json.loads(lab.meta("b", f"heartbeat:{sid_b}") or "{}")
    loop_after = json.loads(lab.meta("b", f"loop:{sid_b}") or "{}")
    ev.write("resumed.json", {"out_hb": out_hb, "out_loop": out_loop, "heartbeat": hb_after, "loop": loop_after})
    assert hb_after.get("status") == "active" and loop_after.get("status") == "active", (out_hb, out_loop)
    with lab.db("b", write=True) as db:
        for key in ("heartbeat", "loop"):
            state = json.loads(db.get_meta(f"{key}:{sid_b}") or "{}")
            db.set_meta(f"{key}:{sid_b}", json.dumps({**state, "status": "cleared"}))


@pytest.mark.e2e_timeout(900)
def test_lab_L15_host_isolation_on_the_listener(lab, ev):
    """L15: with ``plugins.isolation: host`` on A (the listener and source), the routes work (a pull from B
    succeeds, every request < 60 s), the slash command answers through the plugin host, the ``pre_llm_call`` veto
    hook blocks a turn on the retired source, and ``ask_choice`` reaches the client (``/handover setup`` asks).
    Fallback if red: README recommends ``in_process``; ``doctor`` reports it."""
    with lab.preserved("a", restart=("gw-a", "serve-a")):
        lab.config_set("a", "plugins.isolation", "host")
        lab.restart("gw-a", "serve-a")
        src = lab.make_source("a", "l15", repo=False, turns=("hosted plugin",))
        t0 = time.monotonic()
        res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=900)
        took = time.monotonic() - t0
        ev.cli("take", res)
        status_out, _ = lab.slash("a", lab.new_session("a", lab.work("a")), "/handover status", timeout=180)
        ev.write("status-a.txt", status_out)
        probe = f"L15-VETO-{_lab.uid()}"
        mark = lab.llm_mark()
        veto = lab.turn("a", src.sid, probe, timeout=120)
        reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
        ev.write("veto.json", {"turn": veto.brief(), "reached": len(reached)})
        gw = lab.gw("a")
        scratch = lab.new_session("a", lab.work("a"))
        try:
            gw.rpc("session.resume", {"session_id": scratch})
            rid = gw.rid + 1
            gw.ws.send(
                json.dumps(
                    {
                        "jsonrpc": "2.0",
                        "id": rid,
                        "method": "slash.exec",
                        "params": {"session_id": scratch, "command": "/handover setup"},
                    }
                )
            )
            gw.rid = rid
            asked = _lab.wait_for(lambda: gw.linger(1.0) or _asks(gw), 30, poll=0.0)
        finally:
            gw.close()
        ev.write("ask-choice.json", {"requests": gw.requests[-5:], "events": [e.get("type") for e in gw.events]})
    assert res.rc == 0, res.brief()
    assert took < 600, took
    assert "status" in status_out.lower() or "handover" in status_out.lower(), status_out
    assert not reached and "handed over" in veto.text, veto.brief()
    assert asked, "no ask_choice request reached the WebSocket client"


@pytest.mark.e2e_timeout(420)
def test_lab_L16_opening_the_archived_source_from_desktop_runs_into_the_veto(lab, ev):
    """L16: the Desktop path (WebSocket ``session.resume`` + ``prompt.submit``) on the archived, hidden source after a
    handover answers with the veto text and sends nothing to the model."""
    src, _jid, _sid_b = _pulled(lab, ev, "l16")
    probe = f"L16-VETO-{_lab.uid()}"
    mark = lab.llm_mark()
    turn = lab.turn("a", src.sid, probe)
    ev.write("turn.json", turn.brief())
    reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
    assert not reached, "the vetoed turn reached the model"
    assert "handed over to" in turn.text, turn.brief()


@pytest.mark.e2e_timeout(420)
def test_lab_L17_sidebar_lists_the_activated_session(lab, ev):
    """L17: the Desktop sidebar listing (gateway ``session.list``) on B shows the activated session; on A the
    handed-over source is no longer listed among the active sessions (archived + hidden). Fallback: title/hidden."""
    src, _jid, sid_b = _pulled(lab, ev, "l17")
    listed_b = lab.gw_rpc("b", "session.list", {"limit": 100})
    listed_a = lab.gw_rpc("a", "session.list", {"limit": 100})
    ev.write("lists.json", {"b": listed_b, "a": listed_a})
    assert sid_b in _listed(listed_b), _listed(listed_b)[:20]
    assert src.sid not in _listed(listed_a), "the handed-over source is still listed on A"


@pytest.mark.e2e_timeout(420)
def test_lab_L18_row_without_route_adopts_without_credential_error(lab, ev):
    """L18: a session whose provider is the target's main provider arrives without a route (the target writes none)
    and is adopted by B's gateway without ``launch_credentials_unavailable``; the first turn reaches the model.

    Not covered here: the ``gateway_runtime={"provider": <named entry>}`` branch — the lab homes configure no named
    provider entries (``labctl.config_yaml``); the foreign-provider fallback is E13."""
    from ctx_handover.tests.fakes.sessiondb import SessionBuilder

    with lab.db("a", write=True) as db:
        b = SessionBuilder(db)
        sid = b.create(
            title=f"l18 main provider {lab.run_id}",
            model="fake-model",
            provider="custom",
            cwd=str(_own_dir(lab, "l18")),
        )
        b.turns(sid, 2)
    res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", timeout=600)
    ev.cli("take", res)
    assert res.rc == 0, res.brief()
    sid_b = str((lab.job_status("b", res.job_id)["job"].get("ack") or {}).get("session_id") or "")
    row = lab.row("b", sid_b)
    ev.write("row-b.json", row)
    mark = lab.llm_mark()
    turn = lab.turn("b", sid_b, f"L18 adopt {_lab.uid()}")
    ev.write("turn.json", turn.brief())
    blob = json.dumps(turn.brief(), default=str)
    assert "launch_credentials_unavailable" not in blob, blob[-1500:]
    assert turn.ok and any(r.get("kind") == "main" for r in lab.llm_since(mark)), turn.brief()


@pytest.mark.e2e_timeout(600)
def test_lab_L19_block_size_passes_the_body_limit(lab, ev):
    """L19: requests of one 512-KiB block (and the default 8-MiB batches) pass the request-body limit in front of the
    peer. The lab has no proxy, so this records what ``doctor --measure`` finds for the direct path and asserts it
    is at least one block. A proxy with a lower limit is not part of the lab (fallback: one block per request,
    ``doctor`` names the proxy setting)."""
    res = lab.handover("b", "doctor", "--measure", timeout=600)
    ev.cli("doctor-measure", res)
    checks = res.data.get("checks") or []
    ev.write("checks.json", checks)
    peer = [c for c in checks if str(c.get("name")).split()[:2] == ["peer", _lab.SERVER]]
    assert peer and all(c.get("ok") is not False for c in peer), peer
    assert any("MiB" in str(c.get("detail")) for c in peer), peer


def test_lab_L20_one_million_token_export_through_b_prime():
    """L20: path B′ keeps H_all for a real 1M-token export."""
    pytest.skip(
        "no real 1M-token Hermes export exists in the lab (the fake model produces only short turns); the synthetic "
        "shape is E6 (tests/e2e/test_e2e_session.py::test_e06_session_over_the_import_limits) and "
        "tests/integration/test_int_large_session.py::test_int_one_million_token_in_place_session"
    )


@pytest.mark.e2e_timeout(420)
def test_lab_L21_retire_from_serve_while_the_desktop_view_is_open(lab, ev):
    """L21 (SK M6): the retire runs in A's ``hermes serve`` (pull from B) while a Desktop view of the source is open
    on A's gateway; a message sent from that open view is vetoed, and the session is archived after a reload."""
    src = lab.make_source("a", "l21", repo=False, turns=("open view",))
    gw = lab.gw("a")
    try:
        gw.rpc("session.resume", {"session_id": src.sid})
        res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
        ev.cli("take", res)
        probe = f"L21-VETO-{_lab.uid()}"
        mark = lab.llm_mark()
        gw.rpc("prompt.submit", {"session_id": src.sid, "text": probe, "input_id": f"e2e-{_lab.uid(12)}"})
        done = gw.wait(lambda e: e.get("type") in {"message.complete", "error"}, 120)
    finally:
        gw.close()
    ev.write("open-view.json", {"done": done, "events": [e.get("type") for e in gw.events][-30:]})
    assert res.rc == 0, res.brief()
    reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
    assert not reached, "the open view's message reached the model"
    # either veto text of DESIGN §7.6: the gateway's veto index follows the ledger within its 2-s refresh, so the
    # open view may still see ``prepared`` ("is being completed") right after the retire in serve
    said = json.dumps(done or {})
    assert "handed over to" in said or "is being completed" in said, done
    marks = lab.marks("a", src.sid)
    assert marks["archived"] and marks["hidden"], marks


@pytest.mark.e2e_timeout(300)
def test_lab_L22_handover_in_the_desktop_command_catalog(lab, ev):
    """L22: ``/handover`` is in the gateway's command catalog (what Desktop offers) with ``argument_mode="text"``.
    Fallback if red: register without ``argument_mode``."""
    scratch = lab.new_session("b", lab.work("b"))
    reply = lab.gw_rpc("b", "commands.catalog", {"session_id": scratch}, resume=scratch)
    ev.write("catalog.json", reply)
    result = reply.get("result") or {}
    commands = result.get("commands") if isinstance(result.get("commands"), dict) else {}
    entry = commands.get("/handover")
    assert isinstance(entry, dict), sorted(commands)[:40]
    assert "/handover" in json.dumps(result.get("pairs") or []), "not offered in the command list (pairs)"
    assert entry.get("argument_mode") == "text", entry


@pytest.mark.e2e_timeout(420)
def test_lab_L23_manual_compress_under_a_foreign_compression_lock_is_refused(lab, ev):
    """L23: while another holder owns a session's compression lock (as the plugin does in ``locked``/``prepared``),
    a manual ``/compress`` is refused and the lineage and the rows stay as they were. Fallback if red:
    ``stale_session`` restart counter."""
    src = lab.make_source("a", "l23", repo=False, turns=("one", "two", "three", "four"))
    before = lab.hashes("a", src.sid)
    holder = f"pid={os.getpid()}:ctx_handover-e2e:l23"
    with lab.db("a", write=True) as db:
        got = db.try_acquire_compression_lock(src.sid, holder, ttl_seconds=300)
    assert got, "scaffold: could not take the compression lock"
    try:
        out = lab.compress("a", src.sid)
        ev.write("compress.json", out)
        after = lab.hashes("a", src.sid)
        tip = lab.tip("a", src.sid)
    finally:
        with lab.db("a", write=True) as db:
            db.release_compression_lock(src.sid, holder)
    ev.write("hashes.json", {"before": before, "after": after, "tip": tip})
    assert out["reply"].get("error"), f"the compaction was not refused: {out['reply']}"
    assert tip == src.sid
    assert after["h_all"] == before["h_all"] and after["rows"] == before["rows"], (before, after)


@pytest.mark.e2e_timeout(420)
def test_lab_L24_context_larger_than_the_target_model_suggests_compress(lab, ev):
    """L24: when the session is larger than the target model's context window, the confirmation says so and
    suggests ``/compress`` on the source; without an answer (CLI without a terminal, no ``--yes``) the job is
    cancelled with the plan printed and nothing locked. Whether a 1M-token history on a 64k model compacts offline
    sensibly needs a real model and is not part of the lab."""
    from ctx_handover.tests.fakes.sessiondb import SessionBuilder

    with lab.db("a", write=True) as db:
        b = SessionBuilder(db)
        sid = b.create(title=f"l24 big context {lab.run_id}", model="fake-model", cwd=str(_own_dir(lab, "l24")))
        b.turns(sid, 20, size=4000)
    with lab.preserved("b"):
        lab.config_set("b", "model.context_length", "4000")
        res = lab.handover("b", "take", _lab.SERVER, sid, timeout=600)
    ev.cli("take-without-yes", res)
    assert "/compress" in res.text, res.text[-2000:]
    assert lab.marks("a", sid)["ended"] is False
    assert res.rc != 0 or "Nothing was locked" in res.text, res.brief()


def test_lab_L25_continuation_with_profile_flag_in_multiplex():
    """L25: the continuation subprocess with ``-p <profile>`` reaches the right profile under multiplexing."""
    pytest.skip(
        "the lab runs one default profile per HERMES_HOME (no multiplexed profiles), so `-p` is never passed; the "
        "argv rule is covered by tests/unit/F/test_continuation.py::test_continue_spawn_argv_and_profile"
    )


@pytest.mark.e2e_timeout(900)
def test_lab_L26_dangerous_command_in_the_continuation_turn_is_denied(lab, ev):
    """L26 (SEC 4.4): a continuation turn (``--continue`` push, ``allow_remote_continue`` on A,
    ``approvals.mode: manual`` so no guardian model approves, ``single_query_mode`` default ``deny``) in which the
    model asks for ``rm -rf <canary>`` does not delete the canary — with A's gateway running and with it stopped.
    Fallback if red: continue only while the gateway runs, else ``continue_refused``."""
    results = {}
    pids: list[int] = []
    with lab.preserved("a", restart=("serve-a", "gw-a")):
        lab.setting("a", "allow_remote_continue", "true")
        lab.config_set("a", "approvals.mode", "manual")
        lab.restart("serve-a", "gw-a")
        try:
            for variant in ("gateway", "no-gateway"):
                if variant == "no-gateway":
                    lab.stop("gw-a")
                canary = lab.ws("a") / f"l26-canary-{variant}-{_lab.uid()}"
                canary.mkdir(parents=True)
                (canary / "keep.txt").write_text("must survive\n", encoding="utf-8")
                prompt = f"L26-{variant}-{_lab.uid()}"
                rule = {
                    "id": prompt,
                    "when": {"kind": "main", "last_user_contains": prompt},
                    "reply": {"tool_calls": [{"name": "terminal", "arguments": {"command": f"rm -rf {canary}"}}]},
                }
                src = lab.make_source("b", f"l26{variant.replace('-', '')}", repo=False, turns=("dangerous",))
                mark = lab.llm_mark()
                with lab.llm_rules([rule]):
                    res = lab.handover(
                        "b", "to", _lab.SERVER, "--session", src.sid, "--continue", prompt, "--yes", timeout=600
                    )
                    status = lab.job_status("b", res.job_id) if res.job_id else {"job": {}}
                    cont = status["job"].get("continuation") or {}
                    if cont.get("pid"):
                        pids.append(int(cont["pid"]))
                    _lab.wait_for(lambda cont=cont: not _lab.pid_alive(cont.get("pid")), 240, poll=1.0)
                reached = [r for r in lab.llm_since(mark) if prompt in _lab.last_user_message(r)]
                results[variant] = {
                    "rc": res.rc,
                    "cont": cont,
                    "reached": len(reached),
                    "canary": canary.exists(),
                    "text": res.text[-1500:],
                }
        finally:
            for pid in pids:
                _lab.kill_pid(pid)
            if not lab.running("gw-a"):
                lab.start("gw-a")
    ev.write("results.json", results)
    for variant, result in results.items():
        assert result["rc"] == 0, (variant, result)
        assert result["canary"], f"{variant}: the dangerous command ran in the continuation turn"
        assert result["reached"] >= 1 or "continue_refused" in json.dumps(result["cont"]), (variant, result)


def test_lab_L27_env_value_of_the_scoped_profile_under_multiplex():
    """L27 (SEC 11): ``get_env_value_prefer_dotenv`` reads the scoped profile's ``.env`` under multiplexing."""
    pytest.skip(
        "the lab runs no multiplexed profiles (one default profile per HERMES_HOME); the identity read-back across "
        "processes of one profile is covered by "
        "tests/integration/test_int_serve.py::test_int_identity_visible_to_serve_process"
    )
