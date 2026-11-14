# ruff: noqa: F811  (pytest fixtures imported from test_lock_support are redefined as parameters)
"""Veto placeholders that a compaction moved after the retire: reclaim on rotated and in-place compacted
lineages, and the second handover of such a lineage (DESIGN §7.6, §7.7).

How the state arises on the core's own paths: a resume of the archived session reopens its tip
(``hermes_cli/cli_agent_setup_mixin.py:577-580``); every turn runs idle/preflight compaction BEFORE the
``pre_llm_call`` hook (``agent/turn_context.py:4-5``, ``agent/turn_context_compaction.py:4``), so a session near its
threshold is compacted (rotation or, by default, in place) and only then vetoed. Vetoed turns run through the real
agent loop (``vetoed_turn``); the compactions are the compressor's commit paths (``SessionBuilder.rotate`` /
``compact_in_place``).

What the core allows afterwards: no transcript write on a segment closed by compression
(``hermes_state_messages.py:268-269``, ``CompressionSessionClosedError``), a rewind only from a live ``user`` row
(``hermes_state_mutation_transcript.py:14-28``), archived rows (``compacted=1``) are never touched by a rewind, and
an older copy disappears from the display when its newest copy is rewound (``hermes_state_messages.py:1146-1149``).
"""

from __future__ import annotations

import pytest

from ctx_handover.engine import lock, session_export, session_import
from ctx_handover.tests.unit.B.conftest import SOURCE_META, assert_lineage_equal, export, plan
from ctx_handover.tests.unit.B.test_lock_support import (  # noqa: F401 (fixtures)
    JOB,
    ack,
    build,
    chat_requests,
    index_for,
    ledger,
    prepared,
    provider_url,
    vetoed_turn,
)

PLACEHOLDER = "[Message withheld: a pre_llm_call policy hook blocked it.]"


def _compacted_after_retire(session_db, make_session, ledger, provider_url, tmp_path, mode: str):
    """Retire, resume on the source, two vetoed turns, a compaction carrying the last row, two more vetoed turns.
    Returns ``(info, current tip)``."""
    tip = build(make_session)
    info, holder = prepared(session_db, ledger, tip)
    lock.retire(session_db, ledger, info, holder, JOB, ack(), [])
    index = index_for(tmp_path, ledger.path)
    session_db.reopen_session(tip)
    for i in range(2):
        vetoed_turn(session_db, tip, provider_url, index, f"typed {i}")
    if mode == "rotation":
        current = make_session.rotate(tip, keep_last=1)
        index.on_compaction(current, tip)
    else:
        make_session.compact_in_place(tip, keep_last=1)
        current = tip
    for i in range(2, 4):
        vetoed_turn(session_db, current, provider_url, index, f"typed {i}")
    index.refresh_now()
    index.stop()
    assert chat_requests() == []
    return info, current


def _model(db, tip):
    return db.get_resume_conversations(tip)[0]


@pytest.mark.parametrize("mode", ["rotation", "in_place"])
def test_reclaim_after_compaction_keeps_summary_and_returns_every_text(
    session_db, make_session, ledger, provider_url, tmp_path, mode
):
    """★ After a compaction on the source the trailing vetoed attempts are rewound, the compaction summary stays
    in the model context, every typed text comes back (also the ones the compaction moved out of reach) and the
    rest is reported, never silently left: ``kept`` = 2 with a warning.

    Mutant: ``_placeholder_run`` accepts any user row before a veto reply (the v1 rule) ⇒ the run swallows the
    summary row and the model context is empty.
    """
    _info, current = _compacted_after_retire(session_db, make_session, ledger, provider_url, tmp_path, mode)
    before = _model(session_db, current)
    assert before[0].get("_compressed_summary")
    out = lock.reclaim(session_db, ledger, current, force=False)
    assert out["state"] == "reclaimed"
    assert out["rewound"] == 4
    assert out["withheld"] == ["typed 0", "typed 1", "typed 2", "typed 3"]
    assert out["kept"] == 2
    assert any("2 blocked message(s) stay" in w for w in out["warnings"])
    after = _model(session_db, current)
    assert after and after[0].get("_compressed_summary") and after[0]["content"] == before[0]["content"]
    assert not any(m.get("content") == PLACEHOLDER for m in after)
    row = session_db.get_session(current)
    assert (row["archived"], row["hidden"]) == (0, 0)


