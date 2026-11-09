"""Layer 3 — regression tests for the findings of the two-gateway rehearsal (``reports/L3-findings.md`` L3-F01…F17 and
the lab questions L15/L17 of ``reports/L3.md``), each asserting the target behaviour of ``design/DECISIONS.md``.

Every test names the source of each assertion in its docstring and restores what it changes. Findings checked end to
end in ``test_l3_acceptance.py`` instead of here (not duplicated):

* L3-F07 (cancel of a suspended job) — ``test_acc3_cancel_ends_a_suspended_job``;
* L3-F08 (resume after a target restart in PREPARING) — ``test_acc4_resume_after_a_target_restart_in_preparing``;
* L3-F09 / R34 (refinement taken) — ``test_acc6_refinement_lands_in_the_plan[cli|gateway]``;
* L3-F12 (``take --offer``) — ``test_acc5_gateway_offer_is_redeemed_by_take_offer``;
* the bundle import side of L3-F06 and O3 ``--keep`` — ``test_acc1_*``; the R29 return trip — ``test_acc2_*``.

The pull case of R35 (L3-F13) is E15 (``test_e2e_flows.py::test_e15_reclaim``); here only the unreachable target.

Runtime estimate: 15–20 min (F11/R33 restarts gw-a and serve-a twice, R33 waits 30 s more when the pull is refused,
F14 waits for a 25-s model delay, the others run one or two handovers each). The review findings of
``review-code/REVIEW.md`` §2 are ``test_l3_review.py``.
"""

from __future__ import annotations

import json
import re
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

from . import _lab

needs_lab = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)

DEFAULT_ROOT = "~/hermes-handover"
"""Schema default of ``workspace_root`` and ``allow_roots`` (``plugin.yaml`` ``config_schema``)."""
RESERVED = "handover-workspaces"
"""R29: the reserved ``<HERMES_HOME>/handover-workspaces`` (``engine/paths.py`` ``HERMES_WORKSPACES_DIR``)."""
REPO_KEYS = ("head", "branch", "status", "index", "refs", "stash", "tree")

_STAYS_ACTIVE = re.compile(r"\b(?:stays|remains|is still|keeps (?:being|running))\b[^.]{0,40}\bactive\b", re.I)
_NOT_TOLD = re.compile(
    r"\b(?:not|never)\b[^.]{0,20}\b(?:told|notified|informed|learn(?:s)?|know(?:s)?|hear(?:s)?|find(?:s)? out)\b"
    r"|\b(?:learns|knows|hears) nothing\b|\bunaware\b|\bnothing (?:about|of) (?:this|it|the reclaim)\b",
    re.I,
)
"""R35 wording: the copy on the target stays active, and it is not told / learns nothing of the reclaim."""


def _ack(status: dict) -> dict:
    ack = status.get("job", {}).get("ack")
    return ack if isinstance(ack, dict) else {}


def _notices(events: list[dict]) -> list[dict]:
    return [
        e.get("payload") or {}
        for e in events
        if e.get("type") == "notification.show" and "plugin-message:ctx_handover" in json.dumps(e, default=str)
    ]


def _conductor_job(lab, inst: str, after: float, mode: str, timeout: float = 120.0) -> dict | None:
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


def _pull(lab, ev, sid: str, name: str = "take", *extra: str) -> tuple[str, str]:
    res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", *extra, timeout=600)
    ev.cli(name, res)
    assert res.rc == 0 and res.job_id, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write(f"{name}-status.json", status)
    assert status["job"].get("state") == "done", status["job"]
    return res.job_id, str(_ack(status).get("session_id") or "")


def _listed(reply: dict) -> list[str]:
    result = reply.get("result") or {}
    items = result.get("sessions") or result.get("items") or (result if isinstance(result, list) else [])
    out = []
    for item in items:
        if isinstance(item, dict):
            out.append(str(item.get("id") or item.get("session_id") or (item.get("ref") or {}).get("session_id")))
    return out


# ---------------------------------------------------------------------------------------------------------
# F01 / R29, F02 / R30
# ---------------------------------------------------------------------------------------------------------


