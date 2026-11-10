"""First review round, target findings in ``engine/local_target.py``: M7 (the model-history hash was always a gate at
the target and overruled the version rule of the import), M8-A (a process that died inside the hidden import left the
sessions already written behind — the planned ids are journaled before the first write now and discarded on abort)
and the hand-over (the workspace transaction's warnings — conflict fallbacks, git-lfs — never reached the
``CommitAck`` and so the final message)."""

from __future__ import annotations

import dataclasses

import pytest

from ctx_handover.engine.model import OpState, Role
from ctx_handover.tests.fakes.apis_fake import SimulatedCrash
from ctx_handover.tests.unit.A import standins
from ctx_handover.tests.unit.A.test_local_target import WorkspaceTxn, env  # noqa: F401  (fixture)


def test_rv_a_m7_model_hash_follows_the_import_version_rule(env):  # noqa: F811
    """Different Hermes versions repair the alternation differently: the import reports a different ``h_model`` with
    a warning (``session_import.import_hidden`` raises only for equal versions). The target must not overrule that
    with a gate of its own; ``h_all``/``h_rows`` stay the gate."""
    real = env.target.ports.session_import.import_hidden

    def other_version(*args, **kw):
        prepared = real(*args, **kw)
        warning = "model history hash differs (different Hermes versions repair alternation differently)"
        return dataclasses.replace(prepared, h_model="other-version-model-hash", warnings=(warning,))

    env.target.ports.session_import.import_hidden = other_version
    prepared = env.prepared()

    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "prepared"
    assert any("model history hash differs" in w for w in prepared.warnings)


def test_rv_a_m7_rows_hash_is_still_the_gate(env):  # noqa: F811
    real = env.target.ports.session_import.import_hidden

    def wrong_rows(*args, **kw):
        return dataclasses.replace(real(*args, **kw), h_rows="not-the-capsule")

    env.target.ports.session_import.import_hidden = wrong_rows
    with pytest.raises(AssertionError, match="verify_failed"):
        env.prepared()


def test_rv_a_workspace_warnings_reach_the_commit_ack(env):  # noqa: F811
    """A fallback the workspace transaction took (conflict ⇒ fresh directory, no git-lfs …) is part of what the user
    is told at the end: ``CommitAck.warnings``."""

    class WarningTxn(WorkspaceTxn):
        def prepare_item(self, item, decision, pack_item, *, out_row) -> None:
            super().prepare_item(item, decision, pack_item, out_row=out_row)
            self.warnings = [*getattr(self, "warnings", []), f"{decision.dest}: conflict; materialised fresh at /x"]

    env.target.ports.materialize.WorkspaceTxn = WarningTxn
    prepared = env.prepared()
    assert any("materialised fresh" in w for w in prepared.warnings)
    st = env.wait(env.target.start_commit(env.job))
    assert st.state is OpState.DONE, st.error
    assert any("materialised fresh" in w for w in st.result["warnings"])


def test_rv_a_m8_sessions_of_an_interrupted_import_are_discarded_on_abort(env):  # noqa: F811
    """The process dies after the import wrote its first session (Hermes commits every write on its own). The
    journal named the planned ids before that write, so the abort removes what was written; a session that existed
    before under such an id is never touched."""
    real = standins.import_hidden
    written: list[str] = []

    def dies_midway(db, store, capsule_id, plan, **kw):
        sid = plan.segments[0].new_id
        db.create_session(sid, "cli", model="test-model", cwd=plan.cwd)
        written.append(sid)
        raise SimulatedCrash("process died inside import_hidden")

    env.target.ports.session_import.import_hidden = dies_midway
    with pytest.raises(BaseException):  # noqa: B017 - the simulated crash leaves the operation running
        env.prepared()
    assert written and env.db.get_session(written[0]) is not None

    env.target.ports.session_import.import_hidden = real
    env.target.abort(env.job, "owner_died")

    assert env.ledger.job(env.job.job_id, Role.TARGET)["state"] == "aborted"
    assert env.db.get_session(written[0]) is None


# -- M5: the R28 quota through the production wiring -----------------------------------------------------------------


