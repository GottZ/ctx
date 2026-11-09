"""Layer R (red) for V29 (A part) of ``reports/L2-contracts.md``: a local ``--include`` of a pull (R18) selects only
within what the source releases — a path under the source's ``export_roots`` arrives, a path the source does not
release is not transferred and named "withheld by source policy"; the pull itself completes (no ``policy_denied``
for the whole plan). The job result names the withheld paths for the final message: ``summary["withheld"]`` of
``runner.status(job_id)`` is a list of paths (contract V29, F reads it). Real listener process, real pairing, real
modules on both sides."""

from __future__ import annotations

import dataclasses

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.model import Mode, PlanHints, SessionRef
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.A._l2r_helpers import OFF, dirty_repo, errors, paired, session_in, state


@pytest.fixture
def homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    yield Home(tmp_path, "server"), Home(tmp_path, "laptop")
    jobs._reset_for_tests()


def test_pull_include_selects_within_what_the_source_releases(homes):
    server, laptop = homes
    shared = server.home / "shared"
    shared.mkdir()
    (shared / "data.txt").write_text("released by export_roots\n", encoding="utf-8")
    private = server.home / "private"
    private.mkdir()
    (private / "keep.txt").write_text("not released\n", encoding="utf-8")
    server.write_settings({"refine": "off", "export_roots": [str(shared)]})
    repo = dirty_repo(server.home / "proj")
    sid = session_in(server, repo.root, "Pull with include")
    hints = PlanHints(include=(str(shared / "data.txt"), str(private / "keep.txt")))
    with paired(server, laptop) as (_listener, peer):
        job_id, rc = laptop.run(Mode.PULL, peer, SessionRef(id=sid), options=dataclasses.replace(OFF, hints=hints))
    assert (state(laptop, job_id), rc) == ("done", 0), errors(laptop, job_id)
    landed = list((laptop.home / "hermes-handover").rglob("data.txt"))
    assert [p.read_text(encoding="utf-8") for p in landed] == ["released by export_roots\n"], landed
    assert not list(laptop.home.rglob("keep.txt"))
    with laptop.active():
        summary = jobs.runner_for(None).status(job_id)["job"]
    withheld = summary.get("withheld")
    assert isinstance(withheld, list) and str(private / "keep.txt") in withheld, summary
