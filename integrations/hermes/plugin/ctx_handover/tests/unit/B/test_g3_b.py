"""Round G‴, package g3-b: lab findings L3-F12 (offer claim, B side) and L3-F10 (terminal-output files).

F12 — ``take --offer <id>`` sent a ``SessionRef`` with only ``offer_id``; ``session_export.describe`` knew ids,
``latest`` and the calling session and answered ``bad_request: no session given``. ``describe`` now takes an
``offer_session`` lookup (A's ledger) and resolves an offer to the session it names (``reports/L2/g3-b-api.md``).

F10 — the terminal tool keeps a head/tail window of a long output and writes the whole output to
``<HERMES_HOME>/cache/terminal-output/out-*.log``; the tool result names it as ``"full_output_path"``
(``tools/environments/base_output.py:172-205``, ``tools/terminal_tool_result.py:169-193``). The kind
``terminal_output`` is discovered on the source and placed under the same area of the target's home.
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

from ctx_handover.engine import hermes_files as hf
from ctx_handover.engine import session_export, session_import
from ctx_handover.engine.errors import BadRequest, NotFound
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import HermesPaths, Options, SessionRef
from ctx_handover.engine.policy import HERMES_FILE_KINDS, PolicyConfig, check_target_dest
from ctx_handover.tests.unit.B.conftest import export, rich_lineage
from ctx_handover.tests.unit.B.support import spill_terminal_output

PEER = "PEER-LAPTOP"


# ---------------------------------------------------------------------------------------------------------
# F12 — the offer branch of ``describe``
# ---------------------------------------------------------------------------------------------------------


@pytest.fixture
def offer_ledger(tmp_path, clock):
    lg = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    try:
        yield lg
    finally:
        lg.close()


def _claiming(ledger: Ledger, job_id: str):
    """The lookup A wires (``g3-b-api.md``): claim the offer for this job and peer, answer its session id."""
    return lambda offer_id: ledger.offer_claim(offer_id, PEER, job_id)["session_id"]


def test_g3b_f12_offer_ref_resolves_to_the_offered_session(make_session, session_db, offer_ledger):
    """★ F12: an offer names the session (its stored id may be the lineage root); ``describe`` resolves it to the tip
    exactly as for ``SessionRef(id=…)`` — before: ``BadRequest("no session given")``."""
    tip = rich_lineage(make_session)
    root = session_db.get_compression_lineage(tip)[0]
    offer_id, job_id = ulid(), ulid()
    offer_ledger.offer_put(offer_id, PEER, root, "from the server", None, offer_ledger.now() + 7200)
    info = session_export.describe(
        session_db,
        SessionRef(offer_id=offer_id),
        calling_session=None,
        offer_session=_claiming(offer_ledger, job_id),
    )
    by_id = session_export.describe(session_db, SessionRef(id=root), calling_session=None)
    assert info == by_id
    assert info.tip_id == tip and info.root_id == root
    # the lookup claimed the offer for this job; a second describe of the same job is idempotent
    assert offer_ledger.offer_get(offer_id)["claimed_job"] == job_id
    again = session_export.describe(
        session_db, SessionRef(offer_id=offer_id), calling_session=None, offer_session=_claiming(offer_ledger, job_id)
    )
    assert again.tip_id == tip


def test_g3b_f12_offer_ref_beats_latest_and_the_calling_session(make_session, session_db):
    """The offer decides over ``latest`` and over the session the command was typed in (precedence id › offer)."""
    b = make_session
    offered = b.create()
    b.turns(offered, 1)
    other = b.create()
    b.turns(other, 1)
    info = session_export.describe(
        session_db,
        SessionRef(latest=True, offer_id="OFFER"),
        calling_session=other,
        offer_session={"OFFER": offered}.get,
    )
    assert info.tip_id == offered


def test_g3b_f12_offer_errors_are_named(make_session, session_db, offer_ledger):
    """An offer that cannot be resolved says so — never the misleading ``no session given``."""
    b = make_session
    sid = b.create()
    b.turns(sid, 1)
    # no lookup wired: a composition-root error, named with the field
    with pytest.raises(BadRequest) as wiring:
        session_export.describe(session_db, SessionRef(offer_id="OFFER"), calling_session=None)
    assert "offer" in str(wiring.value) and "no session given" not in str(wiring.value)
    assert wiring.value.details.get("field") == "session_ref.offer_id"
    # unknown / empty answer of the lookup
    for answer in (None, ""):
        with pytest.raises(NotFound) as missing:
            session_export.describe(
                session_db, SessionRef(offer_id="OFFER"), calling_session=sid, offer_session=lambda _o, a=answer: a
            )
        assert missing.value.details.get("offer_id") == "OFFER"
    # the ledger's own refusal (offer of another peer / expired / claimed by another job) passes through
    offer_id = ulid()
    offer_ledger.offer_put(offer_id, PEER, sid, None, None, offer_ledger.now() + 7200)
    offer_ledger.offer_claim(offer_id, PEER, ulid())
    with pytest.raises(NotFound):
        session_export.describe(
            session_db,
            SessionRef(offer_id=offer_id),
            calling_session=None,
            offer_session=_claiming(offer_ledger, ulid()),
        )
    # an offer whose session is gone
    with pytest.raises(NotFound):
        session_export.describe(
            session_db,
            SessionRef(offer_id="OFFER"),
            calling_session=None,
            offer_session=lambda _o: "20250101_000000_000000",
        )


def test_g3b_f12_id_and_offer_must_name_the_same_session(make_session, session_db):
    """Route #6 answers ``{id, offer_id}``; a request that pairs an offer with another session is refused (the
    offer's stored refinement must never be applied to a session the offer did not name)."""
    tip = rich_lineage(make_session)
    root = session_db.get_compression_lineage(tip)[0]
    b = make_session
    other = b.create()
    b.turns(other, 1)
    same = session_export.describe(
        session_db, SessionRef(id=tip, offer_id="OFFER"), calling_session=None, offer_session={"OFFER": root}.get
    )
    assert same.tip_id == tip
    with pytest.raises(BadRequest) as foreign:
        session_export.describe(
            session_db, SessionRef(id=other, offer_id="OFFER"), calling_session=None, offer_session={"OFFER": root}.get
        )
    assert foreign.value.details.get("field") == "session_ref.offer_id"
    # without a lookup an explicit id keeps working (old callers, contract: additive keyword)
    assert session_export.describe(session_db, SessionRef(id=other, offer_id="OFFER"), calling_session=None).tip_id == (
        other
    )


