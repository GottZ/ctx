"""R6-A: a job a paired device opens over the wire has the one mode its role has there, and "driven by the other
side" is a fact of this host, never a field the peer sends (review of PR #190, blocking point 1).

* Route #7: a target here is always a ``push`` (the device pushes to this gateway), a source here always a ``pull``
  (the device pulls from it); export, import and offer never cross the wire. Any other ``role``/``mode`` pair is
  refused before a row is written — a push-only device used to open a target as ``pull`` and the target took its
  destinations as typed on this host.
* ``LocalTarget`` treats a job as remote-driven when it serves wire requests (the registry's factory) or when the
  job's row was opened over the wire (no ``params.local``; this also covers rows written before the fix), whatever
  ``mode`` that row carries. The local choice (``--dest``/``--map`` beyond ``allow_roots``) and the unattended
  continuation without ``allow_remote_continue`` stay with this host's own jobs.
"""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.errors import BadRequest, Forbidden, NotFound
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Options, PlanHints, Right, Role
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.fakes.apis_fake import RecordingReporter, make_facts
from ctx_handover.tests.unit.A.test_local_target import Env
from ctx_handover.tests.unit.A.test_participant import RecordingAPI, _auth, _job_id, _open, _peer

WIRE = {"target": "push", "source": "pull"}
OTHER_MODES = {role: [str(m) for m in Mode if str(m) != mode] for role, mode in WIRE.items()}


@pytest.fixture
def reg_env(tmp_path, clock):
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    log: list = []
    reg = ParticipantRegistry(led, lambda role, peer: RecordingAPI(role, peer, log), clock=clock)
    yield led, reg, log
    led.close()


@pytest.mark.parametrize("mode", OTHER_MODES["target"])
def test_push_only_peer_cannot_open_a_target_in_another_mode(reg_env, mode):
    """★ A device holding only the push right opens a target job as ``pull``/``offer``/``export``/``import``: refused
    with ``bad_request`` on ``mode``, no row written, the target API never called.

    Red proof (base ``cab3a492``): ``open`` stores the mode as sent and answers ``open``."""
    led, reg, log = reg_env
    peer = _peer("P1", (Right.PUSH,))
    job_id = _job_id("T" + str(list(Mode).index(Mode(mode))))
    with pytest.raises(BadRequest) as exc:
        _open(reg, _auth(peer), job_id=job_id, role="target", mode=mode)
    assert exc.value.details.get("field") == "mode"
    assert led.job(job_id, Role.TARGET) is None
    assert log == []


@pytest.mark.parametrize("mode", OTHER_MODES["source"])
def test_pull_only_peer_cannot_open_a_source_in_another_mode(reg_env, mode):
    """★ The source side symmetrically: a pull-only device opens a source job only as ``pull`` (an ``export`` source
    titles the locked session "exported to bundle" instead of naming the device that took it)."""
    led, reg, _log = reg_env
    peer = _peer("P2", (Right.PULL,))
    job_id = _job_id("S" + str(list(Mode).index(Mode(mode))))
    with pytest.raises(BadRequest) as exc:
        _open(reg, _auth(peer), job_id=job_id, role="source", mode=mode)
    assert exc.value.details.get("field") == "mode"
    assert led.job(job_id, Role.SOURCE) is None


def test_wire_pairs_still_open(reg_env):
    """The two pairs a conductor really sends stay open (push to a target here, pull from a source here)."""
    led, reg, _log = reg_env
    both = _peer("P3")
    assert _open(reg, _auth(both), job_id=_job_id("W1"), role="target", mode="push")["state"] == "open"
    assert _open(reg, _auth(both), job_id=_job_id("W2"), role="source", mode="pull")["state"] == "open"
    assert led.job(_job_id("W1"), Role.TARGET)["mode"] == "push"
    assert led.job(_job_id("W2"), Role.SOURCE)["mode"] == "pull"


def test_a_row_opened_before_the_fix_in_another_mode_cannot_be_driven(reg_env):
    """★ A wire row written before the fix with a forged mode (target as ``pull``) is no job the device can drive any
    more: every route binding it is refused; the local sweep still ends it (``_internal_job`` needs no bind)."""
    led, reg, _log = reg_env
    peer = _peer("P1", (Right.PUSH,))
    job_id = _job_id("W3")
    params = {"role": "target", "mode": "pull", "conductor": {}, "session_ref": None, "options": {}}
    led.create_job(job_id, Role.TARGET, Mode.PULL, "P1", params)
    with pytest.raises((BadRequest, Forbidden)):
        reg.bind(_auth(peer), job_id, Role.TARGET)


# ---------------------------------------------------------------------------------------------------------
# LocalTarget: remote-driven from the job's origin, not from its mode
# ---------------------------------------------------------------------------------------------------------


