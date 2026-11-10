"""Helpers of the layer-R tests for cluster A (``test_l2r_*.py`` in this directory).

Homes, listener and pairing come from ``tests/integration/harness.py`` (real ``SessionDB``, git, block store,
listener process); journals are read the way the conductor and ``LocalTarget`` write them (``ctx`` events)."""

from __future__ import annotations

import contextlib
import dataclasses
from collections.abc import Iterator
from pathlib import Path

from ctx_handover.engine import jobs
from ctx_handover.engine.jsonutil import from_wire
from ctx_handover.engine.model import FinalPlan, Mode, Options, Role, SessionRef
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.fakes.sessiondb import SessionBuilder
from ctx_handover.tests.integration.harness import Home, Listener, pair

OFF = Options(refine="off", yes=True, stall_timeout_s=20)
"""No refinement, ``--yes``; a job that cannot resolve ``in_doubt`` parks after 20 s instead of the default 30 min."""


def dirty_repo(root: Path) -> RepoBuilder:
    """A repository with a commit, a modified tracked file and an untracked file."""
    r = RepoBuilder.init(root)
    r.write("README.md", "# app\n")
    r.write("src/main.py", "print('v1')\n")
    r.add_all()
    r.commit("first")
    r.write("src/main.py", "print('v2 in the work tree')\n")
    r.write("notes.txt", "untracked\n")
    return r


def session_in(home: Home, cwd: Path, title: str) -> str:
    b = SessionBuilder(home.db(), clock=FakeClock())
    sid = b.create(title=title, cwd=str(cwd))
    b.user(sid, "please change src/main.py")
    b.assistant(sid, "done, see src/main.py")
    return sid


def ctx_of(home: Home, job_id: str, role: Role) -> dict:
    """The journaled context of one job side (all ``ctx`` dicts of its events, in order)."""
    out: dict = {}
    for e in home.ledger().events(job_id, role):
        data = e["data"] or {}
        if isinstance(data.get("ctx"), dict):
            out.update(data["ctx"])
    return out


def errors(home: Home, job_id: str) -> list:
    return [e["data"] for e in home.ledger().events(job_id, Role.CONDUCTOR) if e["kind"] == "error"]


def state(home: Home, job_id: str) -> str:
    return home.ledger().job(job_id, Role.CONDUCTOR)["state"]


def own_final(home: Home, job_id: str) -> FinalPlan:
    """The FinalPlan the target side of ``job_id`` holds on ``home`` (its decisions are this host's)."""
    return from_wire(FinalPlan, ctx_of(home, job_id, Role.TARGET)["final_plan"])


def note_of(home: Home, job_id: str) -> str:
    return str(ctx_of(home, job_id, Role.TARGET).get("note") or "")


@contextlib.contextmanager
def paired(server: Home, laptop: Home) -> Iterator[tuple[Listener, object]]:
    """``server`` served by a listener process, ``laptop`` paired with it (dial peer ``server`` on the laptop,
    inbound peer ``laptop`` with the laptop's key on the server)."""
    server.identity()
    listener = Listener(server)
    try:
        yield listener, pair(server, laptop, listener)
    finally:
        listener.stop()


@dataclasses.dataclass
class PinnedPairs:
    """``jobs.runner_for`` of the active home with the participant pair memoised per job — the seam the layer-2 A
    tests use to keep one ``BundleTarget`` across ``plan_sync`` and ``run_foreground`` (V24 open). Used here only
    where V24 is not the subject, so a V13/V09 test is not red because of V24."""

    runner: jobs.JobRunner
    pairs: dict = dataclasses.field(default_factory=dict)

    def __post_init__(self) -> None:
        self._make = self.runner._apis_factory

        def factory(row):
            if row["job_id"] not in self.pairs:
                self.pairs[row["job_id"]] = self._make(row)
            return self.pairs[row["job_id"]]

        self.runner._apis_factory = factory

    def close(self) -> None:
        self.runner._apis_factory = self._make


def export_pinned(home: Home, sid: str, bundle: Path, options: Options = OFF) -> tuple[str, int]:
    """``plan_sync`` + ``run_foreground`` of an export with one ``BundleTarget`` per job (see ``PinnedPairs``)."""
    bundle.parent.mkdir(parents=True, exist_ok=True)
    with home.active():
        pinned = PinnedPairs(jobs.runner_for(None))
        try:
            job_id, final = pinned.runner.plan_sync(
                Mode.EXPORT,
                None,
                options,
                session_ref=SessionRef(id=sid),
                calling_session=None,
                session_key=None,
                bundle_path=bundle,
            )
            if final is None:
                return job_id, jobs.exit_code(pinned.runner.ledger.job(job_id, Role.CONDUCTOR))
            return job_id, pinned.runner.run_foreground(job_id)
        finally:
            pinned.close()