def test_g3b_f12_fake_describe_has_the_same_offer_branch(tmp_path):
    """The cluster fake (``tests/fakes/session_fake.py``) keeps the keyword and the precedence."""
    from ctx_handover.tests.fakes.session_fake import FakeSession

    fake = FakeSession()
    sid = fake.add_session(title="offered")
    info = fake.describe(None, SessionRef(offer_id="O1"), calling_session=None, offer_session={"O1": sid}.get)
    assert info.tip_id == sid
    with pytest.raises(NotFound):
        fake.describe(None, SessionRef(offer_id="O2"), calling_session=None, offer_session={"O1": sid}.get)
    with pytest.raises(BadRequest):
        fake.describe(None, SessionRef(offer_id="O1"), calling_session=None)


# ---------------------------------------------------------------------------------------------------------
# F10 — terminal-output files
# ---------------------------------------------------------------------------------------------------------


def _cfg(user_home: Path, hermes_home: Path, **kw) -> PolicyConfig:
    return PolicyConfig.from_options(
        Options(**kw),
        home=user_home,
        hermes_home=hermes_home,
        hermes_root=hermes_home,
        plugin_dir=hermes_home / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
        document_cache=hermes_home / "cache" / "documents",
    )


def _paths(home: Path) -> HermesPaths:
    return HermesPaths(home=str(home), document_cache=str(home / "cache" / "documents"))


@pytest.fixture
def terminal_session(make_session, hermes_home, user_home, monkeypatch):
    """A session whose transcript holds a real terminal-tool result with a spilled full output (the core's own
    writer: ``tools.terminal_tool.terminal_tool`` with a 2 000-byte capture window, as upstream's
    ``tests/tools/test_terminal_truncation_spill.py`` does)."""
    result = spill_terminal_output(monkeypatch, "g3b-" + ulid())
    spill = Path(json.loads(result)["full_output_path"])
    assert spill.parent == hermes_home / "cache" / "terminal-output" and spill.is_file()
    b = make_session
    sid = b.create(cwd=str(user_home))
    b.user(sid, "run the long command")
    call = b.tool_call("terminal", {"command": "python3 …"}, call_id="call_term")
    b.assistant(sid, "", tool_calls=[call])
    b.tool(sid, "call_term", result, name="terminal")
    b.assistant(sid, "the output was long; the rest is in the spill file")
    rows = b.db.get_messages(sid, include_inactive=True)
    return {"sid": sid, "rows": rows, "spill": spill, "result": result}


def test_g3b_f10_terminal_output_is_a_session_file_reference(terminal_session, hermes_home):
    """★ F10: the tool result's ``full_output_path`` is a session-file reference of kind ``terminal_output`` (rel =
    the file name in ``cache/terminal-output``) — before: no reference, the file stayed behind."""
    refs = hf.references(terminal_session["rows"], [terminal_session["sid"]], _paths(hermes_home))
    spill = terminal_session["spill"]
    assert {"kind": "terminal_output", "source_path": str(spill), "rel": spill.name, "session_id": None} in refs
    assert hf.TERMINAL_OUTPUT == "terminal_output"
    assert hf.area_root(hf.TERMINAL_OUTPUT, _paths(hermes_home)) == hermes_home / "cache" / "terminal-output"


