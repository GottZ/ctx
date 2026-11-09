"""Layer R (red) for the conductor points of ``reports/L2-contracts.md``:

* V23 — an integrity failure (``block_corrupt``: a manifest that does not hash to the requested id) is fetched once
  more; the second mismatch of the same object ends the job ABORTED with the integrity error and the source free —
  never ``suspended`` (a forged object does not get better by waiting).
* V05 — ``plan.confirmation_reasons(..., cfg=<PolicyConfig of the source>)``: a local ``--include-secret`` asks (d)
  on the composition-root path (``JobRunner.plan_sync(include_secret=…)``).
"""

from __future__ import annotations

import dataclasses

from ctx_handover.engine import jobs
from ctx_handover.engine.model import Mode, Options, Role, SessionRef
from ctx_handover.engine.plan import CONFIRM_SECRET
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.A._l2r_helpers import dirty_repo, session_in
from ctx_handover.tests.unit.A.harness import Harness


def _forge(h: Harness, *, times: int | None):
    """The source answers JSON manifests with a size that does not hash to the requested id — ``times`` requests
    (None = always). Returns the list of requested ids per call."""
    real = h.world.source.blocks_store.manifests
    calls: list[list[str]] = []

    def forged(ids):
        calls.append(list(ids))
        out = real(ids)
        if times is not None and len(calls) > times:
            return out
        return [dataclasses.replace(m, size=m.size + 1) if m.media == "application/json" else m for m in out]

    h.world.source.blocks_store.manifests = forged
    return calls


def test_second_mismatch_of_the_same_object_aborts_with_an_integrity_error(tmp_path, clock):
    """V23: a persistently forged manifest — exactly one refetch, then ABORTED with ``block_corrupt``; nothing
    active at the target, the source released (no lease, no primary lock), the job not ``suspended``."""
    h = Harness(tmp_path, clock)
    calls = _forge(h, times=None)
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    row = h.ledger.job(h.job_id, Role.CONDUCTOR)
    assert row["state"] == "aborted", (row["state"], row["last_error"])
    assert row["last_error"]["code"] == "block_corrupt"
    first = calls[0][0]
    assert sum(ids.count(first) for ids in calls) == 2, calls  # the first fetch and one refetch
    assert not h.world.target.active and h.world.violations == []
    assert not h.world.source.primary_lock and not h.world.source.leases
    assert h.world.source.state == "released"


def test_one_refetch_heals_a_single_mismatch(tmp_path, clock):
    """V23: a mismatch on the first fetch only (a transport fault) is fetched again and the handover completes."""
    h = Harness(tmp_path, clock)
    _forge(h, times=1)
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    h.world.assert_outcome("done")


def test_local_include_secret_asks_reason_d_through_the_runner(tmp_path):
    """V05 on the composition-root path: ``plan_sync(include_secret=…)`` of a local source (export) puts the paths
    into the source's ``PolicyConfig``; the conductor passes that ``cfg`` to ``confirmation_reasons`` and the user
    is asked with reason (d). Without ``include_secret`` (d) is not among the reasons (``--dry-run``: nothing
    is locked either way)."""
    require_upstream()
    jobs._reset_for_tests()
    try:
        home = Home(tmp_path, "laptop")
        home.identity()  # an export signs the bundle
        repo = dirty_repo(home.home / "proj")
        (repo.root / ".env").write_text("TOKEN=abc\n", encoding="utf-8")
        sid = session_in(home, repo.root, "Secret export")
        asked: list[list[str]] = []

        def confirm(info):
            asked.append(list(info["reasons"]))
            return "cancel"

        for include_secret in ((str(repo.root / ".env"),), ()):
            with home.active():
                runner = jobs.runner_for(None)
                _job, final = runner.plan_sync(
                    Mode.EXPORT,
                    None,
                    Options(refine="off", dry_run=True, confirm_above_bytes=1 << 40),
                    session_ref=SessionRef(id=sid),
                    calling_session=None,
                    session_key=None,
                    confirm=confirm,
                    bundle_path=home.root / "out" / f"b{len(asked)}.ctxh",
                    include_secret=include_secret,
                )
            assert final is None
        assert asked and CONFIRM_SECRET in asked[0], asked
        assert len(asked) == 1 or CONFIRM_SECRET not in asked[1], asked
    finally:
        jobs._reset_for_tests()
