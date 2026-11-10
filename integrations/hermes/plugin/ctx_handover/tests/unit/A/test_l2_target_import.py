"""Export → import through the real ``BundleTarget``/``BundleSource``, the real conductor and the real ``LocalTarget``
(contract X02; layer-2 finding on ``local_target``): the import keeps the bundle's FinalPlan as the plan of
record (id recomputed by the conductor, sent as ``PrepareRequest.final_plan_id``); the target decides destinations
for it on this host — those decisions have their own id and never change the one the pack must belong to.
"""

from __future__ import annotations

import dataclasses
from pathlib import Path

import pytest

from ctx_handover.engine import hostcompat, jobs, session_import
from ctx_handover.engine.jsonutil import from_wire, loads_bounded
from ctx_handover.engine.model import FinalPlan, Mode, Options, Role, SessionRef
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.fakes.sessiondb import SessionBuilder
from ctx_handover.tests.integration.harness import Home, lineage_hashes, session_marks

IMPORT_OPTIONS = Options(refine="off", yes=True, trust_unsigned=True)


def bundle_final(source) -> FinalPlan:
    """The FinalPlan document inside a bundle (manifest ``final_plan_id`` names its object)."""
    raw = source.blocks().object_bytes(source.manifest["final_plan_id"])
    return from_wire(FinalPlan, loads_bounded(raw, max_bytes=64 * 1024 * 1024))


@dataclasses.dataclass
class OneBundleTarget:
    """``jobs.runner_for`` of the active home with the participant pair memoised per job. ``plan_sync`` and
    ``run_foreground`` each build a new pair, and ``BundleTarget`` holds its FinalPlan in memory only, so a CLI
    export ends ``bundle commit before prepare`` (the import side needs no seam)."""

    runner: jobs.JobRunner
    pairs: dict = dataclasses.field(default_factory=dict)

    def __post_init__(self) -> None:
        self._make = self.runner._apis_factory  # test seam of the composition root

        def factory(row):
            if row["job_id"] not in self.pairs:
                self.pairs[row["job_id"]] = self._make(row)
            return self.pairs[row["job_id"]]

        self.runner._apis_factory = factory

    def close(self) -> None:
        self.runner._apis_factory = self._make

    def run(self, mode: Mode, sid: str, options: Options, bundle: Path) -> tuple[str, int]:
        job_id, final = self.runner.plan_sync(
            mode,
            None,
            options,
            session_ref=SessionRef(id=sid),
            calling_session=None,
            session_key=None,
            bundle_path=bundle,
        )
        if final is None:
            return job_id, jobs.exit_code(self.runner.ledger.job(job_id, Role.CONDUCTOR))
        return job_id, self.runner.run_foreground(job_id)


def run_in(home: Home, mode: Mode, sid: str, options: Options, bundle: Path) -> tuple[str, int, OneBundleTarget]:
    with home.active():
        r = OneBundleTarget(jobs.runner_for(None))
        try:
            job_id, rc = r.run(mode, sid, options, bundle)
        finally:
            r.close()
    return job_id, rc, r


def dirty_repo(root: Path) -> RepoBuilder:
    r = RepoBuilder.init(root)
    r.write("README.md", "# app\n")
    r.write("src/main.py", "print('v1')\n")
    r.add_all()
    r.commit("first")
    r.write("src/main.py", "print('v2 in the work tree')\n")
    r.write("notes.txt", "untracked\n")
    return r


def make_bundle(laptop: Home, *, hook_output: bytes | None = None) -> tuple[str, Path, Path, str]:
    """A session with a dirty git working directory (and optionally a hook output file) exported from ``laptop``
    into a bundle file. Returns ``(session id, repo root, bundle path, tree digest)``."""
    laptop.identity()
    repo = dirty_repo(laptop.home / "proj")
    b = SessionBuilder(laptop.db(), clock=FakeClock())
    sid = b.create(title="Bundle me", cwd=str(repo.root))
    b.user(sid, "change src/main.py")
    b.assistant(sid, "changed")
    if hook_output is not None:
        hook = laptop.hermes_home / "hook_outputs" / sid
        hook.mkdir(parents=True)
        (hook / "post-turn.txt").write_bytes(hook_output)
    bundle = laptop.root / "out" / "job.ctxh"
    bundle.parent.mkdir(parents=True, exist_ok=True)
    job_id, rc, _r = run_in(laptop, Mode.EXPORT, sid, Options(refine="off", yes=True), bundle)
    assert rc == 0, (laptop.ledger().job(job_id, Role.CONDUCTOR)["state"], errors(laptop, job_id))
    return sid, repo.root, bundle, tree_digest(repo.root)


