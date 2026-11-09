"""Layer-2 ledger additions (contract X08, X10, X13, R28): veto rows with every column the hook texts read,
direction-bound map lookup, object index removal for the manifest GC, the job's object scope, staging quota,
and the cross-process interval claim of the janitor's GC."""

from __future__ import annotations

import threading

import pytest

from ctx_handover.engine import lock
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import MapRow, Mode, ObjectManifest, Role, SessionInfo


@pytest.fixture
def led(tmp_path, clock):
    lg = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    yield lg
    lg.close()


def _info(root="r1", tip="t1", title="My Work") -> SessionInfo:
    return SessionInfo(
        tip_id=tip,
        root_id=root,
        lineage_ids=(root, tip),
        title=title,
        model=None,
        provider=None,
        cwd="/w",
        git_repo_root=None,
        git_branch=None,
        active_messages=3,
        revision=(1, 2, 3),
    )


def _manifest(n: int) -> ObjectManifest:
    h = f"{n:064x}"
    return ObjectManifest(sha256=h, size=n, block_size=524288, blocks=(), media="application/json", object_id=h)


@pytest.mark.parametrize(
    ("state", "fields"),
    [
        ("retired", {"prepared_at": 1_700_000_000.0, "ack": {"committed_at": 1_700_000_100.0, "title": "There"}}),
        ("exported", {"prepared_at": 1_700_000_200.0, "bundle_path": "/b/x.ctxh.tar"}),
        ("prepared", {"prepared_at": 1_700_000_300.0}),
    ],
)
def test_veto_rows_give_the_texts_reclaim_looks_for(led, state, fields):
    """★ X08: the veto text a hook shows (built by ``lock.VetoIndex`` from ``veto_rows``) is one of the texts
    ``reclaim`` searches placeholders for (``lock._all_texts`` over ``source_by_session``); otherwise reclaim
    finds no placeholder to rewind.

    Mutant: ``veto_rows`` without ``prepared_at``/``ack`` (the layer-1 column set) → the retired text says "an
    unknown time" without the title, the exported one has no date: not in the reclaim set."""
    led.source_lock("01JZJ100000000000000000000", _info(), "holder", "server")
    led.source_set("01JZJ100000000000000000000", state, **fields)
    led.veto_add("01JZJ100000000000000000000", ["r1", "t1"], "r1")
    index = lock.VetoIndex(led.path, ledger_factory=lambda _path: led)
    assert index.refresh_now(force=True)
    shown = index.lookup("t1")["message"]
    assert shown in lock._all_texts(led.source_by_session("t1"))
    rows = {r["session_id"]: r for r in led.veto_rows()}
    row = rows["t1"]
    assert set(row) >= {
        "session_id",
        "root_id",
        "job_id",
        "state",
        "peer",
        "title",
        "at",
        "prepared_at",
        "ack",
        "bundle_path",
        "updated_at",
    }
    assert (
        row["job_id"] == row["job"] == "01JZJ100000000000000000000" and row["root_id"] == "r1" and row["state"] == state
    )
    if state == "retired":
        assert "There" in shown and "unknown time" not in shown


def test_map_lookup_binds_direction(led):
    """X10: ``map_lookup(peer, path, direction)`` returns only rows of that direction (pathmap's lookup)."""
    led.map_put(MapRow("/home/u/app", "P1", "/srv/app", "out", None, None, "01JZJ100000000000000000000"))
    led.map_put(MapRow("/home/u/app-in", "P1", "/srv/app", "in", None, None, "01JZJ200000000000000000000"))
    assert led.map_lookup("P1", "/srv/app", "out").local_path == "/home/u/app"
    assert led.map_lookup("P1", "/srv/app", "in").local_path == "/home/u/app-in"
    assert led.map_lookup("P1", "/srv/app").local_path == "/home/u/app"  # out before in
    assert led.map_lookup("P2", "/srv/app", "out") is None


def test_job_objects_is_the_jobs_own_scope(led):
    """★ X13: ``job_objects`` is exactly what one job registered (a set) — a pull peer gets this scope from
    the server; another job's object is not in it.

    Mutant: ``job_objects`` without ``WHERE job_id=?`` → J2's object is in J1's scope."""
    for jid in ("01JZJ100000000000000000000", "01JZJ200000000000000000000"):
        led.create_job(jid, Role.SOURCE, Mode.PULL, None, {})
    led.job_object("01JZJ100000000000000000000", "a" * 64)
    led.job_objects_add("01JZJ100000000000000000000", ["b" * 64, "a" * 64])
    led.job_object("01JZJ200000000000000000000", "c" * 64)
    assert led.job_objects("01JZJ100000000000000000000") == {"a" * 64, "b" * 64}
    assert led.job_objects("01JZJ200000000000000000000") == {"c" * 64}
    assert led.job_objects("01JZJ300000000000000000000") == set()


def test_object_delete_joins_the_open_transaction(led):
    """``object_delete`` removes index rows and runs inside the caller's ``tx()`` (the manifest GC removes files and
    rows in one write transaction): a rollback keeps the rows."""
    for n in (1, 2, 3):
        led.object_put(_manifest(n))
    with pytest.raises(RuntimeError), led.tx():
        led.object_delete([_manifest(1).object_id])
        raise RuntimeError("abort the GC batch")
    assert led.object_get(_manifest(1).object_id) is not None
    with led.tx():
        led.object_delete([_manifest(1).object_id, _manifest(2).object_id])
    assert led.object_get(_manifest(1).object_id) is None and led.object_get(_manifest(2).object_id) is None
    assert led.object_get(_manifest(3).object_id) is not None
    led.object_delete([])  # nothing to do


def test_quota_charge_never_exceeds_the_confirmed_plan(led):
    """R28 (ledger part): without a quota row nothing is charged; raising keeps what was charged; concurrent
    charges from several threads (own connections) never pass the quota."""
    assert led.quota_charge("01JZJ000000000000000000000", Role.TARGET, 10) == (False, 0, -1)
    led.quota_set("01JZJ000000000000000000000", Role.TARGET, 1000)
    assert led.quota_charge("01JZJ000000000000000000000", Role.TARGET, 600) == (True, 600, 1000)
    led.quota_set("01JZJ000000000000000000000", Role.TARGET, 500)  # never lowers
    assert led.quota_get("01JZJ000000000000000000000", Role.TARGET) == (1000, 600)
    results: list[bool] = []
    barrier = threading.Barrier(8)

    def charge():
        barrier.wait(10)
        results.append(led.quota_charge("01JZJ000000000000000000000", Role.TARGET, 100)[0])

    threads = [threading.Thread(target=charge) for _ in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(30)
    assert results.count(True) == 4 and results.count(False) == 4
    assert led.quota_get("01JZJ000000000000000000000", Role.TARGET) == (1000, 1000)


def test_claim_interval_once_per_interval_across_connections(tmp_path, clock):
    """The janitor's GC claim: one winner per interval for all connections (processes) of the ledger; a clock that
    moved back claims again."""
    a = Ledger(tmp_path / "l.sqlite3", clock=clock)
    b = Ledger(tmp_path / "l.sqlite3", clock=clock)
    now = clock()
    assert a.claim_interval("gc", now, 3600) is True
    assert b.claim_interval("gc", now + 10, 3600) is False
    assert a.claim_interval("gc", now + 3599, 3600) is False
    assert b.claim_interval("gc", now + 3600, 3600) is True
    assert a.claim_interval("gc", now - 100, 3600) is True  # clock moved back
    a.meta_set("gc", "garbage")
    assert b.claim_interval("gc", now, 3600) is True
    a.close()
    b.close()
