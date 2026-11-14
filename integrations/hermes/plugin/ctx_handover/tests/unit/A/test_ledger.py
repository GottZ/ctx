"""Ledger unit tests (cluster A; TESTPLAN §2.2, BUILD-ORDER §6 items 1, 4, 20, 21).

Concurrency between processes lives in ``test_ledger_procs.py``; this file covers one process.
"""

from __future__ import annotations

import os
import sqlite3
import stat

import pytest

from ctx_handover.engine import ledger as ledger_mod
from ctx_handover.engine.errors import (
    AlreadyHandingOver,
    ConflictError,
    InvalidState,
    JobConflict,
    NotFound,
)
from ctx_handover.engine.ledger import Fenced, Ledger
from ctx_handover.engine.model import (
    GateAuth,
    GateMode,
    MapRow,
    Mode,
    ObjectManifest,
    OpState,
    Peer,
    Right,
    Role,
    SessionInfo,
)


@pytest.fixture
def led(tmp_path, clock):
    path = tmp_path / "data" / "ledger.sqlite3"
    lg = Ledger(path, clock=clock)
    yield lg
    lg.close()


def _info(root="r1", tip="t1", lineage=("r1", "t1")) -> SessionInfo:
    return SessionInfo(
        tip_id=tip,
        root_id=root,
        lineage_ids=tuple(lineage),
        title="T",
        model=None,
        provider=None,
        cwd="/w",
        git_repo_root=None,
        git_branch=None,
        active_messages=3,
        revision=(1, 2, 3),
    )


def _peer(name="laptop", peer_id="P1", direction="inbound", rights=(Right.PUSH, Right.PULL)) -> Peer:
    return Peer(
        name=name,
        peer_id=peer_id,
        direction=direction,
        url=None if direction == "inbound" else "https://srv.example",
        pubkey="pk-" + peer_id,
        rights=frozenset(rights),
        gate=None if direction == "inbound" else GateAuth(mode=GateMode.SESSION_TOKEN),
    )


# ---------------------------------------------------------------------------------------------------------
# schema, files, pragmas
# ---------------------------------------------------------------------------------------------------------


