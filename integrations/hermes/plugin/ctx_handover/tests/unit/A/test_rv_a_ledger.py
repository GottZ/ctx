"""Review round rv-a, M11 (``review-code/REVIEW.md`` §2): the ledger set ``PRAGMA journal_mode=WAL`` raw. On a
``HERMES_HOME`` on NFS/SMB/WSL1 (``locking protocol``), some FUSE mounts (``not authorized``) or ZFS under bursts
(``disk I/O error``) that pragma fails and the plugin was unusable; Hermes' own rule (``hermes_state_wal``,
``plugins.plugin_storage.plugin_db``) falls back to ``DELETE``. The ledger follows that rule, and the composition
root can hand it Hermes' own journal setter."""

from __future__ import annotations

import sqlite3

import pytest

from ctx_handover.engine import ledger as ledger_mod
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Role


class _NoWal:
    """A real connection whose ``PRAGMA journal_mode=WAL`` fails like it does on a network file system."""

    def __init__(self, conn: sqlite3.Connection, message: str) -> None:
        object.__setattr__(self, "_c", conn)
        object.__setattr__(self, "_m", message)

    def execute(self, sql, *args):
        if sql.replace(" ", "").lower() == "pragmajournal_mode=wal":
            raise sqlite3.OperationalError(self._m)
        return self._c.execute(sql, *args)

    def __getattr__(self, name):
        return getattr(self._c, name)

    def __setattr__(self, name, value):
        setattr(self._c, name, value)


@pytest.mark.parametrize("message", ["locking protocol", "disk I/O error", "not authorized"])
def test_rv_a_m11_ledger_works_where_wal_is_refused(tmp_path, monkeypatch, clock, message):
    real = sqlite3.connect
    monkeypatch.setattr(ledger_mod.sqlite3, "connect", lambda *a, **kw: _NoWal(real(*a, **kw), message))

    led = Ledger(tmp_path / "nfs" / "ledger.sqlite3", clock=clock)
    led.create_job("01JZJ000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {"mode": "push"})

    assert led.job("01JZJ000000000000000000000", Role.CONDUCTOR)["state"] == "created"
    assert led._conn().execute("PRAGMA journal_mode").fetchone()[0].lower() == "delete"
    led.close()


def test_rv_a_m11_other_errors_of_the_pragma_still_raise(tmp_path, monkeypatch, clock):
    """Only the known incompatibilities fall back; a locked database (another opener) is not one."""
    real = sqlite3.connect
    monkeypatch.setattr(ledger_mod.sqlite3, "connect", lambda *a, **kw: _NoWal(real(*a, **kw), "database is locked"))
    with pytest.raises(sqlite3.OperationalError):
        Ledger(tmp_path / "x" / "ledger.sqlite3", clock=clock)


def test_rv_a_m11_hermes_journal_rule_is_used_when_given(tmp_path, clock):
    """The composition root passes Hermes' setter (through ``hostcompat``); the ledger calls it instead of its own."""
    seen: list[str] = []

    def hermes_rule(conn: sqlite3.Connection, *, db_label: str) -> None:
        seen.append(db_label)
        conn.execute("PRAGMA journal_mode=DELETE")

    led = Ledger(tmp_path / "h" / "ledger.sqlite3", clock=clock, journal=hermes_rule)
    assert seen == ["plugin-data/ctx_handover/ledger.sqlite3"]
    assert led._conn().execute("PRAGMA journal_mode").fetchone()[0].lower() == "delete"
    led.close()


def test_rv_a_m11_a_normal_file_system_keeps_wal(tmp_path, clock):
    led = Ledger(tmp_path / "ok" / "ledger.sqlite3", clock=clock)
    assert led._conn().execute("PRAGMA journal_mode").fetchone()[0].lower() == "wal"
    led.close()
