"""Layer R (red first) — open contract points of cluster C (``reports/L2-contracts.md`` V17, V25, V27).

Real modules: cluster A's ``Ledger``, ``ParticipantRegistry`` and ``LocalSource``, cluster C's ``Server``,
``WireClient``, ``RemoteSource``/``RemoteTarget`` and ``BlockStore``. The dialer reaches the listener through the
real ``WireClient`` (signing, response signatures, error envelopes); only the socket is replaced by an in-process
``httpx`` transport that hands the request to ``Server.handle`` the way ``dashboard/plugin_api.py`` does.
"""

from __future__ import annotations

import contextlib
import math
import os
import time

import pytest

from ctx_handover.engine import blocks, wire
from ctx_handover.engine.blocks import BlockStore
from ctx_handover.engine.errors import HandoverError, LeaseLost
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.jsonutil import to_wire
from ctx_handover.engine.model import Mode, Role, SessionInfo
from ctx_handover.engine.remote import RemoteSource, RemoteTarget
from ctx_handover.tests.unit.C._l2r_helpers import M, Pair, job_context, local_source, noise
from ctx_handover.tests.unit.C.conftest import end_job
from ctx_handover.tests.unit.C.helpers import sample_facts

MIB = 1024**2
INFO = SessionInfo("tip", "root", ("root", "tip"), "T", None, None, None, None, None, 1, (1,))


def _pull_pair(tmp_path, fake_host) -> tuple[Pair, RemoteSource, object]:
    """A pull job: the listener's real ``LocalSource`` locked the session for the job, the dialer holds the
    ``RemoteSource`` of it."""
    pair = Pair(tmp_path, source=lambda ledger: local_source(ledger, tmp_path, fake_host))
    remote = RemoteSource(pair.client())
    job = job_context(mode=Mode.PULL)
    remote._open(job, Role.SOURCE)  # what every first RemoteSource call does (POST /jobs, idempotent)
    pair.ledger.source_lock(job.job_id, INFO, "test:1", pair.peer_id)
    return pair, remote, job


# ---------------------------------------------------------------------------------------------------------
# V17 — one bind per request; routes #8/#9 carry ``source_reclaimed``
# ---------------------------------------------------------------------------------------------------------


def test_l2r_server_binds_the_job_once_per_request(tmp_path, fake_host):
    """V17: for a job route the server calls ``registry.bind(auth, job_id, role)`` exactly once — one peer-epoch
    fence and one ``touch_job`` per request, not one for the API and another for the job context.

    Counted at the registry: every way into ``bind`` (``get``, ``context`` or a direct call) goes through the
    instance attribute replaced here.
    """
    pair = Pair(tmp_path, source=lambda ledger: local_source(ledger, tmp_path, fake_host))
    calls: list[tuple[str, Role]] = []
    real_bind = pair.registry.bind

    def counting_bind(auth, job_id, role):
        calls.append((job_id, role))
        return real_bind(auth, job_id, role)

    pair.registry.bind = counting_bind
    client = pair.client()
    push = job_context(mode=Mode.PUSH)
    RemoteTarget(client).open_job(push)
    calls.clear()
    facts = to_wire(sample_facts(M, "src"))
    client.call("POST", f"jobs/{push.job_id}/target/facts", {"source_facts": facts}, epoch=1)
    assert calls == [(push.job_id, Role.TARGET)], "target route: one bind per request"

    pull = job_context(mode=Mode.PULL)
    remote = RemoteSource(client)
    remote._open(pull, Role.SOURCE)
    calls.clear()
    with contextlib.suppress(HandoverError):  # not locked yet: the route refuses after binding
        remote.start_export(pull)
    assert calls == [(pull.job_id, Role.SOURCE)], "source route: one bind per request"


