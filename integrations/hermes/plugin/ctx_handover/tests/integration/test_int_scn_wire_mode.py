"""R6-A over the real listener process: a paired device that holds only the push right must not reach a write outside
``allow_roots`` by opening its target job in another mode (review of PR #190, blocking point 1).

The laptop runs the product's own push conductor against the server's listener; the one forged thing is the ``mode``
of route #7 (``remote._Remote._open``), so the chain is the real one: open → facts → negotiate → blocks → prepare →
commit. The push carries a ``--map`` proposal for its workspace that points outside the server's ``allow_roots``
(``hints.dest_overrides`` travel to the target; DESIGN R19: typed on the other side ⇒ a proposal under
``allow_roots`` only). Before the fix the server took a target opened as ``pull``/``import``/``export``/``offer`` as a
local job and wrote the workspace there."""

from __future__ import annotations

import dataclasses
import os

import pytest

from ctx_handover.engine import remote
from ctx_handover.engine.errors import BadRequest
from ctx_handover.engine.model import Mode, Options, PlanHints, Right, SessionRef
from ctx_handover.tests.integration._int3_helpers import arrival, run_job, session_in, wip_repo
from ctx_handover.tests.integration.harness import Listener, pair


@pytest.fixture
def push_only(homes):
    """``(server, laptop, listener, peer)``: the laptop is paired with the push right only."""
    server, laptop = homes
    server.identity()
    listener = Listener(server)
    try:
        yield server, laptop, listener, pair(server, laptop, listener, rights=(Right.PUSH,))
    finally:
        listener.stop()


def _push_with_map(server, laptop, peer, name: str):
    repo = wip_repo(laptop.home / name)
    sid = session_in(laptop, repo.root, f"Wire mode {name}")
    outside = server.home / "elsewhere" / name
    options = Options(refine="off", yes=True, hints=PlanHints(dest_overrides=((str(repo.root), str(outside)),)))
    return run_job(laptop, Mode.PUSH, peer, SessionRef(id=sid), options=options), outside


def _landed(server, res) -> str | None:
    row = arrival(server, res.job_id)
    if row is None:
        return None
    return (server.db().get_session(row["session_id"]) or {}).get("cwd")


def test_int_push_map_outside_allow_roots_stays_a_proposal(push_only):
    """Control: the honest push with the same map lands under the server's ``allow_roots``, not at the map."""
    server, laptop, _listener, peer = push_only
    res, outside = _push_with_map(server, laptop, peer, "honest")
    assert res.rc == 0, res.why()
    assert not os.path.lexists(outside), "a proposal from the other side left allow_roots"
    cwd = _landed(server, res)
    assert cwd is not None and cwd.startswith(str(server.home / "hermes-handover")), cwd


@pytest.mark.parametrize("forged", [Mode.PULL, Mode.IMPORT, Mode.EXPORT, Mode.OFFER])
def test_int_push_only_peer_forging_the_mode_writes_nothing_outside(push_only, monkeypatch, forged):
    """★ The same push with route #7's ``mode`` forged: the listener refuses the open (``bad_request`` on ``mode``),
    nothing lands at the map outside ``allow_roots`` and no session arrives.

    Red proof (base ``cab3a492``, basetemp outside ``/root`` so that the test home is no denied path of the host's
    own home): the job completes (rc 0) and the workspace is written to ``<server home>/elsewhere/<name>`` — the
    server took the device's map as typed on its own console."""
    server, laptop, _listener, peer = push_only
    real_open = remote._Remote._open

    def forged_open(self, job, role):
        return real_open(self, dataclasses.replace(job, mode=forged), role)

    monkeypatch.setattr(remote._Remote, "_open", forged_open)
    name = f"forged-{forged}"
    outside = server.home / "elsewhere" / name
    res, refused = None, None
    try:
        res, _outside = _push_with_map(server, laptop, peer, name)
    except BadRequest as exc:  # the listener's refusal of route #7 ends the conductor's planning (plan_sync)
        refused = exc
    assert not os.path.lexists(outside), (
        f"target opened as {forged} by a push-only device wrote outside allow_roots (rc {res.rc if res else None}, "
        f"cwd {_landed(server, res) if res else None})"
    )
    assert refused is not None, f"the forged open went through (rc {res.rc if res else None})"
    assert refused.details.get("field") == "mode", refused.details
    assert server.ledger().arrival_rows() == []