@needs_lab
@pytest.mark.e2e_timeout(600)
@pytest.mark.parametrize("variant", ["repo", "session_only"])
def test_l3_f01_default_workspace_root_lands_in_handover_workspaces(lab, ev, variant):
    """L3-F01: B's ``workspace_root``/``allow_roots`` at the schema default ``~/hermes-handover``, which lies inside
    HERMES_HOME in this lab (HOME = ``lab/b/home``; the official Docker image: HOME = HERMES_HOME = ``/opt/data``).
    ``repo``: the session's repository is a workspace item. ``session_only``: the cwd lies in A's Hermes home and
    ``--no-workspace`` hands the session over without it, so only the commit step creates the cwd under the default
    root — the case of the finding (c2:cwd).

    Soll: R29 — the default root is ``<HERMES_HOME>/handover-workspaces/`` and receiving succeeds (``done``); R11 —
    the target's cwd exists under it; ``repo``: the repository there equals A's (BRIEF D10). README §Use: "``--no-
    workspace`` hands the session over without its folder on purpose". The return trip from that folder (R29a) is
    ``test_l3_acceptance.py::test_acc2_*``."""
    if variant == "repo":
        src = lab.make_source("a", f"f01repo{lab.run_id}", turns=("default receive folder",))
        sid, extra, repo_a = src.sid, (), _lab.repo_state(src.repo)
    else:
        cwd = lab.work("a") / f"f01only{lab.run_id}"
        cwd.mkdir(parents=True, exist_ok=True)
        sid = lab.new_session("a", cwd)
        assert lab.turn("a", sid, "default receive folder, session only").ok
        with lab.db("a", write=True) as db:
            db.set_session_title(sid, f"f01only {lab.run_id}")
        extra, repo_a = ("--no-workspace",), None
    with lab.preserved("b"):
        lab.setting("b", "workspace_root", DEFAULT_ROOT)
        lab.setting("b", "allow_roots", json.dumps([DEFAULT_ROOT]))
        _jid, sid_b = _pull(lab, ev, sid, "take-default-root", *extra)
    row_b = lab.row("b", sid_b)
    ev.write("row-b.json", row_b)
    dest = Path(str(row_b.get("cwd") or ""))
    assert dest.is_dir(), row_b.get("cwd")
    assert dest.resolve().is_relative_to((lab.home("b") / RESERVED).resolve()), str(dest)
    if repo_a is not None:
        repo_b = _lab.repo_state(dest)
        ev.write("repo-b.json", repo_b)
        for key in REPO_KEYS:
            assert repo_b[key] == repo_a[key], key


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_f02_left_out_working_folder_is_named_and_needs_a_person(lab, ev):
    """L3-F02: a session whose cwd lies inside A's Hermes home (``lab/a/work/…``; on Docker every workspace does)
    must not leave silently with "0 items, 0 B".

    Soll: R30 — the summary before sending names the left-out folder and asks; ``--yes`` does not confirm it, so the
    CLI without a terminal cancels with the text "--yes does not confirm …" and the hint ``--no-workspace``, and
    nothing is locked (A's session not ended, no job of B reached ``locked``); README §Use — ``--no-workspace``
    hands the session over without its folder on purpose; R23 / DESIGN §11.3 — the note names the folder left
    behind."""
    cwd = lab.work("a") / f"f02proj{lab.run_id}"
    lab.make_repo(cwd)
    sid = lab.new_session("a", cwd)
    assert lab.turn("a", sid, "cwd inside the hermes home").ok
    with lab.db("a", write=True) as db:
        db.set_session_title(sid, f"f02 {lab.run_id}")
    refused = lab.handover("b", "take", _lab.SERVER, sid, "--yes", timeout=600)
    ev.cli("take-yes", refused)
    events = lab.events("b", refused.job_id) if refused.job_id else []
    ev.write("events-b.json", events[-40:])
    marks = lab.marks("a", sid)
    text = refused.text
    assert refused.rc != 0, refused.brief()
    assert f"Working folder left out: {cwd}" in text, "the summary does not name the folder left behind"
    assert "--yes does not confirm" in text and "--no-workspace" in text, text[-2000:]
    assert marks["ended"] is False, marks
    assert not {str(e.get("state")) for e in events} & {"locked", "committing", "committed"}
    _jid, sid_b = _pull(lab, ev, sid, "take-no-workspace", "--no-workspace")
    note = str((lab.arrival("b", sid_b) or {}).get("note") or "")
    ev.write("note.txt", note)
    assert str(cwd) in note, "the note does not name the folder left behind"