def test_ledger_schema_create_and_reopen(tmp_path, clock):
    """Idempotent schema, file 0600, parent 0700, secure_delete and WAL on (SEC 6.1, 6.2)."""
    path = tmp_path / "pd" / "ledger.sqlite3"
    lg = Ledger(path, clock=clock)
    iid = lg.instance_id()
    with lg.tx() as conn:
        assert conn.execute("PRAGMA secure_delete").fetchone()[0] == 1
        assert conn.execute("PRAGMA foreign_keys").fetchone()[0] == 1
        assert conn.execute("PRAGMA journal_mode").fetchone()[0] == "wal"
        tables = {r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    expected = {
        "meta",
        "peers",
        "pair_codes",
        "nonces",
        "jobs",
        "job_events",
        "ops",
        "source_handovers",
        "veto_ids",
        "backups",
        "arrivals",
        "bundle_imports",
        "workspace_map",
        "blocks",
        "objects",
        "job_objects",
        "suggestions",
        "offers",
        "pull_log",
        "ctx_outbox",
    }
    assert expected <= tables
    lg.create_job("01JZJ100000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {"a": 1})
    lg.close()
    for p in [path, *(path.with_name(path.name + s) for s in ("-wal", "-shm"))]:
        if p.exists():
            assert stat.S_IMODE(p.stat().st_mode) == 0o600, p
    assert stat.S_IMODE(path.parent.stat().st_mode) == 0o700

    lg2 = Ledger(path, clock=clock)
    assert lg2.instance_id() == iid
    assert lg2.schema_version == ledger_mod.SCHEMA_VERSION
    assert lg2.job("01JZJ100000000000000000000", Role.CONDUCTOR)["state"] == "created"
    assert not lg2.readonly
    lg2.close()


def test_ledger_newer_schema_refuses_write(tmp_path, clock):
    """A ledger written by a newer plugin opens read-only ("downgrade detected", INTERFACES §7)."""
    path = tmp_path / "ledger.sqlite3"
    Ledger(path, clock=clock).close()
    raw = sqlite3.connect(path)
    raw.execute("UPDATE meta SET value='99' WHERE key='schema_version'")
    raw.commit()
    raw.close()
    lg = Ledger(path, clock=clock)
    assert lg.readonly
    assert lg.schema_version == 99
    assert lg.jobs() == []
    with pytest.raises(InvalidState) as exc:
        lg.create_job("01JZJ000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {})
    assert exc.value.details["reason"] == "newer_schema"
    lg.close()


# ---------------------------------------------------------------------------------------------------------
# jobs, journal, ownership
# ---------------------------------------------------------------------------------------------------------


def test_ledger_job_create_idempotent_and_conflict(led):
    led.create_job("01JZJ100000000000000000000", Role.TARGET, Mode.PUSH, "P1", {"x": 1})
    led.create_job("01JZJ100000000000000000000", Role.TARGET, Mode.PUSH, "P1", {"x": 1})
    with pytest.raises(JobConflict):
        led.create_job("01JZJ100000000000000000000", Role.TARGET, Mode.PUSH, "P1", {"x": 2})
    with pytest.raises(JobConflict):
        led.create_job("01JZJ100000000000000000000", Role.TARGET, Mode.PUSH, "P2", {"x": 1})
    row = led.job("01JZJ100000000000000000000", Role.TARGET)
    assert row["state"] == "open" and row["params"] == {"x": 1} and row["peer_id"] == "P1"
    assert led.job("01JZJ100000000000000000000", Role.SOURCE) is None


def test_ledger_transition_cas_state_and_epoch(led):
    """★ Wrong state or stale epoch ⇒ no change, no event [SK M5].

    Mutant: transition() without the owner-epoch comparison (CAS on state only)."""
    led.create_job("01JZJ000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {})
    epoch = led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "o1", 60)
    assert epoch == 1
    before = len(led.events("01JZJ000000000000000000000", Role.CONDUCTOR))

    with pytest.raises(InvalidState) as wrong_state:
        led.transition("01JZJ000000000000000000000", Role.CONDUCTOR, "hello", "facts", {"x": 1}, epoch=epoch)
    assert not isinstance(wrong_state.value, Fenced)
    assert wrong_state.value.details["state"] == "created"
    assert led.job("01JZJ000000000000000000000", Role.CONDUCTOR)["state"] == "created"
    assert len(led.events("01JZJ000000000000000000000", Role.CONDUCTOR)) == before

    with pytest.raises(Fenced) as stale:
        led.transition("01JZJ000000000000000000000", Role.CONDUCTOR, "created", "hello", {}, epoch=epoch + 1)
    assert stale.value.details["reason"] == "stale_epoch"
    assert led.job("01JZJ000000000000000000000", Role.CONDUCTOR)["state"] == "created"
    assert len(led.events("01JZJ000000000000000000000", Role.CONDUCTOR)) == before

    led.transition(
        "01JZJ000000000000000000000", Role.CONDUCTOR, ("created", "hello"), "hello", {"note": "ok"}, epoch=epoch
    )
    assert led.job("01JZJ000000000000000000000", Role.CONDUCTOR)["state"] == "hello"
    last = led.events("01JZJ000000000000000000000", Role.CONDUCTOR)[-1]
    assert last["kind"] == "transition" and last["state"] == "hello" and last["data"]["note"] == "ok"
    assert last["epoch"] == epoch


def test_ledger_event_cas_on_epoch(led):
    led.create_job("01JZJ000000000000000000000", Role.TARGET, Mode.PUSH, "P", {})
    e = led.claim_owner("01JZJ000000000000000000000", Role.TARGET, "o1", 60)
    led.event("01JZJ000000000000000000000", Role.TARGET, "intent", {"step": "c1:w0", "direction": "forward"}, epoch=e)
    ev = led.events("01JZJ000000000000000000000", Role.TARGET)[-1]
    assert (ev["kind"], ev["step"], ev["direction"]) == ("intent", "c1:w0", "forward")
    n = len(led.events("01JZJ000000000000000000000", Role.TARGET))
    with pytest.raises(Fenced):
        led.event("01JZJ000000000000000000000", Role.TARGET, "done", {"step": "c1:w0"}, epoch=e - 1)
    assert len(led.events("01JZJ000000000000000000000", Role.TARGET)) == n


def test_ledger_claim_owner_live_stale_and_heartbeat(led, clock):
    led.create_job("01JZJ000000000000000000000", Role.CONDUCTOR, Mode.PULL, "P", {})
    assert led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "A", 60) == 1
    assert led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "A", 60) == 1  # same owner: same epoch
    assert led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "B", 60) is None  # live owner
    clock.advance(30)
    assert led.heartbeat("01JZJ000000000000000000000", Role.CONDUCTOR, "A", 1)
    clock.advance(59)
    assert led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "B", 60) is None
    clock.advance(2)
    assert [j["job_id"] for j in led.stale_jobs(60)] == ["01JZJ000000000000000000000"]
    assert led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "B", 60) == 2
    assert not led.heartbeat("01JZJ000000000000000000000", Role.CONDUCTOR, "A", 1)
    assert led.stale_jobs(60) == []
    led.release_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "B", 2)
    assert led.job("01JZJ000000000000000000000", Role.CONDUCTOR)["owner"] is None
    assert led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "C", 60) == 3


