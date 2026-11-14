"""ParticipantRegistry and ``participant_options``, layer 2 (contract X07, X18, X25/K-2; W047).

Real ``Ledger``; participant APIs recorded. The server delegates routes #8–#10 and the job context to the registry;
the registry is the one owner of the job caps (values from its constructor, filled from settings).
"""

from __future__ import annotations

import pytest

from ctx_handover.engine.errors import BadRequest, NotFound, RateLimited
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.local_source import LocalSource
from ctx_handover.engine.model import (
    AuthContext,
    AuthScheme,
    HermesPaths,
    Mode,
    Options,
    ParticipantStatus,
    Peer,
    Right,
    Role,
    SessionInfo,
)
from ctx_handover.engine.ops import OpRunner
from ctx_handover.engine.participant import ParticipantRegistry, participant_options


class Api:
    def __init__(self, log):
        self.log = log

    def open_job(self, job):
        self.log.append(("open_job", job.job_id))

    def heartbeat(self, job):
        self.log.append(("heartbeat", job.job_id))

    def abort(self, job, reason):
        self.log.append(("abort", job.job_id))

    def start_release(self, job, reason):
        self.log.append(("release", job.job_id))

    def status(self, job):
        return ParticipantStatus(state="open", step="", epoch=job.epoch, progress=None, ops={}, last_error=None)


def _peer(pid: str, rights=(Right.PUSH, Right.PULL)) -> Peer:
    return Peer(name=pid.lower(), peer_id=pid, direction="inbound", url=None, pubkey="pk", rights=frozenset(rights))


def _auth(peer: Peer, epoch: int = 1) -> AuthContext:
    return AuthContext(scheme=AuthScheme.PEER_SIGNATURE, peer=peer, rights=peer.rights, epoch=epoch)


def _body(job_id, role="target", mode="push") -> dict:
    return {"job_id": job_id, "epoch": 1, "role": role, "mode": mode, "conductor": {}, "options": {}}


@pytest.fixture
def led(tmp_path, clock):
    ledger = Ledger(tmp_path / "l.sqlite3", clock=clock)
    yield ledger
    ledger.close()


@pytest.mark.parametrize(
    "bad",
    ["J1", "01JOB", ulid().lower(), ulid() + "0", "8" + ulid()[1:], "01HZZZZZZZZZZZZZZZZZZZZZZ" + "I", 12345, None],
)
def test_l2_registry_job_ids_are_ulids(led, clock, bad):
    """X25/K-2: job ids from a peer are canonical ULIDs (``ids.ULID_RE``) at ``POST /jobs`` and at an offer claim;
    anything else is ``bad_request`` before any ledger row exists."""
    reg = ParticipantRegistry(led, lambda role, peer: Api([]), clock=clock)
    p = _peer("P1")
    with pytest.raises(BadRequest) as exc:
        reg.open(_auth(p), _body(bad))
    assert exc.value.details["field"] == "job_id"
    led.offer_put("O1", "P1", "sess-1", "note", None, clock() + 3600)
    with pytest.raises(BadRequest):
        reg.claim(_auth(p), "O1", bad)
    assert led.jobs(limit=10) == []
    good = ulid()
    assert reg.open(_auth(p), _body(good))["job_id"] == good
    assert reg.claim(_auth(p), "O1", ulid())["session_ref"]["id"] == "sess-1"


def test_l2_participant_options_take_no_remote_additions():
    """X07/R18: of a remote conductor's hints only the narrowing ones (``exclude``, ``no_workspace``) and the
    destination overrides survive; ``include_secret`` and ``include`` never come from a peer."""
    remote = {
        "hints": {
            "include": ["/home/u/secret-plans"],
            "include_secret": ["/home/u/.ssh/id_ed25519"],
            "exclude": ["*.log"],
            "dest_overrides": [["w0", "~/x"]],
            "no_workspace": True,
        }
    }
    o = participant_options(Options(), remote)
    assert o.hints.include == () and o.hints.include_secret == ()
    assert o.hints.exclude == ("*.log",) and o.hints.no_workspace is True
    assert o.hints.dest_overrides == (("w0", "~/x"),)