# ---------------------------------------------------------------------------------------------------------
# F03 / R14 / L17, F04
# ---------------------------------------------------------------------------------------------------------


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_f03_gateway_final_message_names_target_session_model_and_how_to_open(lab, ev):
    """L3-F03: the final notice of a push started in the gateway (``/handover to <peer> --yes``).

    Soll: R14 ("Die Abschlussmeldung nennt Zielsession, Modell, Offline-Tauglichkeit und den nächsten Schritt"),
    README §Use ("The final message names the session on the other side (title and session id), its folder, the
    model and whether it works offline, and what to do next"), DESIGN §11.6; L17 fallback (``reports/L3.md``: a
    never opened import is missing from the default ``session.list``) — the message says how to open it: ``hermes
    --resume <id>`` (``engine/surfaces.py`` ``open_text``)."""
    src = lab.make_source("a", f"f03proj{lab.run_id}", turns=("final message",))
    t0 = time.time()
    gw = lab.gw("a")
    jid = None
    try:
        gw.rpc("session.resume", {"session_id": src.sid})
        reply = gw.rpc(
            "slash.exec", {"session_id": src.sid, "command": f"/handover to {_lab.LAPTOP_GW} --yes"}, timeout=300
        )
        row = _conductor_job(lab, "a", t0, "push", timeout=60)
        jid = str(row["job_id"]) if row else None
        deadline = time.monotonic() + 420
        while jid and time.monotonic() < deadline:
            gw.linger(1.0)
            if (lab.job_row("a", jid) or {}).get("state") in _lab.STOP_STATES | {"suspended", "in_doubt"}:
                gw.linger(4.0)
                break
        events = list(gw.events)
    finally:
        gw.close()
    notices = _notices(events)
    status = lab.job_status("a", jid) if jid else {"job": {}}
    ev.write("notices.json", {"reply": reply, "notices": notices[-10:], "status": status})
    ack = _ack(status)
    assert status["job"].get("state") == "done", status
    final = json.dumps(notices[-1] if notices else {}, ensure_ascii=False)
    assert f"session {ack.get('session_id')}" in final, final
    assert f"model {ack.get('model')}" in final, final
    path, _mode = _lab.dest_of(status["job"])
    assert path and path in final, (path, final)
    assert str(ack.get("title") or src.title) in final, final
    assert f"hermes --resume {ack.get('session_id')}" in final, final


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_f04_participant_side_status_finds_the_job(lab, ev):
    """L3-F04: after a push A→B, ``hermes handover status <job>`` on the receiving side B finds the job and shows the
    note; the overview without a job lists the arrival. After a pull, the source side finds the job too.

    Soll: README §Use ("On the receiving side ``/handover status <job>`` shows the note the agent gets"; "``/handover
    status`` also shows the jobs another gateway runs with this one … listed with this gateway's role"), DESIGN
    §11.6 ("After a push, ``/handover status`` on the target lists the arrival"); contract V43 (role ``target`` on B,
    ``source`` on A)."""
    src = lab.make_source("a", f"f04proj{lab.run_id}", turns=("status on both sides",))
    pushed = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--yes", timeout=600)
    ev.cli("push", pushed)
    assert pushed.rc == 0 and pushed.job_id, pushed.brief()
    on_b = lab.handover("b", "status", pushed.job_id, timeout=180)
    ev.cli("status-b-job", on_b)
    overview = lab.handover("b", "status", timeout=180)
    ev.cli("status-b-overview", overview)
    src2 = lab.make_source("a", f"f04bproj{lab.run_id}", turns=("pulled",))
    pulled = lab.handover("b", "take", _lab.SERVER, src2.sid, "--yes", timeout=600)
    ev.cli("pull", pulled)
    on_a = lab.handover("a", "status", pulled.job_id or "missing", timeout=180)
    ev.cli("status-a-job", on_a)
    job_b = (on_b.data.get("status") or {}).get("job") or {}
    job_a = (on_a.data.get("status") or {}).get("job") or {}
    assert on_b.rc == 0 and on_b.error_code is None, on_b.brief()
    assert _lab.NOTE_MARK in on_b.text or "Moved from" in on_b.text, on_b.text[-1500:]
    assert job_b.get("role") == "target", job_b
    short = pushed.job_id[-6:]
    assert pushed.job_id in overview.text or short in overview.text.upper(), overview.text[-1500:]
    assert on_a.rc == 0 and on_a.error_code is None, on_a.brief()
    assert job_a.get("role") == "source", job_a


# ---------------------------------------------------------------------------------------------------------
# F05
# ---------------------------------------------------------------------------------------------------------


@needs_lab
@pytest.mark.e2e_timeout(300)
def test_l3_f05_name_conflict_is_refused_before_the_code_is_redeemed(lab, ev):
    """L3-F05: pasting a pairing string with ``--name`` equal to an existing peer is refused **before** the code is
    redeemed: the listener keeps no inbound peer the dialer does not know.

    Soll: DESIGN §12.3 (pairing is two-sided or not at all), README §Set up step 4 ("a taken name is refused before the
    code is used")."""
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


# ---------------------------------------------------------------------------------------------------------
# F06
# ---------------------------------------------------------------------------------------------------------


