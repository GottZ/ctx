"""Layer 3, phase 3 — acceptance on the real two-gateway stand (BUILD-ORDER §5, ``reports/L3-findings.md``).

Not a regression list: each test re-checks one decision of ``design/DECISIONS.md`` or one lab finding end to end on
the paired stand of ``conftest.lab`` (plugin copied byte-equal from this worktree, no lab patch). The ``Soll`` source
of every assertion is named in the test's docstring.

1. ``test_acc1_bundle_roundtrip_without_lab_patch[cli|gateway]`` — BRIEF D5/D13, L3-F06/V24, R1, R14;
   ``test_acc1_bundle_export_keep_leaves_the_source_active`` — O3.
2. ``test_acc2_default_workspace_root_and_return_from_it`` — R29, R29a (R4 for the way back).
3. ``test_acc3_cancel_ends_a_suspended_job`` — L3-F07, DESIGN §3.8/§3.2.
4. ``test_acc4_resume_after_a_target_restart_in_preparing`` — L3-F08, DESIGN §3.6, TESTPLAN E5b.
5. ``test_acc5_gateway_offer_is_redeemed_by_take_offer`` — L3-F12, O6.
6. ``test_acc6_refinement_lands_in_the_plan[cli|gateway]`` — L3-F09, R34, D9, R18 / hardening 18.
7. ``test_acc7_continue_refused_on_the_target[not_allowed|approvals_mode_off]`` — R10, R22. The allowed case
   (exactly one turn, note, adoption) and approvals off as ``approvals.single_query_mode: approve`` are E19
   (``test_e2e_flows.py::test_e19_push_and_continue``), named by ``test_acc7_continue_allowed_is_e19``.
8. ``test_acc8_return_in_place_is_e04`` — R4 is E4 (``test_e2e_roundtrip.py::test_e04_…``: in place, ignored
   ``build/`` survives, no ``-handover-`` folder); ``test_acc8_return_to_a_changed_origin_keeps_it`` adds the other
   branch of R4's check ("unchanged since the handover"), which E4 does not reach.

Settings a test changes are restored byte-exact (``Lab.preserved``); components restart only through ``labctl.py``
(``Lab.restart``/``start``/``stop``); a process a test starts is ended by PID in ``finally``. Evidence:
``lab/evidence/e2e-<CTXH_E2E_EVIDENCE>/<test>/`` (``conftest.ev``).

Runtime estimate: 30–40 min (acc3/acc4 move 120 MiB and wait for a suspension; acc2/acc8 run three handovers).
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import time
from pathlib import Path

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)

MiB = 1024 * 1024
BIG_FILES = 3
BIG_FILE_MIB = 40
TRANSFER_STATES = frozenset({"exporting", "context_staged", "packing", "data_staged"})
HASH_KEYS = ("h_all", "h_rows", "h_model", "display_norm")
REPO_KEYS = ("head", "branch", "status", "index", "refs", "stash", "tree")
DEFAULT_ROOT = "~/hermes-handover"
"""Schema default of ``workspace_root`` and ``allow_roots`` (``plugin.yaml`` ``config_schema``)."""
RESERVED = "handover-workspaces"
"""R29: the reserved ``<HERMES_HOME>/handover-workspaces`` (``engine/paths.py`` ``HERMES_WORKSPACES_DIR``)."""
ACTIVE_SOURCE_STATES = frozenset({"locking", "locked", "preparing", "prepared", "retiring", "retired", "exported"})
"""Source states that hold a lineage (``engine/ledger.py`` ``ACTIVE_SOURCE_STATES``)."""
PAST_PREPARE = frozenset(
    {
        "target_prepared",
        "source_preparing",
        "source_prepared",
        "committing",
        "in_doubt",
        "committed",
        "retiring",
        "done",
        "aborting",
        "aborted",
        "suspended",
    }
)
"""Conductor states after the target's PREPARE (a partial directory found then is no longer F08's window)."""


# ---------------------------------------------------------------------------------------------------------
# Helpers (this file only; ``_lab``/``conftest`` belong to the lab runner)
# ---------------------------------------------------------------------------------------------------------


def _ack(status: dict) -> dict:
    ack = status.get("job", {}).get("ack")
    return ack if isinstance(ack, dict) else {}


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


def _notices(events: list[dict]) -> list[dict]:
    return [
        e.get("payload") or {}
        for e in events
        if e.get("type") == "notification.show" and "plugin-message:ctx_handover" in json.dumps(e, default=str)
    ]


def _slash_job(lab, inst: str, sid: str, command: str, mode: str, *, timeout: float = 600.0) -> dict:
    """``slash.exec`` of a data-moving ``/handover`` verb in ``sid``; the WebSocket stays open until the new conductor
    job of ``mode`` stops (done, aborted, suspended, in_doubt), so the progress notices are collected."""
    t0 = time.time()
    gw = lab.gw(inst)
    jid = state = None
    try:
        gw.rpc("session.resume", {"session_id": sid})
        reply = gw.rpc("slash.exec", {"session_id": sid, "command": command}, timeout=300)
        output = str((reply.get("result") or {}).get("output") or reply.get("error") or "")
        row = _conductor_job(lab, inst, t0, mode)
        if row is not None:
            jid = str(row["job_id"])
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                gw.linger(1.0)
                state = (lab.job_row(inst, jid) or {}).get("state")
                if state in _lab.STOP_STATES | {"suspended", "in_doubt"}:
                    gw.linger(4.0)
                    break
        notices = _notices(gw.events)
    finally:
        gw.close()
    return {"command": command, "output": output, "job_id": jid, "state": state, "notices": notices[-10:]}


def _arrived(lab, ev, tag: str, inst: str, sid: str, hashes_src: dict, repo_src: dict | None, *, root=None) -> Path:
    """The arrived session equals the source: H_all/H_rows/H_model and the display history (DESIGN §6.9, D7), the
    repository (HEAD, branch, status with object ids, index, refs, stash, work tree without ignored ``build/``;
    BRIEF D10); it is active and its cwd lies under ``root``. Returns that cwd."""
    hashes = lab.hashes(inst, sid)
    row = lab.row(inst, sid)
    ev.write(f"{tag}-arrived.json", {"hashes": hashes, "row": row})
    for key in HASH_KEYS:
        assert hashes[key] == hashes_src[key], (tag, key)
    assert row and not row.get("hidden") and row.get("ended_at") is None, row
    dest = Path(str(row.get("cwd") or ""))
    assert dest.is_dir(), row.get("cwd")
    if root is not None:
        assert dest.resolve().is_relative_to(Path(root).resolve()), (str(dest), str(root))
    if repo_src is not None:
        repo = _lab.repo_state(dest)
        ev.write(f"{tag}-repo.json", repo)
        for key in REPO_KEYS:
            assert repo[key] == repo_src[key], (tag, key)
    return dest


def _first_turn_has_note(lab, ev, tag: str, inst: str, sid: str, dest: Path, old: Path) -> None:
    """The first turn there carries the note (D11) and a system prompt built for the new cwd (D7, R11)."""
    probe = f"{tag}-FIRST-{_lab.uid()}"
    mark = lab.llm_mark()
    turn = lab.turn(inst, sid, probe)
    ev.write(f"{tag}-first-turn.json", turn.brief())
    assert turn.ok, turn.brief()
    recs = [r for r in lab.llm_since(mark) if r.get("kind") == "main" and probe in _lab.last_user_message(r)]
    ev.write(f"{tag}-first-turn-llm.json", _lab.compact_records(recs))
    assert recs, "the first turn reached no model"
    user, system = _lab.last_user_message(recs[0]), _lab.system_prompt(recs[0])
    assert _lab.NOTE_MARK in user, user[-2000:]
    assert str(dest) in system and str(old) not in system, system[:1500]


def _vetoed(lab, ev, tag: str, inst: str, sid: str) -> None:
    """The source is locked (R1, DESIGN §7.4): every segment ended ``handed_over``, archived, hidden; a turn there
    gets the veto text and reaches no model."""
    marks = {s: lab.marks(inst, s) for s in lab.lineage(inst, sid)}
    ev.write(f"{tag}-source-marks.json", marks)
    for seg, m in marks.items():
        assert m["ended"] and m["end_reason"] == "handed_over", (seg, m)
        assert m["archived"] and m["hidden"], (seg, m)
    probe = f"{tag}-VETO-{_lab.uid()}"
    mark = lab.llm_mark()
    turn = lab.turn(inst, sid, probe)
    reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
    ev.write(f"{tag}-veto.json", {"turn": turn.brief(), "reached": len(reached)})
    assert not reached, "a turn on the locked source reached the model"
    assert "/handover reclaim" in turn.text, turn.brief()


def _big_source(lab, name: str) -> _lab.Source:
    """A session on A whose repository carries 120 MiB of untracked random data (a transfer of several seconds)."""
    src = lab.make_source("a", name, turns=("a long transfer",))
    for i in range(BIG_FILES):
        src.rb.write(f"data/blob{i}.bin", os.urandom(BIG_FILE_MIB * MiB))
    return src


def _push_bg(lab, ev, sid: str, tag: str) -> tuple[subprocess.Popen, str]:
    """``hermes handover to laptopgw --session <sid> --yes`` from A's CLI in the background; retried when the plugin
    was skipped in that process (version-probe flake, PATH.md "Stolpersteine")."""
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


def _settle(lab, ev, jid: str | None) -> None:
    """Clean-up, not under test: a job this test left unfinished is cancelled and resumed through A's gateway, so it
    does not hold a participant slot of B for the following tests. Waits ≤ 120 s."""
    if not jid or (lab.job_row("a", jid) or {}).get("state") in _lab.STOP_STATES:
        return
    ev.cli("settle-cancel", lab.handover("a", "cancel", jid, timeout=180))
    scratch = lab.new_session("a", lab.work("a"))
    out, _ = lab.slash("a", scratch, f"/handover resume {jid}", timeout=180)
    ev.write("settle-resume.txt", out)
    _lab.wait_for(lambda: (lab.job_row("a", jid) or {}).get("state") in _lab.STOP_STATES, 120, poll=1.0)
    ev.write("settle-state.json", {"a": lab.job_row("a", jid), "b": lab.job_row("b", jid, "target")})


def _short_timeouts(lab) -> None:
    lab.setting("a", "lease_ttl_s", "30")
    lab.setting("a", "stall_timeout_min", "1")


def _default_roots(lab, inst: str) -> None:
    lab.setting(inst, "workspace_root", DEFAULT_ROOT)
    lab.setting(inst, "allow_roots", json.dumps([DEFAULT_ROOT]))


def _peer_id(lab, inst: str, name: str) -> str:
    rows = lab.ledger_rows(inst, "SELECT peer_id FROM peers WHERE name = ?", (name,))
    if not rows:
        raise _lab.LabError(f"no peer {name} on {inst}")
    return str(rows[0]["peer_id"])


def _active_participants(lab, inst: str, peer_id: str, *, exclude: str | None) -> list[dict]:
    """Jobs the participant cap of ``inst`` counts for ``peer_id`` (``engine/participant.py`` ``_check_caps``: role
    not ``conductor``, state not terminal for the role, ``params.local`` not true), without ``exclude``."""
    from ctx_handover.engine.ledger import TERMINAL
    from ctx_handover.engine.model import Role

    out = []
    for row in lab.ledger_rows(inst, "SELECT job_id, role, mode, state, peer_id, params FROM jobs"):
        if row["role"] == "conductor" or row["job_id"] == exclude or row["peer_id"] != peer_id:
            continue
        if row["state"] in TERMINAL[Role(row["role"])]:
            continue
        try:
            params = json.loads(row["params"] or "{}")
        except ValueError:
            params = {}
        if isinstance(params, dict) and params.get("local") is True:
            continue
        out.append({k: row[k] for k in ("job_id", "role", "mode", "state")})
    return out


def _dry_run(lab, surface: str, sid: str) -> tuple[str, dict]:
    """A push dry run of ``sid`` from A: the plan is shown, nothing is locked (``--dry-run``, DESIGN §14.1)."""
    if surface == "cli":
        res = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", sid, "--dry-run", "--yes", timeout=300)
        return res.text, {"rc": res.rc, "data": res.data, "stderr": res.stderr[-3000:]}
    out, events = lab.slash("a", sid, f"/handover to {_lab.LAPTOP_GW} --dry-run --yes", timeout=300)
    return out, {"events": [e.get("type") for e in events][-30:]}


def _refine_requests(records: list[dict], rid: str) -> list[dict]:
    """Fake-model requests of the refinement: the rule fired, or an aux request with a response format that asks
    for ``notes_for_target`` (``engine/refine.py`` ``_RULES``)."""
    out = []
    for rec in records:
        body = rec.get("body") or {}
        asks = rec.get("kind") == "aux" and "notes_for_target" in json.dumps(body.get("messages") or [])
        if rec.get("rule") == rid or asks:
            out.append(rec)
    return out


# ---------------------------------------------------------------------------------------------------------
# 1. Bundle export and import without the lab patch
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(900)
@pytest.mark.parametrize("surface", ["cli", "gateway"])
def test_acc1_bundle_roundtrip_without_lab_patch(lab, ev, surface):
    """Bundle export on A and import on B, both through the CLI or both through the gateway slash command, on the
    production code (no ``L3-P01``).

    Soll: the export ends ``done`` and leaves exactly one ``.ctxh.tar`` (no ``.partial``) — BRIEF D5/D13, README §Use
    ("``/handover export`` writes one ``.ctxh.tar``"), L3-F06/V24; the source is locked (ended ``handed_over``,
    archived, hidden; a turn gets the veto and reaches no model) — O3 ("export sperrt die Quelle standardmäßig"), R1;
    the import ends ``done`` with origin "verified" (SEC 4.6) and its final message names the session (R14, L3-F03
    for the gateway import); transcript hashes, repository state, note on the first turn and a system prompt for B's
    folder as after a push (D7, D10, D11, R11)."""
    tag = f"acc1{surface}"
    src = lab.make_source("a", f"{tag}{lab.run_id}", turns=("bundle me",))
    hashes_a = lab.hashes("a", src.sid)
    repo_a = _lab.repo_state(src.repo)
    ev.write("source.json", {"sid": src.sid, "title": src.title, "hashes": hashes_a, "repo": repo_a})
    folder = lab.handover_root("a") / f"{tag}-{lab.run_id}"
    folder.mkdir(parents=True, exist_ok=True)
    bundle = folder / f"{tag}.ctxh.tar"
    t0 = time.monotonic()
    with lab.preserved("a"):
        lab.setting("a", "stall_timeout_min", "1")  # bounds the wait if V24 came back (L3-F06)
        if surface == "cli":
            res = lab.handover("a", "export", str(bundle), "--session", src.sid, "--yes", timeout=300)
            ev.cli("export", res)
            export = {"output": res.text, "job_id": res.job_id, "rc": res.rc}
        else:
            export = _slash_job(lab, "a", src.sid, f"/handover export {bundle} --yes", "export", timeout=300)
            ev.write("export.json", export)
    took = time.monotonic() - t0
    status = lab.job_status("a", export["job_id"]) if export.get("job_id") else {"job": {}}
    listing = sorted(p.name for p in folder.iterdir())
    ev.write("export-status.json", {"status": status, "listing": listing, "took_s": round(took, 1)})
    assert status["job"].get("state") == "done", (export, status["job"])
    assert listing == [bundle.name], listing
    assert status["job"].get("source_kept") is not True, status["job"]
    if surface == "cli":
        assert export["rc"] == 0 and "read-only here" in export["output"], export
    _vetoed(lab, ev, tag, "a", src.sid)

    if surface == "cli":
        imp = lab.handover("b", "import", str(bundle), "--yes", timeout=600)
        ev.cli("import", imp)
        assert imp.rc == 0, imp.brief()
        ijob, final = imp.job_id, imp.text
    else:
        scratch = lab.new_session("b", lab.work("b"))
        run = _slash_job(lab, "b", scratch, f"/handover import {bundle} --yes", "import", timeout=600)
        ev.write("import.json", run)
        ijob = run["job_id"]
        final = json.dumps(run["notices"][-1] if run["notices"] else {}, ensure_ascii=False)
    istatus = lab.job_status("b", ijob) if ijob else {"job": {}}
    ev.write("import-status.json", istatus)
    job = istatus["job"]
    assert job.get("state") == "done", job
    origin = str(job.get("origin") or "")
    assert "verified" in origin.lower() and "unverified" not in origin.lower(), origin
    sid_b = str(_ack(istatus).get("session_id") or "")
    assert sid_b and f"session {sid_b}" in final, final[-2000:]
    dest = _arrived(lab, ev, tag, "b", sid_b, hashes_a, repo_a, root=lab.handover_root("b"))
    _first_turn_has_note(lab, ev, tag, "b", sid_b, dest, src.repo)


@pytest.mark.e2e_timeout(600)
def test_acc1_bundle_export_keep_leaves_the_source_active(lab, ev):
    """``export --keep`` makes a copy: the source stays active and the import note says so.

    Soll: O3 ("``--keep`` erzeugt eine Kopie. Die Import-Note nennt bei Kopien, dass die Quelle weiter aktiv ist");
    README §Use ("``export --keep`` leaves this session active and marks the bundle as a copy; status and the final
    message say so"); the copy's transcript equals the source's at export time (D7)."""
    src = lab.make_source("a", f"acc1keep{lab.run_id}", turns=("copy me",))
    hashes_a = lab.hashes("a", src.sid)
    folder = lab.handover_root("a") / f"acc1keep-{lab.run_id}"
    folder.mkdir(parents=True, exist_ok=True)
    bundle = folder / "keep.ctxh.tar"
    res = lab.handover("a", "export", str(bundle), "--session", src.sid, "--keep", "--yes", timeout=300)
    ev.cli("export-keep", res)
    status = lab.job_status("a", res.job_id) if res.job_id else {"job": {}}
    ev.write("export-status.json", status)
    marks = lab.marks("a", src.sid)
    probe = f"ACC1KEEP-{_lab.uid()}"
    mark = lab.llm_mark()
    turn = lab.turn("a", src.sid, probe)
    reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
    ev.write("source-after-export.json", {"marks": marks, "turn": turn.brief(), "reached": len(reached)})
    imp = lab.handover("b", "import", str(bundle), "--yes", timeout=600)
    ev.cli("import", imp)
    istatus = lab.job_status("b", imp.job_id) if imp.job_id else {"job": {}}
    ev.write("import-status.json", istatus)
    sid_b = str(_ack(istatus).get("session_id") or "")
    note = str((lab.arrival("b", sid_b) or {}).get("note") or "") if sid_b else ""
    ev.write("note.txt", note)

    assert res.rc == 0 and bundle.is_file(), res.brief()
    assert status["job"].get("source_kept") is True, status["job"]
    assert "stays active here" in res.text and "copy" in res.text, res.text
    assert not marks["ended"] and not marks["archived"] and not marks["hidden"], marks
    assert turn.ok and reached, turn.brief()
    assert imp.rc == 0 and istatus["job"].get("state") == "done", imp.brief()
    assert "this is a copy" in note, note[-2000:]
    _arrived(lab, ev, "acc1keep", "b", sid_b, hashes_a, None, root=lab.handover_root("b"))


# ---------------------------------------------------------------------------------------------------------
# 2. R29 / R29a: the default write root inside HERMES_HOME, and sending back from it
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(1200)
def test_acc2_default_workspace_root_and_return_from_it(lab, ev):
    """With B's ``workspace_root``/``allow_roots`` at their schema default (``~/hermes-handover``; in this lab HOME =
    ``lab/b/home`` lies inside HERMES_HOME = ``lab/b``, as in the official Docker image) a pull lands under
    ``<HERMES_HOME>/handover-workspaces/``; after work there, B sends the session back from that folder to A; the
    folder next to it stays a Hermes path, as target root and as source.

    Soll: (1) R29 — the reserved directory is the default root, the pull succeeds (transcript and repository as on A);
    (3) R29a — the reserved directory is a valid source: the push back ends ``done``, the repository with B's new
    commit lands in A's original folder (R4: unchanged since the handover ⇒ in place) and B's copy is locked (R1);
    (4) R29 — a ``workspace_root`` next to the reserved directory (``handover-workspaces-x…``, inside HERMES_HOME) is
    refused at the first contact with a message naming ``workspace_root``, before anything is locked (no job state
    ``locked``/``committing``, A's session not ended, nothing written there); (5) R29a "kein Nachbarpfad" + R30 — a
    session whose cwd lies in that neighbour folder does not send its folder: the summary names it, ``--yes`` does not
    confirm it, nothing is locked.

    Settings: B's two keys are set with ``hermes config set`` and restored byte-exact; B's handover processes are CLI
    processes that read the configuration on each call, so no component restarts."""
    reserved = lab.home("b") / RESERVED
    neighbour = lab.home("b") / f"{RESERVED}-x{lab.run_id}"
    src = lab.make_source("a", f"acc2proj{lab.run_id}", turns=("default receive folder",))
    hashes_a = lab.hashes("a", src.sid)
    repo_a = _lab.repo_state(src.repo)
    other = lab.make_source("a", f"acc2other{lab.run_id}", turns=("neighbour root",))
    with lab.preserved("b"):
        _default_roots(lab, "b")
        ev.write("config-b-default.yaml", lab.config_path("b").read_text(encoding="utf-8"))
        # (1) receive with the default root
        took = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
        ev.cli("take-default-root", took)
        assert took.rc == 0 and took.job_id, took.brief()
        status = lab.job_status("b", took.job_id)
        ev.write("take-status.json", status)
        assert status["job"].get("state") == "done", status["job"]
        sid_b = str(_ack(status).get("session_id") or "")
        dest = _arrived(lab, ev, "acc2", "b", sid_b, hashes_a, repo_a, root=reserved)
        # (2) work on in the reserved folder, (3) send it back from there
        rb = lab.repo(dest)
        rb.write("from-the-reserved-folder.txt", "written on b in handover-workspaces\n")
        rb.add("from-the-reserved-folder.txt")
        head_b = rb.commit("work in the reserved folder")
        repo_b = _lab.repo_state(dest)
        sid_tip = lab.tip("b", sid_b)
        back = lab.handover("b", "to", _lab.SERVER, "--session", sid_tip, "--yes", timeout=600)
        ev.cli("push-back", back)
        back_status = lab.job_status("b", back.job_id) if back.job_id else {"job": {}}
        ev.write("push-back-status.json", back_status)
        repo_back = _lab.repo_state(src.repo)
        ev.write("repo-a-after-return.json", repo_back)
        marks_b = {s: lab.marks("b", s) for s in lab.lineage("b", sid_tip)}
        # (4) a target root next to the reserved one (it exists, so only its place can refuse it)
        neighbour.mkdir(parents=True, exist_ok=True)
        lab.setting("b", "workspace_root", str(neighbour))
        lab.setting("b", "allow_roots", json.dumps([str(neighbour)]))
        refused = lab.handover("b", "take", _lab.SERVER, other.sid, "--yes", timeout=600)
        ev.cli("take-neighbour-root", refused)
        refused_events = lab.events("b", refused.job_id) if refused.job_id else []
        ev.write("neighbour-events.json", refused_events[-40:])
        other_marks = lab.marks("a", other.sid)
        written = sorted(p.name for p in neighbour.iterdir())
        # (5) a source folder next to the reserved one
        _default_roots(lab, "b")
        cwd_x = neighbour / f"proj{lab.run_id}"
        lab.make_repo(cwd_x, dirty=False, ignored=False)
        sid_x = lab.new_session("b", cwd_x)
        turn_x = lab.turn("b", sid_x, "acc2 neighbour source")
        sent = lab.handover("b", "to", _lab.SERVER, "--session", sid_x, "--yes", timeout=600)
        ev.cli("push-from-neighbour", sent)
        marks_x = lab.marks("b", sid_x)
    ev.write("after.json", {"marks_b": marks_b, "other_marks": other_marks, "written": written, "marks_x": marks_x})

    assert back.rc == 0 and back_status["job"].get("state") == "done", back.brief()
    path_a, mode_a = _lab.dest_of(back_status["job"], src.repo.name)
    assert path_a and Path(path_a).resolve() == src.repo.resolve(), (path_a, mode_a)
    for key in REPO_KEYS:
        assert repo_back[key] == repo_b[key], key
    assert repo_back["head"] == head_b
    for seg, m in marks_b.items():
        assert m["ended"] and m["end_reason"] == "handed_over", (seg, m)

    assert refused.rc != 0, refused.brief()
    assert "workspace_root" in refused.text and "Nothing was locked" in refused.text, refused.text[-1500:]
    reached = {str(e.get("state")) for e in refused_events}
    assert not reached & {"locked", "committing", "committed"}, sorted(reached)
    assert other_marks["ended"] is False, other_marks
    assert written == [], written

    assert turn_x.ok, turn_x.brief()
    assert sent.rc != 0, sent.brief()
    assert str(cwd_x) in sent.text and "--yes does not confirm" in sent.text, sent.text[-2000:]
    assert marks_x["ended"] is False, marks_x


# ---------------------------------------------------------------------------------------------------------
# 3. F07: cancel of a suspended job
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(1200)
def test_acc3_cancel_ends_a_suspended_job(lab, ev):
    """A push A→B is suspended (B's listener ``serve-b`` stopped while the workspace moves); B comes back; ``hermes
    handover cancel <job>`` on A, with nothing else driving the job, ends it.

    Soll (L3-F07; DESIGN §3.8 "cancel … stops it before the commit", §3.2 ABORTING → ``target.abort``,
    ``source.release`` → ABORTED; README §Use): the conductor ends ``aborted`` (terminal) without a ``resume``; B's
    target job is terminal (``aborted``/``expired``); A's source handover no longer holds the lineage; a turn on A's
    session runs and reaches the model; the participant slot on B is free — B's ``max_jobs_per_peer`` is set to the
    number of other unfinished jobs of A there plus one (read when ``serve-b`` starts), so the next push from A is
    accepted only if this job's slot was released.

    Deviations: suspension as in E5 (``labctl.py stop serve-b`` at the first observed transfer state, A's
    ``lease_ttl_s`` 30 / ``stall_timeout_min`` 1); ``max_jobs_total`` on B is set to 64 so only the per-peer cap is
    sharp. Both homes' settings are restored, ``serve-b`` restarted."""
    proc = None
    jid = None
    peer_b = _peer_id(lab, "b", _lab.SERVER_IN)
    with lab.preserved("a"), lab.preserved("b", restart=("serve-b",)):
        _short_timeouts(lab)
        src = _big_source(lab, f"acc3proj{lab.run_id}")
        nxt = lab.make_source("a", f"acc3next{lab.run_id}", turns=("the next handover",))
        try:
            proc, jid = _push_bg(lab, ev, src.sid, "acc3")
            seen = lab.wait_job("a", jid, TRANSFER_STATES | {"target_preparing", "aborted", "done"}, poll=0.02)
            ev.write("injection.json", {"job": jid, "seen": seen})
            assert seen in TRANSFER_STATES, f"scaffold: injection window missed (first observed state {seen})"
            lab.stop("serve-b")
            parked = lab.wait_job("a", jid, {"suspended", "aborted", "done"}, timeout=300)
            ev.write("parked.json", {"state": parked, "row": lab.job_row("a", jid)})
            assert parked == "suspended", parked
            _end(proc, 120)
            proc = None
            others = _active_participants(lab, "b", peer_b, exclude=jid)
            cap = len(others) + 1
            lab.setting("b", "max_jobs_per_peer", str(cap))
            lab.setting("b", "max_jobs_total", "64")
            ev.write("cap.json", {"others": others, "max_jobs_per_peer": cap})
            lab.start("serve-b")
            cancel = lab.handover("a", "cancel", jid, timeout=180)
            ev.cli("cancel", cancel)
            final = lab.wait_job("a", jid, _lab.STOP_STATES, timeout=90, poll=0.5)
            target = lab.job_row("b", jid, "target") or {}
            source = lab.source_row("a", jid) or {}
            ev.write("after-cancel.json", {"final": final, "conductor": lab.job_row("a", jid), "target": target})
            ev.write("source-a.json", source)
            probe = f"ACC3-FREE-{_lab.uid()}"
            mark = lab.llm_mark()
            turn = lab.turn("a", src.sid, probe, timeout=120)
            reached = [r for r in lab.llm_since(mark) if probe in _lab.last_user_message(r)]
            ev.write("turn-a.json", {"turn": turn.brief(), "reached": len(reached)})
            following = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", nxt.sid, "--yes", timeout=600)
            ev.cli("next-push", following)
        finally:
            _lab.kill_tree(proc)
            if not lab.running("serve-b"):
                lab.start("serve-b")
            _settle(lab, ev, jid)
    assert cancel.rc == 0, cancel.brief()
    assert final == "aborted", (final, cancel.text)
    assert target.get("state") in {"aborted", "expired"}, target
    assert source.get("state") not in ACTIVE_SOURCE_STATES, source
    assert turn.ok and reached and "handed over" not in turn.text, turn.brief()
    assert following.rc == 0 and following.error_code != "rate_limited", following.brief()
    assert (lab.job_row("a", following.job_id) or {}).get("state") == "done", following.brief()


# ---------------------------------------------------------------------------------------------------------
# 4. F08: target restart during TARGET_PREPARING, then resume
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(1800)
def test_acc4_resume_after_a_target_restart_in_preparing(lab, ev):
    """A push A→B whose target is restarted while it prepares the workspace (its partial directory
    ``<root>/.ctxh-<job6>…`` exists): ``labctl.py stop serve-b`` the moment that directory appears, the job is
    suspended, ``labctl.py start serve-b``, ``/handover resume <job>`` through A's gateway.

    Soll (L3-F08; DESIGN §3.6 target ``preparing`` ⇒ "resume from the journal", §3.5/I9, TESTPLAN E5b "result as
    E1"): the resumed job ends ``done``; transcript hashes and repository on B equal A's; no ``.ctxh-<job6>``
    directory is left in B's root; the partial directory of the stopped prepare was still there after the stop (the
    finding's precondition — checked last, so a missed precondition is reported as such after the result checks).

    Deviations: the window is found by polling B's root every 5 ms; when a run passes PREPARE without the directory
    being seen, that job is let finish and a second source is tried (two attempts). The moment the directory is seen,
    ``serve-b`` is frozen (SIGSTOP), so the prepare cannot finish during the graceful shutdown that ``labctl.py stop``
    starts with SIGTERM; ``labctl.py stop`` then ends it (SIGKILL after its 45-s grace, the lid-close case) and
    ``labctl.py start`` brings it back. A's ``lease_ttl_s`` 30 / ``stall_timeout_min`` 1 (restored)."""
    proc = None
    jid = None
    frozen = None
    attempts = []
    root = lab.handover_root("b")
    with lab.preserved("a"):
        _short_timeouts(lab)
        try:
            partial = None
            for attempt in (1, 2):
                src = _big_source(lab, f"acc4p{attempt}{lab.run_id}")
                hashes_a = lab.hashes("a", src.sid)
                repo_a = _lab.repo_state(src.repo)
                proc, jid = _push_bg(lab, ev, src.sid, f"acc4-{attempt}")
                job6 = jid[-6:].lower()
                state = None
                deadline = time.monotonic() + 420
                while time.monotonic() < deadline:
                    found = sorted(root.glob(f".ctxh-{job6}*"))
                    state = (lab.job_row("a", jid) or {}).get("state")
                    if found:
                        frozen = lab.pid("serve-b")
                        if frozen:
                            os.kill(frozen, signal.SIGSTOP)
                        partial = found[0]
                        break
                    if state in PAST_PREPARE:
                        break
                    time.sleep(0.005)
                attempts.append({"attempt": attempt, "job": jid, "partial": str(partial or ""), "state": state})
                if partial is not None:
                    break
                lab.wait_job("a", jid, _lab.STOP_STATES | {"suspended", "in_doubt"}, timeout=300)
                _end(proc, 60)
                proc = None
            ev.write("attempts.json", attempts)
            assert partial is not None, f"scaffold: no partial directory seen in two attempts: {attempts}"
            partial_frozen = partial.exists()
            lab.stop("serve-b")
            frozen = None
            partial_after_stop = partial.exists()
            parked = lab.wait_job("a", jid, {"suspended", "aborted", "done"}, timeout=420)
            ev.write(
                "parked.json",
                {
                    "state": parked,
                    "partial": str(partial),
                    "frozen": partial_frozen,
                    "after_stop": partial_after_stop,
                    "row": lab.job_row("a", jid),
                },
            )
            assert parked == "suspended", parked
            _end(proc, 120)
            proc = None
            lab.start("serve-b")
            scratch = lab.new_session("a", lab.work("a"))
            out, events = lab.slash("a", scratch, f"/handover resume {jid}", timeout=300)
            ev.write("resume.json", {"output": out, "events": [e.get("type") for e in events]})
            final = lab.wait_job("a", jid, _lab.STOP_STATES | {"suspended", "in_doubt"}, timeout=600)
        finally:
            _lab.kill_tree(proc)
            if frozen and _lab.pid_alive(frozen):
                os.kill(frozen, signal.SIGCONT)
            if not lab.running("serve-b"):
                lab.start("serve-b")
            _settle(lab, ev, jid)
    status = lab.job_status("a", jid)
    ev.write("job-a.json", status)
    leftovers = sorted(p.name for p in root.glob(f".ctxh-{jid[-6:].lower()}*"))
    ev.write("leftovers.json", leftovers)
    assert final == "done", status["job"]
    sid_b = str(_ack(status).get("session_id") or "")
    _arrived(lab, ev, "acc4", "b", sid_b, hashes_a, repo_a, root=root)
    assert leftovers == [], leftovers
    assert partial_after_stop, "precondition of L3-F08 not reproduced: the stopped target left no partial directory"


# ---------------------------------------------------------------------------------------------------------
# 5. F12: offer from the gateway, take --offer
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(900)
def test_acc5_gateway_offer_is_redeemed_by_take_offer(lab, ev):
    """``/handover to laptop`` in A's gateway (``laptop`` only dials in) leaves an offer; ``hermes handover take
    server --offer <id> --yes`` on B claims it.

    Soll: the slash answers with the offer and the session stays usable on A until taken (O6 "Nie Auto-Claim",
    README §Use "leaves an offer (2 h)"); the take ends ``done`` and the offer is claimed by that job (L3-F12, DESIGN
    §14.1 ``take … --offer <id>``); the arrived session is the offered one — transcript hashes, repository and title
    equal A's (D7, D10); A's session is locked afterwards (R1)."""
    src = lab.make_source("a", f"acc5proj{lab.run_id}", turns=("offer this from the gateway",))
    hashes_a = lab.hashes("a", src.sid)
    repo_a = _lab.repo_state(src.repo)
    out, _events = lab.slash("a", src.sid, f"/handover to {_lab.LAPTOP}", timeout=300)
    ev.write("offer.txt", out)
    offers = lab.ledger_rows("a", "SELECT * FROM offers WHERE session_id = ?", (src.sid,))
    ev.write("offers-a.json", offers)
    marks = lab.marks("a", src.sid)
    assert "Offer created" in out, out
    assert len(offers) == 1 and not offers[0].get("claimed_job"), offers
    assert marks["ended"] is False, marks
    oid = str(offers[0]["offer_id"])
    take = lab.handover("b", "take", _lab.SERVER, "--offer", oid, "--yes", timeout=600)
    ev.cli("take-offer", take)
    status = lab.job_status("b", take.job_id) if take.job_id else {"job": {}}
    ev.write("job-b.json", status)
    claimed = lab.ledger_rows("a", "SELECT claimed_job FROM offers WHERE offer_id = ?", (oid,))
    ev.write("claimed.json", claimed)
    assert take.rc == 0 and status["job"].get("state") == "done", take.brief()
    assert claimed and claimed[0]["claimed_job"] == take.job_id, claimed
    sid_b = str(_ack(status).get("session_id") or "")
    _arrived(lab, ev, "acc5", "b", sid_b, hashes_a, repo_a, root=lab.handover_root("b"))
    assert lab.row("b", sid_b).get("title") == src.title
    _vetoed(lab, ev, "acc5", "a", src.sid)


# ---------------------------------------------------------------------------------------------------------
# 6. F09 / R34: the refinement reaches the plan
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.e2e_timeout(900)
@pytest.mark.parametrize("surface", ["cli", "gateway"])
def test_acc6_refinement_lands_in_the_plan(lab, ev, surface):
    """A push dry run from A (CLI ``hermes handover to laptopgw --session … --dry-run --yes`` or the same slash command
    in A's gateway) with a fake-model rule that answers the refinement with a path outside the project; then the same
    dry run with ``refine: off``.

    Soll: R34 / L3-F09 — the refinement under the session context is asked (the fake model gets the request from
    the source side, ``ctx.llm.complete_structured`` in CLI and gateway, D9) and its answer is taken: the plan names
    the added path; R18 / hardening 18 / DESIGN §8.4 — a path outside the project is only *proposed*
    (``[proposed]``, ``refine_auto_include`` default false); O4 — with ``refine: off`` no refinement request is made
    and the path is not in the plan; ``--dry-run`` locks nothing (DESIGN §14.1).

    Deviation: in this lab HOME lies inside HERMES_HOME, so an extra path outside the project passes the source
    policy's location rule only under ``export_roots`` (``engine/policy.py`` ``_location_keys``: ``$HOME`` or an
    export root); A's ``export_roots`` is set to the extra folder for the test (restored)."""
    src = lab.make_source("a", f"acc6{surface}{lab.run_id}", turns=("which resources does this work need",))
    extra = lab.ws("a") / f"acc6-extra-{surface}-{lab.run_id}"
    extra.mkdir(parents=True, exist_ok=True)
    resource = extra / "resource-notes.md"
    resource.write_text("notes the file scan cannot see\n", encoding="utf-8")
    note = f"ACC6-NOTE-{_lab.uid()}"
    rid = f"acc6-refine-{_lab.uid()}"
    answer = {
        "add": [{"path": str(resource), "reason": "the notes the work continues from"}],
        "drop": [],
        "tools_needed": ["acc6tool"],
        "notes_for_target": note,
    }
    rule = {
        "id": rid,
        "when": {"kind": "aux", "has_response_format": True, "any_contains": "notes_for_target"},
        "reply": {"text": json.dumps(answer)},
    }
    with lab.preserved("a"):
        lab.setting("a", "export_roots", json.dumps([str(extra)]))
        with lab.llm_rules([rule]):
            mark = lab.llm_mark()
            on, on_raw = _dry_run(lab, surface, src.sid)
            asked_on = _refine_requests(lab.llm_since(mark), rid)
            lab.setting("a", "refine", "off")
            mark = lab.llm_mark()
            off, off_raw = _dry_run(lab, surface, src.sid)
            asked_off = _refine_requests(lab.llm_since(mark), rid)
    marks = lab.marks("a", src.sid)
    ev.write("dry-run-refine-auto.txt", on)
    ev.write("dry-run-refine-off.txt", off)
    ev.write(
        "dry-runs.json",
        {
            "on": on_raw,
            "off": off_raw,
            "asked_on": _lab.compact_records(asked_on),
            "asked_off": _lab.compact_records(asked_off),
            "marks": marks,
        },
    )
    line = next((ln for ln in on.splitlines() if str(resource) in ln), None)
    assert asked_on, "no refinement request reached the fake model"
    assert line is not None, on[-3000:]
    assert "[proposed]" in line, line
    assert "nothing was locked" in on.lower(), on[-1500:]
    assert not asked_off, f"refine: off still asked the model ({len(asked_off)} requests)"
    assert str(resource) not in off, off[-3000:]
    assert marks["ended"] is False, marks


# ---------------------------------------------------------------------------------------------------------
# 7. R10 / R22: --continue refused on the target
# ---------------------------------------------------------------------------------------------------------


def test_acc7_continue_allowed_is_e19():
    """R10 with ``allow_remote_continue: true`` (exactly one turn with the note, session adopted) and R22 with
    ``approvals.single_query_mode: approve`` (``continue_refused``, no turn) are asserted by E19."""
    pytest.skip("covered by tests/e2e/test_e2e_flows.py::test_e19_push_and_continue (not duplicated)")


@pytest.mark.e2e_timeout(900)
@pytest.mark.parametrize("variant", ["not_allowed", "approvals_mode_off"])
def test_acc7_continue_refused_on_the_target(lab, ev, variant):
    """``hermes handover to server --session … --continue "<prompt>" --yes`` from B, with A not allowing it.

    ``not_allowed``: A's ``allow_remote_continue`` is false (the default). ``approvals_mode_off``: A allows it but runs
    with ``approvals.mode: off`` (the other switch of "approvals off", next to E19's ``single_query_mode``; ``hermes
    config set`` writes the word as YAML ``false``, which ``engine/hostcompat.py`` ``approvals_off`` reads as off).

    Soll: the handover itself ends ``done`` and the session arrives active on A (R10: the continuation is optional);
    no continuation starts and no turn with the prompt reaches the model within 15 s (R10 "ausdrückliche Wahl … plus
    Ziel-Einstellung", R22 "startet nicht, wenn am Ziel Approvals abgeschaltet sind"); the job records
    ``continue_refused`` with the reason (``not_allowed`` | ``approvals_off``, ``engine/continuation.py``) and the
    message names it (``allow_remote_continue`` | "approvals are switched off"; ``engine/surfaces.py``
    ``continue_refused``)."""
    pids: list[int] = []
    reason, words = (
        ("not_allowed", "allow_remote_continue")
        if variant == "not_allowed"
        else ("approvals_off", "approvals are switched off")
    )
    with lab.preserved("a", restart=("serve-a",)):
        if variant == "not_allowed":
            lab.setting("a", "allow_remote_continue", "false")
        else:
            lab.setting("a", "allow_remote_continue", "true")
            lab.config_set("a", "approvals.mode", "off")
        ev.write("config-a.yaml", lab.config_path("a").read_text(encoding="utf-8"))
        lab.restart("serve-a")
        src = lab.make_source("b", f"acc7{variant.replace('_', '')}{lab.run_id}", turns=("closing",))
        prompt = f"ACC7-{variant}-{_lab.uid()}"
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
    assert "continue_refused" in json.dumps(cont) and reason in json.dumps(cont), cont
    assert words in res.text.lower() or words in res.text, res.text[-2000:]
    assert not late, "a continuation turn ran although the target refuses it"


# ---------------------------------------------------------------------------------------------------------
# 8. R4: the way back
# ---------------------------------------------------------------------------------------------------------


def test_acc8_return_in_place_is_e04():
    """R4 with uncommitted work handed away: the return updates A's original folder in place, the ignored ``build/``
    survives byte-equal, no ``<dest>-handover-<ts>`` folder exists — all asserted by E4 on the E1→E3 chain."""
    pytest.skip(
        "covered by tests/e2e/test_e2e_roundtrip.py::test_e04_return_to_the_same_path_with_handed_away_wip "
        "(not duplicated)"
    )


@pytest.mark.e2e_timeout(900)
def test_acc8_return_to_a_changed_origin_keeps_it(lab, ev):
    """Pull A→B, then A's original folder changes (an edit and a new file after the handover), then B pushes back.

    Soll: R4 ("der Rückweg prüft 'unverändert seit Übergabe' statt 'sauber'") — the folder changed since the
    handover, so the return must not update it in place: A's folder is byte-identical afterwards (``.git``
    included), the repository lands in another folder (D10 "Am Ziel nie uncommittete lokale Änderungen
    überschreiben (Konflikt → frisches Verzeichnis)"), and that folder holds B's state (HEAD, status, index, refs,
    work tree)."""
    from ctx_handover.tests.fakes.repo_builder import dir_fingerprint

    src = lab.make_source("a", f"acc8proj{lab.run_id}", turns=("there and back",))
    took = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
    ev.cli("take", took)
    assert took.rc == 0 and took.job_id, took.brief()
    status = lab.job_status("b", took.job_id)
    ev.write("take-status.json", status)
    sid_b = str(_ack(status).get("session_id") or "")
    dest_b = Path(str(lab.row("b", sid_b).get("cwd") or ""))
    assert dest_b.is_dir(), dest_b
    src.rb.write("README.md", "# changed on the server after the handover\n")
    src.rb.write("server-only.txt", "written on the server after the handover\n")
    origin_before = dir_fingerprint(src.repo)
    repo_b = _lab.repo_state(dest_b)
    back = lab.handover("b", "to", _lab.SERVER, "--session", lab.tip("b", sid_b), "--yes", timeout=600)
    ev.cli("push-back", back)
    back_status = lab.job_status("b", back.job_id) if back.job_id else {"job": {}}
    ev.write("push-back-status.json", back_status)
    path_a, mode_a = _lab.dest_of(back_status["job"], src.repo.name)
    ev.write("dest.json", {"path": path_a, "mode": mode_a, "origin": str(src.repo)})
    assert back.rc == 0 and back_status["job"].get("state") == "done", back.brief()
    assert dir_fingerprint(src.repo) == origin_before, "A's changed original folder was modified by the return"
    assert path_a and Path(path_a).resolve() != src.repo.resolve(), (path_a, mode_a)
    assert not (mode_a or "").lower().endswith("return"), mode_a
    repo_back = _lab.repo_state(Path(path_a))
    ev.write("repo-a-fresh.json", repo_back)
    for key in ("head", "status", "index", "refs", "tree"):
        assert repo_back[key] == repo_b[key], key
