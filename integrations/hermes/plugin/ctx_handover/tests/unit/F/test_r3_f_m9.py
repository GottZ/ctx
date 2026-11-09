"""Review round 3, M9 (``REVIEW-2.md`` §4; g7 round 2, ``test_g7_botchat_title.py``): ``README.md:92-93`` said the
canonical *Bot Chat* of Bot Mode "keeps its title" while it is handed over. With Hermes' real ``SessionDB`` the
archive mark (2) releases the Bot Chat name (``title = NULL``) and the plugin hides the session again: during the
handover it has no title. A reclaim or a cancelled handover gives the name back, or — when Bot Mode made a new Bot
Chat meanwhile — brings it back listed as ``Bot Chat (before handover)`` (``lock._bot_chat_elsewhere``).

The README sentence is bound to that behaviour here; ``test_rv_f_readme.py::test_rv_f_bot_chat_exception_of_mark_4``
pinned the wrong wording and now checks the corrected one."""

from __future__ import annotations

from pathlib import Path

from ctx_handover.engine import lock
from ctx_handover.engine.ledger import Ledger
from ctx_handover.tests.unit.B.test_lock_support import JOB, PEER, describe, now

PLUGIN_ROOT = Path(lock.__file__).resolve().parents[1]


def _lock_section() -> str:
    text = " ".join((PLUGIN_ROOT / "README.md").read_text(encoding="utf-8").split())
    return text.split("## What the lock on the source guarantees", 1)[1].split("## Set up", 1)[0]


def test_r3_f_m9_bot_chat_has_no_title_while_handed_over_and_gets_it_back(session_db, make_session, tmp_path):
    """★ Real ``SessionDB`` + real ledger: after ``prepare`` the canonical Bot Chat is untitled, archived and hidden
    (the README says it loses its title); a cancelled handover (``release``) gives the name back.

    Red before the README change: the README said "keeps its title" — the assertion on the README fails."""
    canonical = session_db.CANONICAL_BOT_CHAT_TITLE
    sid = make_session.create(title=canonical)
    make_session.turns(sid, 2)
    session_db.set_session_hidden(sid, True)
    ledger = Ledger(tmp_path / "data" / "ledger.sqlite3")
    try:
        info = describe(session_db, sid)
        holder = lock.acquire(session_db, ledger, info, JOB, ttl_s=120, wait_s=0, cancelled=lambda: False)
        lock.prepare(session_db, ledger, info, JOB, PEER, now())
        row = session_db.get_session(sid)
        assert (row["title"], row["hidden"], row["archived"], row["end_reason"]) == (None, 1, 1, "handed_over")
        lock.release(session_db, ledger, info, holder, JOB)
        row = session_db.get_session(sid)
        assert (row["title"], row["hidden"]) == (canonical, 1)
    finally:
        ledger.close()
    section = _lock_section()
    assert "keeps its title" not in section
    assert "loses its title while it is handed over" in section, section
    assert "archiving releases the Bot Chat name in Hermes" in section
    assert "A reclaim or a cancelled handover gives the name back" in section
    assert "`Bot Chat (before handover)`" in section