@needs_lab
@pytest.mark.e2e_timeout(600)
@pytest.mark.parametrize("surface", ["cli", "gateway"])
def test_l3_f06_export_writes_a_bundle(lab, ev, surface):
    """L3-F06 (a): ``hermes handover export <path> --session <id> --yes`` (CLI) and ``/handover export <path> --yes``
    (gateway) each write one ``.ctxh.tar`` and the job ends ``done`` within a bounded time.

    Soll: BRIEF D5/D13, README §Use ("``/handover export`` writes one ``.ctxh.tar``"), L2-contracts V24 (no
    ``.partial`` left, no ``in_doubt``); bound: 180 s from the command (``abcbd8a7``: ``in_doubt`` after 182 s with
    ``stall_timeout_min: 1``, which is set here too — restored). Import side: ``test_l3_acceptance.py::test_acc1_*``."""
    src = lab.make_source("a", f"f06{surface}{lab.run_id}", turns=("export",))
    folder = lab.handover_root("a") / f"f06{surface}-{lab.run_id}"
    folder.mkdir(parents=True, exist_ok=True)
    bundle = folder / "f06.ctxh.tar"
    jid = state = None
    try:
        with lab.preserved("a"):
            lab.setting("a", "stall_timeout_min", "1")
            t0, w0 = time.monotonic(), time.time()
            if surface == "cli":
                res = lab.handover("a", "export", str(bundle), "--session", src.sid, "--yes", timeout=300)
                ev.cli("export", res)
                jid = res.job_id
                state = (lab.job_row("a", jid) or {}).get("state") if jid else None
            else:
                out, _ = lab.slash("a", src.sid, f"/handover export {bundle} --yes", timeout=300)
                ev.write("export-slash.txt", out)
                row = _conductor_job(lab, "a", w0, "export", timeout=60)
                jid = str(row["job_id"]) if row else None
                if jid:
                    state = lab.wait_job("a", jid, _lab.STOP_STATES | {"in_doubt", "suspended"}, timeout=240, poll=0.5)
            took = time.monotonic() - t0
        listing = sorted(p.name for p in folder.iterdir())
        ev.write("export.json", {"job": jid, "state": state, "took_s": round(took, 1), "folder": listing})
        assert state == "done", (state, lab.job_status("a", jid) if jid else None)
        assert listing == [bundle.name], listing
        assert took <= 180, f"the export took {took:.0f} s"
    finally:
        ev.cli("cleanup-reclaim", lab.handover("a", "reclaim", src.sid, "--force", timeout=180))


def test_l3_f06_in_doubt_wait_is_bounded_by_stall_timeout():
    """L3-F06 (b): ``stall_timeout_min`` bounds the in-doubt wait from the first IN_DOUBT entry (contract V39)."""
    pytest.skip(
        "not reproducible in the lab since V24 is fixed: the finding needed a commit the target refuses fast and "
        "repeatedly (V24's 'bundle commit before prepare'); the lab has no fault hook for that, and a stopped target "
        "never answers 'prepared' (no re-entry). Covered by "
        "tests/unit/A/test_gp_a.py::test_stall_timeout_counts_from_the_first_in_doubt_entry"
    )


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_f06_reclaim_accepts_the_session_option(lab, ev):
    """L3-F06 (c): ``hermes handover reclaim --session <id>`` is accepted (README §Use: "in a terminal ``hermes
    handover reclaim --session <id>`` (or ``reclaim <id>``)"; observed on ``abcbd8a7``: "--session is not an option
    of 'reclaim'")."""
    src = lab.make_source("a", f"f06cproj{lab.run_id}", turns=("reclaim with --session",))
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


# ---------------------------------------------------------------------------------------------------------
# F10, F11, L15/R33
# ---------------------------------------------------------------------------------------------------------


@needs_lab
@pytest.mark.e2e_timeout(900)
def test_l3_f10_terminal_output_files_travel_with_the_session(lab, ev):
    """L3-F10: a terminal tool call with a long output on A (the tool keeps ≈ 59 500 characters and writes the rest to
    ``<HERMES_HOME>/cache/terminal-output/…``, named as ``full_output_path`` in the transcript); pull to B.

    Soll: BRIEF D10 ("sessionbezogene HERMES_HOME-Dateien"), DESIGN §6.11, README Disclosure ("``cache/spillover``,
    ``hook_outputs``, ``native-inputs`` and ``cache/terminal-output`` files of that session") — every such file exists
    on B at the same place relative to B's HERMES_HOME, byte-equal (the transcript keeps its paths, BRIEF D10)."""
    tools = [{"name": "terminal", "arguments": {"command": "seq 1 40001"}}]
    src = lab.make_source("a", f"f10proj{lab.run_id}", turns=("long output",), tool_turn=tools)
    paths = []
    for m in lab.messages("a", src.sid):
        for path in re.findall(r'"full_output_path": "([^"]+)"', _lab.message_text(m)):
            if path not in paths:
                paths.append(path)
    ev.write("terminal-output-a.json", paths)
    home_a = lab.home("a")
    local = [Path(p) for p in paths if Path(p).is_file() and Path(p).resolve().is_relative_to(home_a)]
    assert local, f"scaffold: the terminal call left no output file under A's home: {paths}"
    _jid, sid_b = _pull(lab, ev, src.sid)
    copies = {}
    for path in local:
        there = lab.home("b") / path.resolve().relative_to(home_a)
        copies[str(path)] = {"on_b": str(there), "exists": there.is_file()}
        if there.is_file():
            copies[str(path)]["equal"] = _lab.file_sha(there) == _lab.file_sha(path)
    ev.write("terminal-output-b.json", {"session": sid_b, "copies": copies})
    for path, info in copies.items():
        assert info["exists"] and info.get("equal"), (path, info)