def test_g3b_f10_capsule_names_the_terminal_output(terminal_session, session_db, store, source_paths):
    """★ F10: the exported capsule's ``hermes_file_refs`` carry the terminal output (the source-side record)."""
    _info, cap = export(session_db, store, terminal_session["sid"], source_paths)
    doc = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    kinds = {r["kind"]: r for r in doc["hermes_file_refs"]}
    assert kinds["terminal_output"]["source_path"] == str(terminal_session["spill"])


def test_g3b_f10_terminal_output_references_stay_in_their_area(hermes_home, tmp_path):
    """Only a single file directly in ``<home>/cache/terminal-output`` counts; JSON escapes are decoded; a path
    elsewhere, a nested path, a relative path or a non-string value is no reference."""
    area = hermes_home / "cache" / "terminal-output"
    good = area / "out-1-2-ab.log"
    elsewhere = tmp_path / "out-1-2-cd.log"
    rows = [
        {"role": "tool", "content": json.dumps({"output": "x", "full_output_path": str(good)})},
        {"role": "tool", "content": json.dumps({"full_output_path": str(elsewhere)})},
        {"role": "tool", "content": json.dumps({"full_output_path": str(area / "sub" / "x.log")})},
        {"role": "tool", "content": json.dumps({"full_output_path": "cache/terminal-output/out-r.log"})},
        {"role": "tool", "content": json.dumps({"full_output_path": 42})},
        {"role": "tool", "content": json.dumps({"full_output_path": str(area / 'out-"q".log')})},
        {"role": "tool", "content": [{"type": "text", "text": json.dumps({"full_output_path": str(area / "o2.log")})}]},
    ]
    refs = hf.references(rows, (), _paths(hermes_home))
    got = sorted(r["source_path"] for r in refs if r["kind"] == "terminal_output")
    assert got == sorted([str(good), str(area / 'out-"q".log'), str(area / "o2.log")])


def test_g3b_f10_kinds_agree_with_k0():
    """★ F10 (contract): the session-file kinds cluster B recognises are exactly K0's ``HERMES_FILE_KINDS`` —
    red until the integrator adds ``terminal_output`` to K0 (``reports/L2/g3-b-k0.diff``)."""
    assert tuple(hf.SESSION_FILE_KINDS) == tuple(HERMES_FILE_KINDS)
    assert hf.TERMINAL_OUTPUT in HERMES_FILE_KINDS


def test_g3b_f10_terminal_output_is_discovered_and_placed(terminal_session, hermes_home, user_home, tmp_path):
    """★ F10 end of cluster B: discovery carries the spill file (regular, in its area, within the size cap); on the
    target it goes to ``<home>/cache/terminal-output/<name>`` — from kind + checked basename only — and the target
    policy allows that destination. Needs K0's kind (``g3-b-k0.diff``)."""
    found = hf.discover(
        terminal_session["rows"], [terminal_session["sid"]], _paths(hermes_home), _cfg(user_home, hermes_home)
    )
    (term,) = [f for f in found if f.kind == hf.TERMINAL_OUTPUT]
    assert term.source_path == str(terminal_session["spill"]) and term.size == terminal_session["spill"].stat().st_size
    target_home = tmp_path / "target-home"
    target_home.mkdir()
    data = terminal_session["spill"].read_bytes()
    dest = hf.placement(
        {"kind": term.kind, "rel": "../../evil/" + term.rel},
        hashlib.sha256(data).hexdigest(),
        _paths(target_home),
        "20260101_000000_abcdef",
    )
    assert dest == target_home / "cache" / "terminal-output" / terminal_session["spill"].name
    verdict = check_target_dest(
        dest,
        _cfg(tmp_path / "target-user", target_home),
        recorded=False,
        local_choice=False,
        hermes_file_kind=hf.TERMINAL_OUTPUT,
    )
    assert verdict.allowed, verdict


def test_g3b_f10_terminal_output_without_k0_kind_is_named_not_dropped(
    terminal_session, hermes_home, user_home, monkeypatch
):
    """Until K0 knows the kind, discovery refuses the file with a reason (``hermes``) instead of failing the whole
    discovery or silently dropping it — the plan and the note name it as not transferred."""
    import ctx_handover.engine.hermes_files as mod

    monkeypatch.setattr(mod, "HERMES_FILE_KINDS", tuple(k for k in HERMES_FILE_KINDS if k != hf.TERMINAL_OUTPUT))
    rejected: list[tuple[str, str]] = []
    found = hf.discover(
        terminal_session["rows"],
        [terminal_session["sid"]],
        _paths(hermes_home),
        _cfg(user_home, hermes_home),
        rejected=rejected,
    )
    assert all(f.kind != hf.TERMINAL_OUTPUT for f in found)
    assert (str(terminal_session["spill"]), "hermes") in rejected


