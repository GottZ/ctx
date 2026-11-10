"""Layer G — route #9 of a pull source (the layer-2 contract V17, ``source_reclaimed``): what reaches the
dialer's ``RemoteSource.heartbeat`` when the listener's source gave up its leases, and what stays an error.

The listener side runs through the real ``Server`` and A's ``ParticipantRegistry`` (``_l2r_helpers.Pair``; route
#9 is the registry's, contract X18); the dialer side of the mapping is also checked against refusals a listener may
send instead of ``{state}``.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine.errors import HandoverError, InvalidState, LeaseLost, NotFound
from ctx_handover.engine.model import Mode, Role, SessionInfo
from ctx_handover.engine.remote import RemoteSource
from ctx_handover.tests.unit.C._l2r_helpers import Pair, job_context

INFO = SessionInfo("tip", "root", ("root", "tip"), "T", None, None, None, None, None, 1, (1,))


class _LeaseFails:
    """A listener source whose keepalive raises ``LeaseLost`` (the client-only error of ``LocalSource``)."""

    def heartbeat(self, job) -> None:
        raise LeaseLost("the session's turn lease was lost")


def _locked_pull(tmp_path, source) -> tuple[RemoteSource, object]:
    pair = Pair(tmp_path, source=source)
    remote = RemoteSource(pair.client())
    job = job_context(mode=Mode.PULL)
    remote._open(job, Role.SOURCE)
    pair.ledger.source_lock(job.job_id, INFO, "test:1", pair.peer_id)
    return remote, job


def test_route9_keeps_a_lost_lease_of_a_locked_source_an_error(tmp_path):
    """★ Only a source that gave up its leases answers route #9 with its state; a lost turn lease of a source the
    ledger still has ``locked`` stays an error for the dialer — the conductor must not read ``{state: locked}`` as
    a source that is alive.

    Red proof (on A's registry, which owns route #9): mutant = every ``LeaseLost`` answered with the source's ledger
    state → ``heartbeat`` returns.
    """
    remote, job = _locked_pull(tmp_path, _LeaseFails())
    with pytest.raises(HandoverError) as exc:
        remote.heartbeat(job)
    assert not isinstance(exc.value, LeaseLost), exc.value
    assert exc.value.code == "internal"


class _Refusing:
    """A ``WireClient`` stand-in: opening the job succeeds, the keepalive is refused with ``exc``."""

    def __init__(self, exc: HandoverError) -> None:
        self.exc = exc

    def conductor_info(self) -> dict:
        return {}

    def call(self, method: str, route: str, body: dict, *, epoch: int | None = None) -> dict:
        if route == "jobs":
            return {"job_id": body["job_id"], "state": "open", "epoch": 1}
        raise self.exc


def _from_peer(code: str, **details) -> HandoverError:
    envelope = {"code": code, "message": "m", "retryable": False, "details": details}
    return HandoverError.from_wire(409, {"error": envelope})


@pytest.mark.parametrize(
    ("refusal", "state"),
    [
        (InvalidState("the source is reclaimed", reason="state", state="reclaimed"), "reclaimed"),
        (InvalidState("the source is released", state="released"), "released"),
        (_from_peer("source_reclaimed"), "reclaimed"),
    ],
    ids=["invalid-state-reclaimed", "invalid-state-released", "source-reclaimed-code"],
)
def test_a_refused_keepalive_naming_a_lost_source_is_lease_lost(refusal, state):
    """★ V17: a listener that refuses route #9 instead of answering ``{state}`` — a non-retryable refusal naming a
    lost source state, or the code ``source_reclaimed`` — ends the same way at the dialer: ``LeaseLost`` with that
    state (not a retryable error the conductor would keep polling).

    Red proof: mutant = ``RemoteSource.heartbeat`` without the refusal mapping → ``InvalidState``/``internal``.
    """
    remote = RemoteSource(_Refusing(refusal))
    with pytest.raises(LeaseLost) as exc:
        remote.heartbeat(job_context(mode=Mode.PULL))
    assert exc.value.details.get("state") == state


@pytest.mark.parametrize(
    "refusal",
    [
        InvalidState("stale epoch", reason="stale_epoch", state="released"),
        HandoverError("listener busy", state="reclaimed"),
        NotFound("job not found"),
    ],
    ids=["stale-epoch", "retryable", "not-found"],
)
def test_other_keepalive_refusals_stay_what_they_are(refusal):
    """A stale epoch of this conductor, a retryable error and an unknown job are no lost source: they reach the
    conductor unchanged."""
    remote = RemoteSource(_Refusing(refusal))
    with pytest.raises(HandoverError) as exc:
        remote.heartbeat(job_context(mode=Mode.PULL))
    assert type(exc.value) is type(refusal)