@needs_lab
@pytest.mark.e2e_timeout(900)
def test_l3_f11_abort_ends_when_the_source_never_opened_the_job(lab, ev):
    """L3-F11: a pull from B whose source never opens the job — ctx_handover is disabled on A and ``serve-a`` is
    restarted, so A's dashboard has no ctx_handover routes (``listener_missing``).

    Soll: DESIGN §3.2 (ABORTING → ABORTED), §3.7 (``source.release`` idempotent: the source that never had the job is
    no reason to stay in ``aborting``) — B's job ends ``aborted``; DESIGN §14.1 — the message names the cause ("has
    no ctx_handover routes"), not "job … not found"; L3-F14 / README §Use — exit code ≠ 0 and ``error.code``
    ``listener_missing`` in ``--json``; A's session is untouched (not ended).

    Lab means: ``hermes plugins disable ctx_handover`` on A (as E20 does), restart of ``serve-a`` through
    ``labctl.py``; afterwards ``hermes plugins enable``, ``config.yaml`` restored byte-exact and ``serve-a``
    restarted (``gw-a`` is not restarted and keeps the plugin loaded)."""
    src = lab.make_source("a", f"f11proj{lab.run_id}", turns=("source without routes",))
    with lab.preserved("a", restart=("serve-a",)):
        try:
            disabled = lab.hermes("a", "plugins", "disable", "ctx_handover", timeout=300)
            ev.write("disable.txt", f"rc={disabled.returncode}\n{disabled.stdout}\n{disabled.stderr}")
            lab.restart("serve-a")
            res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
            ev.cli("take", res)
            final = lab.wait_job("b", res.job_id, _lab.STOP_STATES, timeout=120, poll=0.5) if res.job_id else None
            ev.write("job-b.json", {"final": final, "row": lab.job_row("b", res.job_id) if res.job_id else None})
        finally:
            enabled = lab.hermes("a", "plugins", "enable", "ctx_handover", timeout=300)
            ev.write("enable.txt", f"rc={enabled.returncode}\n{enabled.stdout}\n{enabled.stderr}")
    marks = lab.marks("a", src.sid)
    assert res.rc != 0, res.brief()
    assert "has no ctx_handover routes" in res.text, res.text[-1500:]
    assert "not found" not in res.text.lower(), res.text[-1500:]
    assert res.error_code == "listener_missing", res.brief()
    if res.job_id:
        assert final == "aborted", final
    assert marks["ended"] is False, marks


class _HomeLogs:
    """New part of a home's Hermes logs since construction (``<HERMES_HOME>/logs/agent.log`` and ``errors.log``; the
    plugin's ``ctx_handover listener unavailable`` / ``identity not usable`` lines land there, g5.md §0)."""

    NAMES = ("agent.log", "errors.log")
    KEEP = 400_000

    def __init__(self, lab, inst: str) -> None:
        self.dir = lab.home(inst) / "logs"
        self.offsets = {name: self._size(name) for name in self.NAMES}

    def _size(self, name: str) -> int:
        path = self.dir / name
        return path.stat().st_size if path.exists() else 0

    def save(self, ev, tag: str) -> None:
        for name in self.NAMES:
            path = self.dir / name
            if not path.exists():
                ev.write(f"{tag}-{name}.txt", f"{path} does not exist")
                continue
            size = path.stat().st_size
            start = self.offsets[name] if size >= self.offsets[name] else 0  # rotated: from the start
            with open(path, "rb") as fh:
                fh.seek(max(start, size - self.KEEP))
                ev.write(f"{tag}-{name}", fh.read())


def _hello(base: str, token: str, *, timeout: float = 30.0) -> dict:
    """Unsigned ``GET <base>/api/plugins/ctx_handover/v1/hello`` with the dashboard token — the first request of a
    dialer (``engine/wire.py`` ``hello(signed=False)``): status, elapsed time, head of the body."""
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


_VERIFIED = re.compile(r"(?<!not )\bverified\b", re.I)