def _spy_choose_all(target) -> list:
    """Record the ``local_choice`` of every ``pathmap.choose_all`` call (the stand-in port ignores it)."""
    seen: list = []
    inner = target.ports.pathmap.choose_all

    def choose_all(items, **kw):
        seen.append(kw.get("local_choice"))
        return inner(items, **kw)

    target.ports.pathmap = SimpleNamespace(**{**vars(target.ports.pathmap), "choose_all": choose_all})
    return seen


def _job(e, mode: Mode, options: Options | None = None) -> JobContext:
    return JobContext(
        job_id=e.job.job_id,
        mode=mode,
        peer=None,
        options=options or e.options,
        epoch=1,
        reporter=RecordingReporter(),
        cancelled=lambda: False,
        created_at=e.clock(),
    )


def _wire_row(e, mode: Mode) -> None:
    """The target row exactly as route #7 writes it (``ParticipantRegistry.open``: no ``params.local``)."""
    params = {"role": "target", "mode": str(mode), "conductor": {}, "session_ref": None, "options": {}}
    e.ledger.create_job(e.job.job_id, Role.TARGET, mode, "PEER1", params)


@pytest.fixture
def tgt(tmp_path, clock, session_db, make_session, user_home, hermes_home, fake_host):
    e = Env(tmp_path, clock, session_db, make_session, user_home, hermes_home, mode=Mode.PULL)
    yield e
    e.ledger.close()


@pytest.mark.parametrize("mode", [Mode.PULL, Mode.IMPORT, Mode.EXPORT, Mode.OFFER])
def test_wire_row_in_another_mode_is_remote_driven(tgt, mode):
    """★ A target row route #7 opened (a row from before the fix may carry any mode): the destinations come from
    the other side (``local_choice=False``) and a continuation names the device that asked, so it needs
    ``allow_remote_continue`` here.

    Red proof (base ``cab3a492``): ``_remote_driven`` is ``mode is PUSH and source_peer_id``: ``local_choice=True``,
    ``requested_by=None``."""
    e = tgt
    _wire_row(e, mode)
    job = _job(e, mode)
    seen = _spy_choose_all(e.target)
    e.target.facts(job, make_facts("laptop"))
    e.target.negotiate(job, e.plan())
    assert seen == [False]
    assert e.target._remote_driven(job) is True


def test_wire_instance_is_remote_driven_whatever_the_row(tgt):
    """★ The instance the registry's factory builds (``remote_driven=True``) treats every job it serves as driven
    by the other side, also one whose row says ``local`` and whose mode is ``pull``."""
    e = tgt
    e.target.remote_driven = True
    job = _job(e, Mode.PULL)
    e.target.open_job(job)  # the row of a local job (``params.local``)
    seen = _spy_choose_all(e.target)
    e.target.facts(job, make_facts("laptop"))
    e.target.negotiate(job, e.plan())
    assert seen == [False]


@pytest.mark.parametrize("mode", [Mode.PULL, Mode.IMPORT])
def test_local_jobs_keep_the_local_choice(tgt, mode):
    """The target of this host's own pull or import (row with ``params.local``, not a wire instance) keeps
    ``--dest``/``--map`` as typed here — also with a peer bound to it (pull, verified bundle signer)."""
    e = tgt
    dest = str(e.home / "elsewhere" / "app")
    job = _job(e, mode, Options(hints=PlanHints(dest_overrides=(("/srv/app", dest),))))
    e.target.open_job(job)
    seen = _spy_choose_all(e.target)
    e.target.facts(job, make_facts("server"))
    e.target.negotiate(job, e.plan())
    assert seen == [True]
    assert e.target._remote_driven(job) is False


def test_wire_row_in_another_mode_continuation_names_the_device(tgt):
    """★ After the commit of a wire job whose row says ``pull``: the continuation is a remote request (named, and
    refused without ``allow_remote_continue`` by the real ``Spawner``)."""
    e = tgt
    _wire_row(e, Mode.PULL)
    e.job = _job(e, Mode.PULL)
    e.prepared()
    assert e.wait(e.target.start_commit(e.job)).state.value == "done"
    e.target.continue_session(e.job, "go on")
    assert e.spawner.spawned[-1][3] == "server"


def test_a_local_row_with_the_devices_id_is_no_job_it_drives(reg_env):
    """★ The planning row of an offer carries the device's id and ``params.local`` (its id is the offer id the device
    reads from route #5): every route binding it answers ``not_found`` (Fremd-Review R6 finding 6).

    Red proof (base ``cab3a492``): ``bind`` serves the row; the device drives this host's own job."""
    led, reg, _log = reg_env
    peer = _peer("P1", (Right.PULL,))
    job_id = _job_id("W4")
    led.create_job(job_id, Role.SOURCE, Mode.OFFER, "P1", {"role": "source", "local": True})
    with pytest.raises(NotFound):
        reg.bind(_auth(peer), job_id, Role.SOURCE)