def test_reclaim_never_writes_a_segment_closed_by_compression(
    session_db, make_session, ledger, provider_url, tmp_path, monkeypatch
):
    """The rewind goes to the current tip only: the ancestor (closed by compression, where the core refuses every
    transcript write) is read, never written, and no core refusal reaches the warnings."""
    info, current = _compacted_after_retire(session_db, make_session, ledger, provider_url, tmp_path, "rotation")
    ancestor = info.tip_id
    assert session_db.get_session(ancestor)["end_reason"] == "compression"
    before = session_db.get_messages(ancestor, include_inactive=True)
    targets: list[str] = []
    real = session_db.rewind_to_message

    def spy(session_id, message_id, **kw):
        targets.append(session_id)
        return real(session_id, message_id, **kw)

    monkeypatch.setattr(session_db, "rewind_to_message", spy)
    out = lock.reclaim(session_db, ledger, current, force=False)
    assert targets == [current]
    assert session_db.get_messages(ancestor, include_inactive=True) == before
    assert not any("not rewound" in w for w in out["warnings"])


def test_reclaim_rewinds_the_late_delta_row_placeholder(session_db, make_session, ledger, provider_url, tmp_path):
    """A message typed during the handover (written at submit time, before p1; carried to the target as a delta)
    becomes the withheld placeholder when its queued turn runs into the veto after retire — the core rewrites the
    submit-time row in place (``agent/turn_context.py:885-893``: ``set_user_message_content``) and appends the
    veto reply. Reclaim rewinds that pair too and returns the text.

    Mutant: candidacy limited to rows above the p1 mark (the v1 rule) ⇒ the pair stays, ``withheld == []``.
    """
    tip = build(make_session)
    info, holder = lock_and_late_row(session_db, ledger, make_session, tip)
    lock.prepare(session_db, ledger, info, JOB, "laptop", _now())
    lock.retire(session_db, ledger, info, holder, JOB, ack(), [])
    entry = ledger.source_state(JOB)
    late = session_db.get_messages(tip)[-1]
    assert late["content"] == "typed during the handover"
    session_db.set_user_message_content(tip, int(late["id"]), PLACEHOLDER, {"withheld_prompt": late["content"]})
    session_db.append_message(tip, "assistant", lock.veto_texts({**entry, "job_id": JOB}))
    out = lock.reclaim(session_db, ledger, tip, force=False)
    assert out["withheld"] == ["typed during the handover"]
    assert out["rewound"] == 2 and out["kept"] == 0
    assert not any(m.get("content") == PLACEHOLDER for m in session_db.get_messages(tip))


def lock_and_late_row(db, ledger, make_session, tip):
    from ctx_handover.tests.unit.B.test_lock_support import locked

    make_session.user(tip, "typed during the handover")
    return locked(db, ledger, tip)


def _now():
    from ctx_handover.tests.unit.B.test_lock_support import now

    return now()


def test_lineage_with_kept_placeholders_transfers_byte_true(
    session_db, make_session, ledger, provider_url, tmp_path, target_db, store, source_paths, target_cwd
):
    """The next handover of a reclaimed lineage that still holds placeholders on its ancestor (and a veto reply
    behind the summary) needs no special case: every row, model and display history and H_all/H_rows/H_model
    arrive equal — nothing to refuse at planning or export."""
    _info, current = _compacted_after_retire(session_db, make_session, ledger, provider_url, tmp_path, "rotation")
    lock.reclaim(session_db, ledger, current, force=False)
    info, cap = export(session_db, store, current, source_paths, job_id="01JSECONDJOBAAAAAAAAAAAAAA")
    capsule = session_export.read_capsule(store, cap.capsule_id, max_json_bytes=64 * 1024**2)
    p = plan(target_db, capsule, target_cwd)
    prepared_ = session_import.import_hidden(
        target_db, store, cap.capsule_id, p, local_hermes_version=SOURCE_META["hermes_version"]
    )
    assert (prepared_.h_all, prepared_.h_rows, prepared_.h_model) == (cap.h_all, cap.h_rows, cap.h_model)
    assert_lineage_equal(session_db, target_db, list(info.lineage_ids), [s.new_id for s in p.segments])
    ancestor_rows = target_db.get_messages(p.segments[1].new_id)
    assert sum(1 for m in ancestor_rows if m.get("content") == PLACEHOLDER) == 2