@needs_lab
@pytest.mark.e2e_timeout(1200)
def test_l3_r33_host_isolation_doctor_matches_what_works(lab, ev):
    """L15 / R33a (L3-F21): A (the gateway a paired laptop dials) runs under ``plugins.isolation: host`` (``gw-a`` and
    ``serve-a`` restarted); ``/handover doctor`` in A's gateway, then a pull from B through A's ``listen_url``
    (``serve-a``).

    Soll, each assertion from R33a ("Unter ``plugins.isolation: host`` sind die Plugin-Routen über ``hermes serve``
    erreichbar …; nicht erreichbar ist der Gateway-Listener. ``doctor`` meldet je Weg „verifiziert“ oder „nicht
    verifiziert“ statt FAIL"):

    * doctor, whatever the pull does: a ``plugin isolation`` line that is not ``[FAIL]``, does not claim this gateway
      "cannot receive or provide handovers over HTTP" (the R33 wording R33a corrects), names the ``hermes serve`` way
      in a sentence that calls it verified and the gateway listener in a sentence that calls it not verified (the
      detail before the remedy ``→``);
    * pull ``done``: A's session is locked afterwards (R1);
    * pull refused (the one open case of R33a, "unsigniertes 404 kurz nach serve-Start … mit Wiederholung im Labor zu
      klären"; g5.md §8 point 2): only ``listener_missing`` plus its text is checked — "has no ctx_handover routes",
      without the R33 claim that a host-isolated gateway cannot receive or provide handovers over HTTP — and A's
      session is untouched (R1, D8); ``a/logs/agent.log`` and ``errors.log`` are saved, and an unsigned ``hello`` on
      ``serve-a`` 30 s later must answer 200 with a hello document (the routes are reachable through ``hermes serve``,
      R33a — a second 404 means the refusal is not a cold-start effect but sticks, e.g. a remembered identity error,
      g5.md §0 cause (c)).

    Expected red on ``2f5fd1d5`` (read, not run): ``engine/commands.py:1434-1478`` — ``HOST_ISOLATION_DETAIL`` says
    "this gateway cannot receive or provide handovers over HTTP", and ``_host_isolation_check`` returns ``ok: False``
    (``[FAIL]``) because ``laptop`` is a paired inbound peer; ``engine/surfaces.py`` ``listener_missing`` repeats the
    R33 claim. The doctor assertions come first, so both outcomes of the pull are red there. The wording checked here
    is R33a's; ``feat/ctx-handover`` ``11ead29b`` ("Verified end to end under host: … through `hermes serve` … Not
    verified under host: this gateway's own listener …") meets it. The dialer side under host isolation is E14.

    A's ``config.yaml`` is restored and ``gw-a``/``serve-a`` restarted through ``labctl.py``."""
    src = lab.make_source("a", f"r33proj{lab.run_id}", turns=("host isolated listener",))
    logs = _HomeLogs(lab, "a")
    hello2: dict | None = None
    with lab.preserved("a", restart=("gw-a", "serve-a")):
        lab.config_set("a", "plugins.isolation", "host")
        lab.restart("gw-a", "serve-a")
        doctor, _ = lab.slash("a", lab.new_session("a", lab.work("a")), "/handover doctor", timeout=300)
        ev.write("doctor-a.txt", doctor)
        res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
        ev.cli("take", res)
        final = lab.wait_job("b", res.job_id, _lab.STOP_STATES, timeout=120, poll=0.5) if res.job_id else None
        ev.write("job-b.json", {"final": final, "row": lab.job_row("b", res.job_id) if res.job_id else None})
        marks = lab.marks("a", src.sid)
        refused = not (res.rc == 0 and final == "done")
        if refused:
            logs.save(ev, "refusal")
            time.sleep(30)
            hello2 = _hello(_lab.SERVE_URL["a"], lab.tokens["a"])
            ev.write("hello-after-30s.json", hello2)
            logs.save(ev, "after-second-hello")
    line = next((ln for ln in doctor.splitlines() if "plugin isolation" in ln), "")
    detail = line.split(" → ", 1)[0]
    sentences = [x.strip() for x in re.split(r"(?<=[.;])\s+", detail) if x.strip()]
    ev.write("doctor-line.json", {"line": line, "sentences": sentences, "refused": refused, "hello2": hello2})
    assert line, doctor[-3000:]
    assert not line.startswith("[FAIL]"), line
    assert "cannot receive or provide handovers over HTTP" not in line, line
    assert any(_VERIFIED.search(x) and "hermes serve" in x for x in sentences), sentences
    assert any("not verified" in x.lower() and "listener" in x.lower() for x in sentences), sentences
    if not refused:
        assert marks["ended"] is True, marks
        return
    assert res.error_code == "listener_missing", res.brief()
    assert "has no ctx_handover routes" in res.text, res.text[-1500:]
    assert "cannot receive or provide handovers over HTTP" not in res.text, res.text[-1500:]
    assert marks["ended"] is False, marks
    assert hello2 is not None and hello2["status"] == 200 and "listener_pubkey" in hello2["body"], (
        f"serve-a still refuses the ctx_handover routes 30 s after the refused pull: {hello2}"
    )


