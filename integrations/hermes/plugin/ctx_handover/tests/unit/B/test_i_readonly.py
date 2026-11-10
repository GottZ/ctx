"""Layer I (integrator decision 6): the README's "read-only SQL on ``session_admissions`` and
``async_delegations``" (§Disclosure, Reads) holds for the busy probe — every connection it opens on ``state.db``
refuses a write (``lock._ro_connect``, ``?mode=ro``)."""

from __future__ import annotations

import sqlite3

from ctx_handover.engine import lock
from ctx_handover.tests.unit.B.test_lock_support import build, describe


def test_probe_busy_opens_state_db_read_only(session_db, make_session, hermes_home, monkeypatch):
    """★ ``lock.probe_busy`` reads ``state.db`` over its own best-effort connection. Each connection it opens there
    is tried with a write (``CREATE TABLE``) before the probe uses it: the write must fail as a read-only
    database, the probe still answers (not busy, no warning), and ``state.db`` carries no trace of the attempt.

    Mutant (``?mode=ro`` removed from ``lock._ro_connect``): the write succeeds.
    """
    tip = build(make_session, drivers=False)
    state_db = hermes_home / "state.db"
    info = describe(session_db, tip)
    real_connect = sqlite3.connect
    attempts: list[str] = []

    def spy(database, *args, **kwargs):
        conn = real_connect(database, *args, **kwargs)
        if "state.db" in str(database):
            try:
                conn.execute("CREATE TABLE ctxh_i_readonly_probe (x)")
                attempts.append("written")
            except sqlite3.OperationalError as exc:
                attempts.append(str(exc))
        return conn

    with monkeypatch.context() as m:
        m.setattr(lock.sqlite3, "connect", spy)
        report = lock.probe_busy(session_db, state_db, info, hermes_home, "agent:main:local:dm:" + tip)

    assert attempts, "probe_busy opened no connection on state.db"
    assert all("readonly" in a for a in attempts), attempts
    assert report.busy is False and report.warnings == (), report
    check = real_connect(state_db)
    try:
        names = {r[0] for r in check.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    finally:
        check.close()
    assert "ctxh_i_readonly_probe" not in names
    assert {"session_admissions", "async_delegations"} <= names