# ---------------------------------------------------------------------------------------------------------
# Success sweep (Gp-common): ``import_sessions`` answers ``ok: True`` for a payload whose ids already exist —
# it skips them (``hermes_state_portability.py:822-838``) — and ``create_session`` is an upsert
# (``hermes_state_sessions.py:418-421``). A planned id taken between planning and writing must neither count as
# imported nor be deleted by the import's cleanup.
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("path", ["A", "B'"])
def test_g3b_sweep_import_never_takes_over_a_session_holding_a_planned_id(
    path, make_session, session_db, target_db, store, source_paths, target_cwd
):
    """★ A session that appeared under a planned id after ``plan_import`` stays untouched (rows, model, existence);
    the import re-mints its ids and lands complete. Before: path A counted the skipped segment as written and the
    failed verification's cleanup deleted the foreign session; path B′ wrote into it."""
    from dataclasses import replace

    from ctx_handover.tests.unit.B.conftest import plan

    tip = rich_lineage(make_session)
    _info, cap = export(session_db, store, tip, source_paths)
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    p = plan(target_db, capsule, target_cwd)
    if path == "B'":
        p = replace(p, segments=tuple(replace(s, path="B'") for s in p.segments))
    foreign = p.segments[0].new_id
    target_db.create_session(foreign, "cli", model="someone-elses-model")
    target_db.append_message(foreign, "user", "a conversation that is not part of the handover")
    before = [(m["role"], m["content"]) for m in target_db.get_messages(foreign, include_inactive=True)]
    prepared = session_import.import_hidden(target_db, store, cap.capsule_id, p)
    row = target_db.get_session(foreign)
    assert row is not None, "the import's cleanup deleted a session it did not create"
    assert row["model"] == "someone-elses-model" and not row["hidden"]
    assert [(m["role"], m["content"]) for m in target_db.get_messages(foreign, include_inactive=True)] == before
    assert foreign not in (prepared.root_id, prepared.tip_id)
    assert foreign not in target_db.get_compression_lineage(prepared.tip_id)


# ---------------------------------------------------------------------------------------------------------
# L17 — where a never-opened import is listed (characterisation; no plugin defect, see reports/L2/g3-b.md)
# ---------------------------------------------------------------------------------------------------------


def test_g3b_l17_committed_import_is_in_the_stored_listing(
    make_session, session_db, target_db, store, source_paths, target_cwd
):
    """The lab's L17 asked the gateway's DEFAULT ``session.list`` (scope ``live``): it lists only sessions the running
    gateway holds in memory (``gateway/session_controls.py:611-629``, ``gateway/session_live_listing.py:93-115``),
    so an import nobody opened yet is never there. The stored listings — gateway ``session.list`` with
    ``scope: "stored"`` (``gateway/session_listing.py:50-67``) and the Desktop sidebar's REST ``/api/sessions``
    (``apps/desktop/src/api/sessions.ts:104-118`` → ``hermes_cli/web_routers/sessions.py:182-226``) — read
    ``list_sessions_rich``; after the commit's unhide the imported tip is there, with a source the stored picker
    treats as local (resumable), never before the unhide."""
    from gateway.session_listing import _LOCAL_SOURCES
    from hermes_state_sessions import INTERNAL_LISTING_SOURCES

    from ctx_handover.tests.unit.B.conftest import roundtrip

    tip = rich_lineage(make_session)
    *_, prepared = roundtrip(session_db, target_db, store, tip, source_paths, target_cwd)

    def stored() -> set[str]:
        picker = target_db.list_sessions_rich(  # the gateway's stored picker query
            exclude_sources=list(INTERNAL_LISTING_SOURCES),
            limit=200,
            offset=0,
            order_by_last_active=True,
            compact_rows=True,
        )
        sidebar = target_db.list_sessions_rich(  # GET /api/sessions?limit=40&archived=exclude&order=recent
            limit=40,
            offset=0,
            order_by_last_active=True,
            compact_rows=True,
            include_pinned=True,
            min_message_count=0,
            include_archived=False,
            archived_only=False,
        )
        return {r["id"] for r in picker} & {r["id"] for r in sidebar}

    assert prepared.tip_id not in stored()
    session_import.unhide(target_db, prepared)
    assert prepared.tip_id in stored()
    assert target_db.get_session(prepared.tip_id)["source"] in _LOCAL_SOURCES