# ---------------------------------------------------------------------------------------------------------
# F13 / R35
# ---------------------------------------------------------------------------------------------------------


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_f13_reclaim_after_done_says_the_unreachable_target_copy_stays_active(lab, ev):
    """L3-F13 under R35: after a finished push A→B, B's listener ``serve-b`` is stopped (``labctl.py``) and ``hermes
    handover reclaim <id>`` runs on A; ``serve-b`` is started again afterwards.

    Soll: R35 — reclaim after DONE makes two independent copies; the other side cannot be told, so the reclaim
    output says explicitly that the copy on the target (``laptopgw``) stays active and does not learn of the reclaim
    (wording checked by meaning: "stays/remains/is still … active" and "not told/notified/informed …"/"learns
    nothing"); the session is active here again (DESIGN §7.7); ``--json`` does not claim the target was notified.
    No README check and no target-side notification check (R35). The pull case is E15."""
    src = lab.make_source("a", f"f13proj{lab.run_id}", turns=("reclaim with the target away",))
    pushed = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--yes", timeout=600)
    ev.cli("push", pushed)
    assert pushed.rc == 0 and (lab.job_row("a", pushed.job_id) or {}).get("state") == "done", pushed.brief()
    try:
        lab.stop("serve-b")
        rec = lab.handover("a", "reclaim", src.sid, timeout=180)
        ev.cli("reclaim-target-stopped", rec)
    finally:
        if not lab.running("serve-b"):
            lab.start("serve-b")
    marks = lab.marks("a", src.sid)
    ev.write("marks-a.json", marks)
    text = rec.text
    reclaim = rec.data.get("reclaim") if isinstance(rec.data.get("reclaim"), dict) else {}
    assert rec.rc == 0, rec.brief()
    assert not marks["ended"] and not marks["hidden"], marks
    assert reclaim.get("target_notified") is not True, reclaim
    assert _lab.LAPTOP_GW in text and _STAYS_ACTIVE.search(text), text
    assert _NOT_TOLD.search(text), f"the reclaim output does not say the copy on the target learns nothing: {text}"


# ---------------------------------------------------------------------------------------------------------
# F14, F15, F16
# ---------------------------------------------------------------------------------------------------------


@needs_lab
@pytest.mark.e2e_timeout(420)
def test_l3_f14_busy_take_carries_the_error_code(lab, ev):
    """L3-F14: ``take`` from B while a turn runs on A's session.

    Soll: DESIGN §14.2 / README §Use ("exit codes … 3 busy/conflict; a refusal carries ``ok: false`` and
    ``error.code`` in ``--json``") — exit code 3, ``ok`` false, ``error.code`` ``session_busy`` (E12 asserts the code
    and the text, not the ``--json`` error; the refused ``reclaim`` during a commit is E10's second half)."""
    src = lab.make_source("a", f"f14proj{lab.run_id}", turns=("busy source",))
    slow = f"F14-SLOW-{_lab.uid()}"
    rule = {"id": slow, "when": {"kind": "main", "last_user_contains": slow}, "reply": {"text": "done", "delay": 25}}
    thread = None
    with lab.llm_rules([rule]):
        mark = lab.llm_mark()
        thread, box = lab.turn_bg("a", src.sid, f"please think long {slow}", timeout=120)
        running = _lab.wait_for(lambda: any(slow in r.get("last_user", "") for r in lab.llm_since(mark)), 60)
        assert running, "scaffold: the slow turn never reached the model"
        busy = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=180)
        ev.cli("take-busy", busy)
        thread.join(120)
    ev.write("slow-turn.json", {"error": box.get("error")})
    assert busy.rc == 3, busy.brief()
    assert busy.data.get("ok") is not True, busy.data
    assert busy.error_code == "session_busy", busy.brief()


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_f15_printed_short_id_finds_the_job_in_the_cli(lab, ev):
    """L3-F15: the six-character job id every message prints works in the CLI.

    Soll: README §Use ("``status``, ``cancel``, ``resume``, ``logs`` and ``gc --job`` take the six-character job id
    the messages print"), DESIGN §14.1. ``status <short>`` finds the job (same full id), ``logs <short>`` lists its
    journal, ``cancel <short>`` of the finished job is resolved (its wording is not asserted) — none answers "not
    found". The slash form of ``status`` is in ``test_surfaces.py::test_e2e_take_progress_notice_and_final_message``."""
    src = lab.make_source("a", f"f15proj{lab.run_id}", turns=("short id",))
    jid, _sid_b = _pull(lab, ev, src.sid)
    short = jid[-6:]
    status = lab.handover("b", "status", short, timeout=180)
    ev.cli("status-short", status)
    logs = lab.handover("b", "logs", short, timeout=180)
    ev.cli("logs-short", logs)
    cancel = lab.handover("b", "cancel", short, timeout=180)
    ev.cli("cancel-short", cancel)
    job = (status.data.get("status") or {}).get("job") or {}
    assert status.rc == 0 and job.get("job_id") == jid, status.brief()
    assert logs.rc == 0 and "not found" not in logs.text.lower() and logs.text.strip(), logs.brief()
    assert "not found" not in cancel.text.lower(), cancel.brief()


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_f16_reclaim_does_not_wait_for_a_stopped_target(lab, ev):
    """L3-F16: a finished push A→B, then B's listener is frozen (SIGSTOP of ``serve-b``: connections are accepted,
    nothing answers) and ``hermes handover reclaim <id>`` runs on A.

    Soll: DESIGN §3.4 point 5 / I2 ("The user can always ``/handover reclaim``"), §7.7 (the notice to the target is
    best effort; ``engine/jobs.py`` ``NOTIFY_WAIT_S`` = 5 s) — the reclaim returns within 45 s (the dialer's read
    timeout alone is 60 s; ``abcbd8a7`` did not return within 180 s), exit code 0, and the session is active here
    again. ``serve-b`` gets SIGCONT in any case."""
    src = lab.make_source("a", f"f16proj{lab.run_id}", turns=("reclaim with the target frozen",))
    pushed = lab.handover("a", "to", _lab.LAPTOP_GW, "--session", src.sid, "--yes", timeout=600)
    ev.cli("push", pushed)
    assert pushed.rc == 0, pushed.brief()
    rec = None
    t0 = time.monotonic()
    with _lab.stopped(lab.pid("serve-b")):
        try:
            rec = lab.handover("a", "reclaim", src.sid, timeout=150)
        except subprocess.TimeoutExpired:
            rec = None
        took = time.monotonic() - t0
    if rec is not None:
        ev.cli("reclaim", rec)
    marks = lab.marks("a", src.sid)
    ev.write("reclaim.json", {"took_s": round(took, 1), "marks": marks})
    assert rec is not None, f"reclaim did not return within 150 s ({took:.0f} s)"
    assert took <= 45, f"reclaim took {took:.0f} s with the target frozen"
    assert rec.rc == 0, rec.brief()
    assert not marks["ended"] and not marks["archived"] and not marks["hidden"], marks