def _listener_over(env, tmp_path):  # noqa: F811
    """The real ``Server`` with the real ``ParticipantRegistry`` serving ``env.target`` (a real ``LocalTarget`` over a
    real ``BlockStore``) on the target's own ledger, and a paired pushing peer."""
    import time as _time

    from ctx_handover.engine.ids import ulid
    from ctx_handover.engine.ledger import Ledger
    from ctx_handover.engine.model import Peer, Right
    from ctx_handover.engine.participant import ParticipantRegistry
    from ctx_handover.engine.server import OperatorSessionAuth, PeerSignatureAuth, Server
    from ctx_handover.tests.unit.C.conftest import new_identity
    from ctx_handover.tests.unit.C.test_server import Dialer

    identity = new_identity(env.ledger, {})
    registry = ParticipantRegistry(env.ledger, lambda role, peer: env.target)
    auths = [OperatorSessionAuth(enabled=False), PeerSignatureAuth(env.ledger, identity)]
    server = Server(env.ledger, identity, registry, {"name": "server"}, auths)
    dialer_ledger = Ledger(tmp_path / "dialer" / "ledger.sqlite3")
    ident = new_identity(dialer_ledger, {})
    peer = Peer(
        name="laptop",
        peer_id=ulid(),
        direction="inbound",
        url=None,
        pubkey=ident.public_b64(),
        rights=frozenset({Right.PUSH}),
        created_at=_time.time(),
    )
    env.ledger.upsert_peer(peer)
    return server, Dialer(ident, peer.peer_id, identity.instance_id), dialer_ledger


def _noise(n: int, seed: int) -> bytes:
    import hashlib

    out, i = bytearray(), 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}:{i}".encode()).digest()
        i += 1
    return bytes(out[:n])


def test_rv_a_m5_put_beyond_the_confirmed_plan_is_refused_by_the_real_target(env, tmp_path):  # noqa: F811
    """R28 end to end on the listener: ``PUT …/blocks`` beyond ``need · 1.1`` of the confirmed plan ⇒ 507
    ``insufficient_storage`` (``job_quota``) and no byte of the request in the store. The sink is the target's own
    ``blocks_for`` (a mutant handing out the bare store stores everything)."""
    import asyncio

    from ctx_handover.engine.blocks import BLOCK_SIZE, MEDIA_BLOCKS, BlockStore, encode_frame
    from ctx_handover.tests.fakes.apis_fake import make_facts
    from ctx_handover.tests.unit.C.test_server import body_of

    env.store = BlockStore(env.data, env.ledger)
    env.target = env.make_target()
    server, dialer, dialer_ledger = _listener_over(env, tmp_path)
    job = env.job.job_id
    body = {"job_id": job, "epoch": 1, "role": "target", "mode": "push", "conductor": {"name": "l"}, "options": {}}
    assert asyncio.run(server.handle(dialer.req("POST", "jobs", body))).status == 200
    env.target.facts(env.job, make_facts("server"))
    env.target.negotiate(env.job, env.plan(size=1000))
    quota, _used = env.ledger.quota_get(job, Role.TARGET)
    blocks = []
    while sum(len(raw) for _b, raw in blocks) <= quota:
        raw = _noise(BLOCK_SIZE, len(blocks))
        import hashlib

        blocks.append((hashlib.sha256(raw).hexdigest(), raw))
    frames = b"".join(encode_frame(b, raw, peer_caps=frozenset({"enc.zlib"})) for b, raw in blocks)

    resp = asyncio.run(server.handle(dialer.req("PUT", f"jobs/{job}/blocks", raw=frames, ctype=MEDIA_BLOCKS, epoch=1)))

    assert resp.status == 507, (resp.status, resp.body[:300])
    err = body_of(resp)["error"]
    assert err["code"] == "insufficient_storage" and err["details"]["reason"] == "job_quota"
    assert env.store.missing([b for b, _raw in blocks]) == [b for b, _raw in blocks]
    dialer_ledger.close()


def test_rv_a_m5_a_renegotiated_smaller_plan_lowers_the_quota(env):  # noqa: F811
    """The quota follows the plan confirmed last (``max(used, need · 1.1)``), not the largest proposal ever made — a
    rejected 4 GB proposal does not stay storable after the 1 kB plan was confirmed."""
    from ctx_handover.tests.fakes.apis_fake import make_facts

    env.target.open_job(env.job)
    env.target.facts(env.job, make_facts("server"))
    env.target.negotiate(env.job, env.plan(size=4 * 1024**3))
    big, _ = env.ledger.quota_get(env.job.job_id, Role.TARGET)
    small_final = env.target.negotiate(env.job, env.plan(size=1000))
    small, used = env.ledger.quota_get(env.job.job_id, Role.TARGET)
    assert big > 4 * 1024**3
    assert small == max(used, int(small_final.need_bytes * 1.1)) < 1024**3