def test_participant_epoch_monotonic(led):
    led.create_job("01JZJ000000000000000000000", Role.SOURCE, Mode.PULL, "P", {})
    assert led.peer_epoch("01JZJ000000000000000000000", Role.SOURCE, 3) == 3
    assert led.peer_epoch("01JZJ000000000000000000000", Role.SOURCE, 3) == 3
    assert led.peer_epoch("01JZJ000000000000000000000", Role.SOURCE, 5) == 5
    with pytest.raises(InvalidState) as exc:
        led.peer_epoch("01JZJ000000000000000000000", Role.SOURCE, 4)
    assert exc.value.details["reason"] == "stale_epoch"


def test_ledger_cancel_versus_commit_intent(led):
    """Cancel is accepted until ``intent: commit`` is journaled; the two are serialised (DESIGN §3.8)."""
    led.create_job("01JZJ000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {})
    e = led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "A", 60)
    led.set_cancel("01JZJ000000000000000000000")
    assert led.cancel_requested("01JZJ000000000000000000000")
    assert led.intent_commit("01JZJ000000000000000000000", e) is False

    led.create_job("01JZK000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {})
    e2 = led.claim_owner("01JZK000000000000000000000", Role.CONDUCTOR, "A", 60)
    assert led.intent_commit("01JZK000000000000000000000", e2) is True
    with pytest.raises(InvalidState) as exc:
        led.set_cancel("01JZK000000000000000000000")
    assert exc.value.details["reason"] == "committing"
    assert not led.cancel_requested("01JZK000000000000000000000")


def test_ledger_restarts_and_result(led):
    led.create_job("01JZJ000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {})
    e = led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "A", 60)
    led.job_update(
        "01JZJ000000000000000000000", Role.CONDUCTOR, epoch=e, restarts=2, last_error={"code": "x"}, result={"ok": True}
    )
    row = led.job("01JZJ000000000000000000000", Role.CONDUCTOR)
    assert row["restarts"] == 2 and row["last_error"] == {"code": "x"} and row["result"] == {"ok": True}
    with pytest.raises(Fenced):
        led.job_update("01JZJ000000000000000000000", Role.CONDUCTOR, epoch=e + 7, restarts=3)


def test_ledger_compact_terminal_and_purge_job(led, clock):
    led.create_job("01JZJ000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {})
    e = led.claim_owner("01JZJ000000000000000000000", Role.CONDUCTOR, "A", 60)
    for i in range(5):
        led.event("01JZJ000000000000000000000", Role.CONDUCTOR, "info", {"i": i}, epoch=e)
    led.transition("01JZJ000000000000000000000", Role.CONDUCTOR, "*", "done", {}, epoch=e)
    led.op_put("01JZJ000000000000000000000", "plan", "d", "op1", OpState.DONE, result={})
    led.job_object("01JZJ000000000000000000000", "obj1")
    clock.advance(31 * 86400)
    assert led.compact_terminal(30 * 86400) >= 5
    evs = led.events("01JZJ000000000000000000000", Role.CONDUCTOR)
    assert len(evs) == 1 and evs[0]["state"] == "done"
    assert led.op_by_id("op1") is None  # ops of terminal jobs go after 7 days

    led.create_job("01JZK000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {})
    with pytest.raises(InvalidState):
        led.purge_job("01JZK000000000000000000000")
    led.purge_job("01JZJ000000000000000000000")
    assert (
        led.job("01JZJ000000000000000000000", Role.CONDUCTOR) is None
        and led.events("01JZJ000000000000000000000", Role.CONDUCTOR) == []
    )
    assert "obj1" not in led.live_objects()


# ---------------------------------------------------------------------------------------------------------
# ops
# ---------------------------------------------------------------------------------------------------------


def test_ledger_ops_roundtrip(led):
    led.op_put("01JZJ000000000000000000000", "export", "dg", "op-1", OpState.RUNNING, progress={"step": "x"})
    row = led.op_get("01JZJ000000000000000000000", "export", "dg")
    assert row["op_id"] == "op-1" and row["state"] == "running" and row["progress"] == {"step": "x"}
    led.op_put("01JZJ000000000000000000000", "export", "dg", "op-1", OpState.DONE, result={"capsule_id": "c"})
    row = led.op_by_id("op-1")
    assert row["state"] == "done" and row["result"] == {"capsule_id": "c"} and row["progress"] is None
    assert led.op_get("01JZJ000000000000000000000", "export", "other") is None
    assert [r["op_id"] for r in led.ops_for_job("01JZJ000000000000000000000")] == ["op-1"]


# ---------------------------------------------------------------------------------------------------------
# source handovers, veto, backups
# ---------------------------------------------------------------------------------------------------------


def test_ledger_one_active_handover_per_root(led):
    """★ Second lock of a lineage while one is active ⇒ AlreadyHandingOver (DESIGN §7.2 step 1).

    Mutant: ``source_lock`` without its conflict check and without the partial unique index
    ``one_active_handover``."""
    led.source_lock("01JZJ100000000000000000000", _info(), "h1", "laptop")
    led.source_lock("01JZJ100000000000000000000", _info(), "h1b", "laptop")  # same job: idempotent re-lock
    assert led.source_state("01JZJ100000000000000000000")["holder"] == "h1b"
    with pytest.raises(AlreadyHandingOver) as exc:
        led.source_lock("01JZJ200000000000000000000", _info(tip="t2", lineage=("r1", "t2")), "h2", "server")
    assert exc.value.details["job"] == "01JZJ100000000000000000000"
    assert led.source_state("01JZJ200000000000000000000") is None
    led.source_set("01JZJ100000000000000000000", "released")
    led.source_lock("01JZJ200000000000000000000", _info(tip="t2", lineage=("r1", "t2")), "h2", "server")
    assert led.source_state("01JZJ200000000000000000000")["state"] == "locking"
    with pytest.raises(AlreadyHandingOver):
        led.source_set("01JZJ100000000000000000000", "locking")


def test_ledger_source_rows_veto_backups(led):
    led.source_lock("01JZJ100000000000000000000", _info(), "h1", "laptop")
    led.source_set("01JZJ100000000000000000000", "prepared", prepared_at=5.0, title="T (handed over)")
    led.veto_add("01JZJ100000000000000000000", ["r1", "t1"], "r1")
    led.backup_put("01JZJ100000000000000000000", "goal:t1", '{"status": "active"}')
    led.backup_put("01JZJ100000000000000000000", "title:t1", None)
    rows = {r["session_id"]: r for r in led.veto_rows()}
    assert (
        set(rows) == {"r1", "t1"}
        and rows["t1"]["state"] == "prepared"
        and rows["t1"]["job"] == "01JZJ100000000000000000000"
    )
    assert rows["t1"]["peer"] == "laptop" and rows["t1"]["root_id"] == "r1"
    assert led.source_by_session("t1")["job_id"] == "01JZJ100000000000000000000"
    assert led.source_by_session("nope") is None
    assert dict(led.backups("01JZJ100000000000000000000")) == {"goal:t1": '{"status": "active"}', "title:t1": None}
    led.source_set("01JZJ100000000000000000000", "retiring", ack={"session_id": "S"})
    assert led.source_state("01JZJ100000000000000000000")["ack"] == {"session_id": "S"}
    led.veto_remove("01JZJ100000000000000000000")
    assert led.veto_rows() == []


# ---------------------------------------------------------------------------------------------------------
# peers, pairing, nonces, revocation
# ---------------------------------------------------------------------------------------------------------


def test_ledger_peers_roundtrip(led):
    led.upsert_peer(_peer("server", "D1", "dial", (Right.PULL,)))
    led.upsert_peer(_peer("laptop", "I1", "inbound"))
    p = led.peer("server")
    assert p.direction == "dial" and p.gate.mode is GateMode.SESSION_TOKEN and p.rights == frozenset({Right.PULL})
    assert led.peer("D1") == p
    assert led.inbound_peer("D1") is None  # a dial peer never authenticates inbound requests (SEC 1.7)
    assert led.inbound_peer("I1").name == "laptop"
    with pytest.raises(ConflictError):
        led.upsert_peer(_peer("laptop", "X9", "dial"))
    assert led.rename_peer("server", "srv")
    assert led.peer("srv").peer_id == "D1"
    led.touch_peer("I1", "10.0.0.2")
    assert led.peer("I1").last_ip == "10.0.0.2"
    assert led.remove_peer("srv")
    assert led.peer("D1") is None
    assert not led.remove_peer("laptop")  # inbound peers are revoked, not removed


def test_ledger_pair_code_lifecycle(led, clock):
    now = clock()
    led.put_pair_code("ABC123", "s3cret", frozenset({Right.PUSH}), "laptop", now + 600)
    assert led.pairing_pending(now)
    assert led.pair_code("ABC123", now) == ("s3cret", frozenset({Right.PUSH}), "laptop")
    assert led.pair_code("ABC123", now + 601) is None
    assert led.redeem_pair_code("ABC123", now, _peer("laptop", "I1"))
    assert not led.redeem_pair_code("ABC123", now, _peer("laptop2", "I2"))
    assert led.inbound_peer("I1") is not None and led.peer("I2") is None
    assert not led.pairing_pending(now)

    led.put_pair_code("DEF456", "x", frozenset({Right.PULL}), "", now + 600)
    assert [led.fail_pair_code("DEF456") for _ in range(7)] == [1, 2, 3, 4, 5, 5, 5]
    assert led.pair_code("DEF456", now) is None
    assert not led.redeem_pair_code("DEF456", now, _peer("z", "I9"))

    led.put_pair_code("GHI789", "y", frozenset({Right.PULL}), "", now + 10)
    assert led.redeem_pair_code("GHI789", now, _peer("laptop", "I3"))  # "laptop" is taken by I1 ⇒ suffix
    assert led.peer("I3").name == "laptop-2"


def test_janitor_purges_expired_codes(led, clock):
    """Expired and burned codes are deleted (SEC 6.2; the janitor calls this on every start)."""
    now = clock()
    led.put_pair_code("OLD000", "a", frozenset({Right.PUSH}), "", now - 1)
    led.put_pair_code("BURN00", "b", frozenset({Right.PUSH}), "", now + 600)
    for _ in range(5):
        led.fail_pair_code("BURN00")
    led.put_pair_code("LIVE00", "c", frozenset({Right.PUSH}), "", now + 600)
    assert led.purge_expired_codes(now) == 2
    with led.tx() as conn:
        assert [r[0] for r in conn.execute("SELECT code_id FROM pair_codes")] == ["LIVE00"]


def test_ledger_nonce_replay(led, clock):
    ts = int(clock())
    assert not led.nonce_seen("P", "n1", ts, 900)
    assert led.nonce_seen("P", "n1", ts, 900)
    assert not led.nonce_seen("Q", "n1", ts, 900)  # per peer
    clock.advance(901)
    assert not led.nonce_seen("P", "n2", int(clock()), 900)  # prunes the expired n1
    assert not led.nonce_seen("P", "n1", int(clock()), 900)


def test_ledger_revoke_cascades(led, clock):
    """★ Revoke ⇒ offers deleted, non-terminal jobs of the peer ``aborting`` except prepared/committing (SEC 1.6).

    Mutant: revoke_peer only sets ``revoked_at``."""
    led.upsert_peer(_peer("laptop", "I1", "inbound"))
    led.upsert_peer(_peer("other", "I2", "inbound"))
    now = clock()
    led.offer_put("O1", "I1", "s1", None, None, now + 3600)
    led.offer_put("O2", "I2", "s2", None, None, now + 3600)
    for jid, role, state in (
        ("01JZT0PREP0000000000000000", Role.TARGET, "preparing"),
        ("01JZT0C0MMYT00000000000000", Role.TARGET, "committing"),
        ("01JZT0D0NE0000000000000000", Role.TARGET, "committed"),
    ):
        led.create_job(jid, role, Mode.PUSH, "I1", {})
        led.transition(jid, role, "*", state, {}, epoch=0)
    led.create_job("01JZS0Y0CKED00000000000000", Role.SOURCE, Mode.PULL, "I1", {})
    led.source_lock("01JZS0Y0CKED00000000000000", _info(root="ra", tip="ta", lineage=("ra", "ta")), "h", "laptop")
    led.source_set("01JZS0Y0CKED00000000000000", "locked")
    led.create_job("01JZS0PREP0000000000000000", Role.SOURCE, Mode.PULL, "I1", {})
    led.source_lock("01JZS0PREP0000000000000000", _info(root="rb", tip="tb", lineage=("rb", "tb")), "h", "laptop")
    led.source_set("01JZS0PREP0000000000000000", "prepared")
    led.create_job("01JZS00THER000000000000000", Role.SOURCE, Mode.PULL, "I2", {})

    ids = led.revoke_peer("laptop")
    assert sorted(ids) == ["01JZS0Y0CKED00000000000000", "01JZT0PREP0000000000000000"]
    assert led.peer("I1").revoked_at is not None
    assert led.inbound_peer("I1") is None
    assert led.offers_for("I1", now) == [] and [o["offer_id"] for o in led.offers_for("I2", now)] == ["O2"]
    assert led.job("01JZT0PREP0000000000000000", Role.TARGET)["state"] == "aborting"
    assert led.job("01JZS0Y0CKED00000000000000", Role.SOURCE)["state"] == "aborting"
    assert led.job("01JZT0C0MMYT00000000000000", Role.TARGET)["state"] == "committing"
    assert led.job("01JZT0D0NE0000000000000000", Role.TARGET)["state"] == "committed"
    assert led.source_state("01JZS0PREP0000000000000000")["state"] == "prepared"
    assert led.job("01JZS00THER000000000000000", Role.SOURCE)["state"] == "open"
    with pytest.raises(NotFound):
        led.revoke_peer("nobody")


# ---------------------------------------------------------------------------------------------------------
# offers, suggestions, outbox, maps, objects, arrivals
# ---------------------------------------------------------------------------------------------------------


def test_ledger_offers_per_peer(led, clock):
    now = clock()
    led.offer_put("O1", "I1", "s1", "note", {"notes": "x"}, now + 7200)
    assert [o["offer_id"] for o in led.offers_for("I1", now)] == ["O1"]
    assert led.offers_for("I2", now) == []
    with pytest.raises(NotFound):
        led.offer_claim("O1", "I2", "J9")
    got = led.offer_claim("O1", "I1", "01JZJ100000000000000000000")
    assert got["session_id"] == "s1" and got["refinement"] == {"notes": "x"}
    assert (
        led.offer_claim("O1", "I1", "01JZJ100000000000000000000")["claimed_job"] == "01JZJ100000000000000000000"
    )  # idempotent
    with pytest.raises(NotFound):
        led.offer_claim("O1", "I1", "01JZJ200000000000000000000")
    assert led.offers_for("I1", now) == []
    led.offer_put("O2", "I1", "s2", None, None, now + 10)
    clock.advance(11)
    with pytest.raises(NotFound):
        led.offer_claim("O2", "I1", "01JZJ300000000000000000000")


def test_ledger_suggestions_outbox_pull_log(led, clock):
    led.suggestion_put("s1", {"path": "/x"})
    clock.advance(10)
    assert [s["path"] for s in led.suggestions("s1", clock())] == ["/x"]
    clock.advance(86400)
    assert led.suggestions("s1", clock()) == []

    led.outbox_put("departure", {"a": 1})
    due = led.outbox_due(clock())
    assert len(due) == 1 and due[0]["payload"] == {"a": 1}
    led.outbox_done(due[0]["id"], False, clock())
    assert led.outbox_due(clock()) == []
    clock.advance(3600)
    assert len(led.outbox_due(clock())) == 1
    led.outbox_done(due[0]["id"], True, clock())
    clock.advance(86400)
    assert led.outbox_due(clock()) == []

    led.pull_log_put("01JZJ000000000000000000000", "I1", ["w0", "x1"])
    assert led.pull_log(5)[0]["items"] == ["w0", "x1"]


def test_ledger_workspace_map(led):
    led.map_put(MapRow("/home/u/app", "I1", "/srv/app", "in", "rid", None, "01JZJ100000000000000000000"))
    led.map_put(MapRow("/srv/app", "I1", "/home/u/app", "out", "rid", {"head": "h"}, "01JZJ200000000000000000000"))
    assert led.map_lookup("I1", "/srv/app").local_path == "/home/u/app"
    assert led.map_out("/srv/app", "I1").state == {"head": "h"}
    assert led.map_out("/home/u/app", "I1") is None
    assert {r.direction for r in led.map_by_local("/srv/app")} == {"out"}


def test_ledger_objects_blocks_gc(led, clock):
    led.object_put(ObjectManifest("s", 10, 524288, ("b1",), "application/json", "o1"))
    assert led.object_get("o1").sha256 == "s"
    led.block_put("b1", 100)
    clock.advance(10)
    led.block_put("b2", 100)
    led.block_put("b3", 100)
    led.create_job("01JZJ000000000000000000000", Role.TARGET, Mode.PUSH, "P", {})
    led.job_object("01JZJ000000000000000000000", "o1")
    assert led.live_objects() == {"o1"}
    assert led.gc_candidates(clock() - 5, 10_000) == ["b1"]
    assert led.gc_candidates(0, 150) == ["b1", "b2"]
    clock.advance(1)
    led.block_touch(["b1"])
    led.block_delete(["b2"])
    assert led.gc_candidates(clock() + 1, 10_000) == ["b3", "b1"]


def test_ledger_arrivals_and_bundle_imports(led):
    from ctx_handover.engine.model import PathMapEntry

    led.arrival_put(
        session_ids=["n1", "n2"],
        root_id="n1",
        job_id="01JZJ000000000000000000000",
        peer="server",
        note="NOTE",
        report_path="/r.md",
        path_map=[PathMapEntry("/a", "/b", "workspace")],
        continue_prompt=None,
    )
    a = led.arrival_for("n2")
    assert a["note"] == "NOTE" and a["note_state"] == "pending" and a["path_map"][0]["target"] == "/b"
    led.note_offer("n2", "turn-1")
    assert led.arrival_for("n2")["note_state"] == "offered" and led.arrival_for("n2")["offers"] == 1
    led.note_delivered("n2")
    assert led.arrival_for("n2")["note_state"] == "delivered"
    assert {r["session_id"] for r in led.arrival_rows()} == {"n1", "n2"}
    led.arrival_source_reclaimed("01JZJ000000000000000000000")
    assert led.arrival_for("n1")["source_reclaimed_at"] is not None
    led.arrival_delete("01JZJ000000000000000000000")
    assert led.arrival_for("n1") is None

    assert led.bundle_import_seen("B1") is None
    led.bundle_import_put("B1", "n1")
    assert led.bundle_import_seen("B1") == "n1"


def test_ledger_data_version_changes_on_foreign_commit(tmp_path, clock):
    path = tmp_path / "l.sqlite3"
    a = Ledger(path, clock=clock)
    b = Ledger(path, clock=clock)
    v0 = a.data_version()
    b.meta_set("k", "v")
    assert a.data_version() != v0
    assert a.meta_get("k") == "v"
    a.close()
    b.close()


def test_ledger_threads_get_own_connections(led):
    import threading

    led.create_job("01JZJ000000000000000000000", Role.CONDUCTOR, Mode.PUSH, None, {})
    errors = []

    def worker(i):
        try:
            for k in range(20):
                led.meta_set(f"t{i}-{k}", "x")
        except Exception as exc:
            errors.append(exc)

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(4)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert errors == []
    assert led.meta_get("t3-19") == "x"


def test_ledger_files_never_world_readable_under_permissive_umask(tmp_path, clock):
    """SEC 6.1: 0700 directory before the first file, 0600 files even with umask 000."""
    old = os.umask(0)
    try:
        lg = Ledger(tmp_path / "pd2" / "ledger.sqlite3", clock=clock)
        lg.meta_set("a", "b")
        lg.close()
    finally:
        os.umask(old)
    assert stat.S_IMODE((tmp_path / "pd2").stat().st_mode) == 0o700
    for p in (tmp_path / "pd2").iterdir():
        assert stat.S_IMODE(p.stat().st_mode) == 0o600, p
