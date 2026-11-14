"""A message typed into the session while it is being handed over (TESTPLAN §3
``test_int_typed_message_during_handover_becomes_delta``; DESIGN §6.2 delta, §6.8 delta append, §7.6, §11.3;
[SK H2], R3).

TUI and Desktop write the user row at submit time, before any lease (``tui_gateway/session_workdir.py``
``_write_submit_user_row``) — so a message typed during the handover lands in the source transcript although the
handover holds the turn lease. The test types it after the capsule was exported and the target prepared, right
before the source's prepare re-check (the conductor's ``TARGET_PREPARED`` step). Expected: the source answers
``delta``, the target appends the row to the hidden import and re-verifies, the target's note says the message is
waiting there, and on the source the queued turn of that message runs into the veto and becomes the withheld
placeholder."""

from __future__ import annotations

from ctx_handover.engine import conductor
from ctx_handover.engine.model import Mode, Options, PlanHints, SessionRef
from ctx_handover.tests.integration._int2_helpers import (
    HookHost,
    ack_ids,
    conductor_ctx,
    events_of,
    outcome,
    typed_submit,
    withhold_like_core,
)
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks, wait_for

TYPED = "one more thing: also bump the version in setup.cfg"
DELTA_NOTE_LINE = "Your last message, sent during the handover, is waiting here."


def test_int_typed_message_during_handover_becomes_delta(online, monkeypatch):
    """★ ``append_message`` during the lock (TUI submit pattern) ⇒ delta at the target, placeholder at the source.

    Mutant: the source's prepare re-check ignores the grown transcript (``local_source._prepare`` never takes the
    ``DELTA`` branch) — the target commits without the typed row."""
    server, laptop, _listener, peer = online
    b = builder(laptop)
    sid = b.create(title="Typing during handover", cwd=str(laptop.home))
    b.turns(sid, 5, with_tools=True)

    typed: dict = {}
    original = conductor.Conductor._target_prepared

    def type_while_locked(self):
        if not typed:
            typed["row_id"] = typed_submit(laptop.db(), sid, TYPED)
        return original(self)

    monkeypatch.setattr(conductor.Conductor, "_target_prepared", type_while_locked)
    opts = Options(refine="off", yes=True, hints=PlanHints(no_workspace=True), stall_timeout_s=60)
    job_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=opts)
    assert typed, "the handover never reached TARGET_PREPARED"
    assert rc == 0, outcome(laptop, job_id)

    # The source answered ``delta`` once and the conductor applied it.
    ctx = conductor_ctx(laptop, job_id)
    assert ctx.get("deltas") == 1, ctx.get("deltas")
    states = [e.get("ctx", {}) for e in events_of(laptop, job_id, "transition")]
    assert any("delta" in s for s in states), "no DELTA_APPLYING transition journaled"

    # Target: the typed row is the last live row of the new tip, the lineage verifies against the grown source.
    src_db, dst_db = laptop.db(), server.db()
    _root, new_tip = ack_ids(laptop, job_id)
    assert new_tip
    src_row = next(r for r in src_db.get_messages(sid, include_inactive=True) if r["id"] == typed["row_id"])
    last = dst_db.get_messages(new_tip)[-1]
    assert (last["role"], last["content"]) == ("user", TYPED)
    assert last.get("message_uid") == src_row.get("message_uid")
    assert lineage_hashes(src_db, sid) == lineage_hashes(dst_db, new_tip)
    assert not session_marks(dst_db, new_tip)["hidden"]

    # Target note: stored with the arrival and offered to the first turn there (DESIGN §11.3/§11.4).
    notes = [r["note"] for r in server.ledger().arrival_rows() if r["session_id"] == new_tip]
    assert notes and DELTA_NOTE_LINE in notes[0], notes
    with server.active():
        target_hooks = HookHost.registered().hooks
        offered: dict = {}
        assert wait_for(
            lambda: (
                offered.update(target_hooks["pre_llm_call"](session_id=new_tip, turn_id="t1") or {}) or bool(offered)
            ),
            timeout=15,
        )
    assert DELTA_NOTE_LINE in offered.get("context", ""), offered

    # Source: locked; the typed message's queued turn runs into the veto and becomes the withheld placeholder.
    marks = session_marks(src_db, sid)
    assert marks["ended"] and marks["archived"] and marks["hidden"], marks
    with laptop.active():
        source_hooks = HookHost.registered().hooks
        block: dict = {}
        assert wait_for(
            lambda: block.update(source_hooks["pre_llm_call"](session_id=sid, turn_id="t2") or {}) or bool(block),
            timeout=15,
        )
    assert block.get("action") == "block" and block.get("message"), block
    withhold_like_core(src_db, sid, typed["row_id"], TYPED, block["message"])
    placeholder = next(r for r in src_db.get_messages(sid, include_inactive=True) if r["id"] == typed["row_id"])
    assert placeholder["content"] != TYPED
    assert (placeholder.get("display_metadata") or {}).get("withheld_prompt") == TYPED
    assert dst_db.get_messages(new_tip)[-1]["content"] == TYPED  # the target keeps the message itself