def test_l2r_route8_note_source_reclaimed_marks_the_target_arrival(tmp_path):
    """V17/W047 (DESIGN §7.7): a source that dialed tells the target with route #8 ``{"note":
    "source_reclaimed"}`` (the request ``jobs._notify_reclaimed`` sends); through the server the registry marks
    the arrival on the listener (``arrival_for(...)["source_reclaimed_at"]``).

    Red proof: mutant = ``Server._h_job_status`` hands the registry the body without ``note``.
    """
    pair = Pair(tmp_path)
    client = pair.client()
    job = job_context(mode=Mode.PUSH)
    RemoteTarget(client).open_job(job)
    pair.ledger.arrival_put(
        session_ids=["n1"],
        root_id="n1",
        job_id=job.job_id,
        peer="laptop",
        note="N",
        report_path="",
        path_map=[],
        continue_prompt=None,
    )
    assert pair.ledger.arrival_for("n1")["source_reclaimed_at"] is None
    client.call("POST", f"jobs/{job.job_id}/status", {"epoch": 1, "note": "source_reclaimed"}, epoch=1)
    assert pair.ledger.arrival_for("n1")["source_reclaimed_at"] is not None


def test_l2r_pull_source_reclaimed_reaches_the_dialer_by_status(tmp_path, fake_host):
    """V17/W047: the listener was the source of a pull and took the session back; route #8 brings
    ``source_reclaimed`` in the status ``notes`` to the dialer's ``RemoteSource`` (DESIGN §7.7: "the target learns
    it from the source's status notes on its next contact").

    Red proof: mutant = ``Server._h_job_status`` answers the status without its ``notes``.
    """
    pair, remote, job = _pull_pair(tmp_path, fake_host)
    pair.ledger.source_set(job.job_id, "reclaimed")
    assert remote.status(job).notes == ("source_reclaimed",)


def test_l2r_pull_source_reclaimed_reaches_the_dialer_by_heartbeat(tmp_path, fake_host):
    """V17/W047: the same reclaimed pull source, reached by the conductor's keepalive (route #9,
    ``{state, lease_expires_at?}``, DESIGN §4.7 table): the dialer learns that the source was *reclaimed* — the
    ``LeaseLost`` its ``RemoteSource.heartbeat`` raises names ``state == "reclaimed"`` (the form ``RemoteSource``
    gives a route-#9 state in ``remote.LOST_STATES``), not an anonymous lost lease."""
    pair, remote, job = _pull_pair(tmp_path, fake_host)
    pair.ledger.source_set(job.job_id, "reclaimed")
    with pytest.raises(LeaseLost) as exc:
        remote.heartbeat(job)
    assert exc.value.details.get("state") == "reclaimed", (exc.value.message, exc.value.details)


# ---------------------------------------------------------------------------------------------------------
# V25 — time protection of the GC only for running jobs; suspended jobs keep their registered objects
# ---------------------------------------------------------------------------------------------------------


class _Clock:
    """Ledger clock: ``at`` while set (job rows created in the past), else wall time."""

    def __init__(self) -> None:
        self.at: float | None = None

    def __call__(self) -> float:
        return time.time() if self.at is None else self.at


def _aged(store: BlockStore, m, seconds: float) -> None:
    """Block files and manifest of ``m`` as last touched ``seconds`` ago (file times are what the GC reads)."""
    then = time.time() - seconds
    for path in (store.manifest_path(m.object_id), *(store.block_path(b) for b in m.blocks)):
        os.utime(path, (then, then))


def _readable(store: BlockStore, m, data: bytes) -> bool:
    with store.open_object(m.object_id) as fh:
        return fh.read() == data


def _gone(store: BlockStore, m) -> bool:
    return not any(store.has(b) for b in m.blocks) and not store.manifest_path(m.object_id).exists()