# ---------------------------------------------------------------------------------------------------------
# F17 / R32, L17
# ---------------------------------------------------------------------------------------------------------


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_f17_session_over_the_target_window_is_not_confirmed_by_yes(lab, ev):
    """L3-F17: a session of ≈ 160 000 characters (``SessionBuilder``, 20 turns × 4 000) pulled by B with
    ``model.context_length: 4000`` and ``--yes``, without a terminal.

    Soll: R32 — the session is larger than the context window of the model there, so the summary names both sizes
    ("larger than the context window") and asks; ``--yes`` does not confirm it ("--yes does not confirm …", README
    §Use "Nor does it confirm a session larger than the context window there"); without an answer nothing is locked
    (A's session not ended, B's job not ``done``). L24 (without ``--yes``) is in ``test_lab_questions.py``."""
    from ctx_handover.tests.fakes.sessiondb import SessionBuilder

    cwd = lab.ws("a") / f"f17{lab.run_id}"
    lab.make_repo(cwd, dirty=False, ignored=False)  # own repository: the lab lies inside the ctx checkout
    with lab.db("a", write=True) as db:
        b = SessionBuilder(db)
        sid = b.create(title=f"f17 big context {lab.run_id}", model="fake-model", cwd=str(cwd))
        b.turns(sid, 20, size=4000)
    with lab.preserved("b"):
        lab.config_set("b", "model.context_length", "4000")
        res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", timeout=600)
    ev.cli("take-yes", res)
    state = (lab.job_row("b", res.job_id) or {}).get("state") if res.job_id else None
    marks = lab.marks("a", sid)
    ev.write("after.json", {"state": state, "marks": marks})
    assert res.rc != 0, res.brief()
    assert "larger than the context window" in res.text, res.text[-2000:]
    assert "--yes does not confirm" in res.text, res.text[-2000:]
    assert marks["ended"] is False, marks
    assert state != "done", state


@needs_lab
@pytest.mark.e2e_timeout(600)
def test_l3_l17_final_message_and_stored_listing_after_a_pull(lab, ev):
    """L17 (``reports/L3.md``: a never opened import is missing from the default ``session.list``), fallback taken:
    the final message of a pull names title and session id and says how to open it; the stored listing has it.

    Soll: README §Use ("The final message names the session on the other side (title and session id) … A received
    session is stored, not open: it is listed in the Desktop sidebar and in ``hermes sessions list``, and ``hermes
    --resume <session-id>`` opens it"), R14, ``engine/surfaces.py`` ``open_text``; ``status <job>`` names the target
    session (``target_session``); B's gateway ``session.list`` with ``scope: stored`` lists it (PATH.md §5)."""
    src = lab.make_source("a", f"l17proj{lab.run_id}", turns=("open me later",))
    res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
    ev.cli("take", res)
    assert res.rc == 0 and res.job_id, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write("status.json", status)
    sid_b = str(_ack(status).get("session_id") or "")
    title = str(_ack(status).get("title") or src.title)
    stored = lab.gw_rpc("b", "session.list", {"scope": "stored", "limit": 200})
    ev.write("session-list-stored.json", stored)
    target = status["job"].get("target_session") or {}
    assert sid_b and f'"{title}"' in res.text and sid_b in res.text, res.text[-2000:]
    assert f"hermes --resume {sid_b}" in res.text, res.text[-2000:]
    assert target.get("id") == sid_b, target
    assert sid_b in _listed(stored), _listed(stored)[:20]