def test_l2_registry_owns_the_job_caps_from_its_constructor(led, clock):
    """X18: the caps are the registry's constructor values (filled from ``max_jobs_per_peer``/``max_jobs_total``);
    conductor jobs of this host do not count; a peer's own routes #8–#10 stay bound to its jobs."""
    log: list = []
    reg = ParticipantRegistry(led, lambda role, peer: Api(log), clock=clock, max_jobs_per_peer=1, max_jobs=2)
    led.create_job(ulid(), Role.CONDUCTOR, Mode.PUSH, None, {"mode": "push"})
    p1, p2, p3 = _peer("P1"), _peer("P2"), _peer("P3")
    j1 = ulid()
    reg.open(_auth(p1), _body(j1))
    with pytest.raises(RateLimited):
        reg.open(_auth(p1), _body(ulid()))
    reg.open(_auth(p2), _body(ulid(), role="source", mode="pull"))
    with pytest.raises(RateLimited):
        reg.open(_auth(p3), _body(ulid()))
    assert reg.open(_auth(p1), _body(j1))["job_id"] == j1  # re-opening an existing job is not a new one
    for route in (reg.status, reg.heartbeat, reg.cancel):
        with pytest.raises(NotFound):
            route(_auth(p2), j1, {"epoch": 1})
    reg.heartbeat(_auth(p1), j1, {"epoch": 1})
    reg.cancel(_auth(p1), j1, {"epoch": 1, "reason": "user"})
    assert ("abort", j1) in log


def test_l2_source_reclaimed_reaches_both_sides(led, clock, tmp_path, fake_host):
    """W047 (DESIGN §7.7): the source reports ``source_reclaimed`` in its status ``notes`` (a target that dialed
    learns it on its next contact); a source that dialed sends ``{"note": "source_reclaimed"}`` with route #8 and
    the target's registry marks the arrival. The sender is ``jobs.reclaim``; the server's route #8 must hand the
    body to ``ParticipantRegistry.status`` (X18)."""
    job_id = ulid()
    led.create_job(job_id, Role.SOURCE, Mode.PUSH, None, {"role": "source"})
    info = SessionInfo("tip", "root", ("root", "tip"), "T", None, None, None, None, None, 1, (1,))
    led.source_lock(job_id, info, "h", None)
    led.source_set(job_id, "reclaimed")
    src = LocalSource(
        ledger=led,
        store=None,
        db_factory=lambda: None,
        hermes=HermesPaths(home=str(tmp_path), document_cache=str(tmp_path)),
        host=fake_host.host_facts(),
        config={},
        ops=OpRunner(led, None, inline=True),
        git=None,
        calling_session=None,
        session_key=None,
        side_question=None,
        llm=None,
        read_secret=lambda n: None,
        redact=lambda t: t,
        state_db=tmp_path / "state.db",
        remote_driven=False,
        policy=None,
    )
    from ctx_handover.engine.contracts import JobContext

    job = JobContext(job_id, Mode.PUSH, None, Options(), 1, None, lambda: False, clock())
    assert src.status(job).notes == ("source_reclaimed",)

    target_job = ulid()
    reg = ParticipantRegistry(led, lambda role, peer: Api([]), clock=clock)
    p = _peer("P1")
    reg.open(_auth(p), _body(target_job))
    led.arrival_put(
        session_ids=["n1"],
        root_id="n1",
        job_id=target_job,
        peer="p1",
        note="N",
        report_path="",
        path_map=[],
        continue_prompt=None,
    )
    reg.status(_auth(p), target_job, {"epoch": 1, "note": "source_reclaimed"})
    assert led.arrival_for("n1")["source_reclaimed_at"] is not None