@pytest.mark.parametrize("role", [Role.CONDUCTOR, Role.SOURCE], ids=["conductor", "source"])
def test_l2r_gc_time_protection_only_for_running_jobs(tmp_path, role):
    """V25: the GC's time protection (blocks and manifests touched since the oldest job began, at most 24 h back)
    counts only *running* jobs. A ``suspended`` job (it may wait for ``/handover resume`` for days) protects its
    registered objects — and nothing else: unregistered cache touched after it began goes over the cap.

    Second half (control, holds today): a running job that began three days ago protects what was touched within
    the last 24 h, not what is older.
    """
    clock = _Clock()
    from ctx_handover.engine.ledger import Ledger

    ledger = Ledger(tmp_path / "data" / "ledger.sqlite3", clock=clock)
    store = BlockStore(tmp_path / "data", ledger, cache_bytes=0, cache_days=3)

    clock.at = time.time() - 2 * 3600  # the suspended job began two hours ago
    parked = ulid()
    ledger.create_job(parked, role, Mode.PULL if role is Role.SOURCE else Mode.PUSH, None, {"role": str(role)})
    end_job(ledger, parked, role, "suspended")
    clock.at = None
    assert ledger.job(parked, role)["state"] == "suspended"

    data_kept, data_cache = noise(blocks.BLOCK_SIZE + 5, 61), noise(blocks.BLOCK_SIZE + 6, 62)
    kept = store.put_bytes(data_kept, "application/octet-stream")
    ledger.job_object(parked, kept.object_id)
    cache = store.put_bytes(data_cache, "application/octet-stream")
    _aged(store, kept, 3 * 3600)  # registered before the job was parked
    _aged(store, cache, 3600)  # touched by something after the parked job began, never registered

    store.gc(keep_days=3, max_bytes=0)
    assert _readable(store, kept, data_kept), "a suspended job keeps its registered objects"
    assert _gone(store, cache), "a suspended job gives no time protection to unregistered cache (V25)"

    clock.at = time.time() - 3 * 86_400  # a running job that began three days ago
    running = ulid()
    ledger.create_job(running, Role.CONDUCTOR, Mode.PUSH, None, {"role": "conductor", "n": 2})
    clock.at = None
    data_recent, data_old = noise(blocks.BLOCK_SIZE + 7, 63), noise(blocks.BLOCK_SIZE + 8, 64)
    recent = store.put_bytes(data_recent, "application/octet-stream")
    old = store.put_bytes(data_old, "application/octet-stream")
    _aged(store, recent, 3600)
    _aged(store, old, 2 * 86_400)
    store.gc(keep_days=3, max_bytes=0)
    assert _readable(store, recent, data_recent), "a running job protects what was touched since it began"
    assert _gone(store, old), "the protection reaches back at most MAX_IN_FLIGHT_S (24 h)"
    assert _readable(store, kept, data_kept)
    assert blocks.MAX_IN_FLIGHT_S <= 86_400 and not math.isinf(blocks.MAX_IN_FLIGHT_S)


# ---------------------------------------------------------------------------------------------------------
# V27 — the body-limit measurement sizes are a constant of ``wire``
# ---------------------------------------------------------------------------------------------------------


def test_l2r_probe_sizes_are_a_wire_constant(tmp_path, monkeypatch):
    """V27 (wp-f §7 wish 5, wp-c W-5): the sizes of ``probe_gate(measure_limit=True)`` are one constant in
    ``wire`` — ``wire.PROBE_SIZES_MIB == (1, 2, 4, 8)`` — and the probe sends exactly those bodies (checked by
    narrowing the constant: the probe follows it, no literal sizes beside it)."""
    assert wire.PROBE_SIZES_MIB == (1, 2, 4, 8)
    pair = Pair(tmp_path)
    monkeypatch.setattr(wire, "PROBE_SIZES_MIB", (1, 3))
    report = pair.client().probe_gate(measure_limit=True)
    sent = [n for method, route, n in pair.transport.seen if method == "PUT" and route.endswith("/blocks")]
    assert sent == [1 * MIB, 3 * MIB]
    assert report["body_limit"] == 3 * MIB