def errors(home: Home, job_id: str) -> list:
    return [e["data"] for e in home.ledger().events(job_id, Role.CONDUCTOR) if e["kind"] == "error"]


@pytest.fixture
def two_homes(tmp_path):
    require_upstream()
    jobs._reset_for_tests()  # per-process profile caches of the composition root
    laptop, server = Home(tmp_path, "laptop"), Home(tmp_path, "server")
    yield laptop, server
    jobs._reset_for_tests()


def _arrived(server: Home, job_id: str) -> str:
    rows = [r for r in server.ledger().arrival_rows() if r["job_id"] == job_id]
    assert rows, "no arrival row on the target"
    return rows[0]["session_id"]


def _target_ctx(home: Home, job_id: str) -> dict:
    out: dict = {}
    for e in home.ledger().events(job_id, Role.TARGET):
        if isinstance((e["data"] or {}).get("ctx"), dict):
            out.update(e["data"]["ctx"])
    return out


def test_export_then_import_runner(two_homes):
    """★ X02 (layer-2 finding). The runner path end to end: the pack belongs to the bundle's FinalPlan, the target's own
    destinations (under this host's handover root) are kept and the transcript arrives intact. Mutant (red proof):
    the conductor sends the id negotiated here as ``PrepareRequest.final_plan_id`` (the c32a862d behaviour) — the
    target refuses the pack (``verify_failed``, ``which=final_plan_id``)."""
    laptop, server = two_homes
    sid, _root, bundle, digest = make_bundle(laptop)
    job_i, rc = server.run(Mode.IMPORT, None, SessionRef(id=""), options=IMPORT_OPTIONS, bundle_path=bundle)
    row = server.ledger().job(job_i, Role.CONDUCTOR)
    assert rc == 0, (row["state"], errors(server, job_i))
    new_tip = _arrived(server, job_i)
    assert lineage_hashes(laptop.db(), sid) == lineage_hashes(server.db(), new_tip)
    assert not session_marks(server.db(), new_tip)["hidden"]
    own = from_wire(FinalPlan, _target_ctx(server, job_i)["final_plan"])
    with server.active():
        from ctx_handover.engine.bundle import BundleSource

        src = BundleSource(bundle, trusted_pubkeys=frozenset(), trust_unsigned=True, ledger=server.ledger(), again=True)
        try:
            exported = bundle_final(src)
        finally:
            src.close()
    assert own.plan == exported.plan and own.plan_id != exported.plan_id  # same items, this host's decisions
    dest = Path(next(d.dest for d in own.decisions))
    assert dest.is_relative_to(server.home)
    assert tree_digest(dest) == digest


def test_import_ack_model_and_offline_from_session_import(two_homes):
    """X04: the ack's ``model`` and ``offline_capable`` describe the route the import wrote into the session row
    (B ``plan_import`` + ``offline_capable``) — not the facts of either host."""
    laptop, server = two_homes
    server.write_settings({"refine": "off"}, model={"default": "local-model", "provider": "ollama"})
    _sid, _root, bundle, _digest = make_bundle(laptop)
    job_id, rc = server.run(Mode.IMPORT, None, SessionRef(id=""), options=IMPORT_OPTIONS, bundle_path=bundle)
    assert rc == 0, errors(server, job_id)
    new_tip = _arrived(server, job_id)
    row = server.db().get_session(new_tip)
    with server.active():
        config = hostcompat.load_config()
    cfg = session_import._json_object(row.get("model_config"))  # B's own reader of the column
    ack = _target_ctx(server, job_id)["ack"]
    assert row["model"] == "local-model" and ack["model"] == row["model"]
    assert ack["offline_capable"] is session_import.offline_capable(cfg, config) is True
