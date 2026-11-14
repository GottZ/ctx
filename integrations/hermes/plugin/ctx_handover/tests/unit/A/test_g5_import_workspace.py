"""Integrator decision on R36a: the bundle import does not ask reason (j) ``workspace_differs`` again.

R36a (DECISIONS): "Beim Bündel-Import wird Grund j nicht erneut gefragt, wenn der Export ihn bestätigt hat." The
export asks (j) — ``--yes`` does not answer it (``plan.YES_EXEMPT``) — so a bundle exists only after a person (or an
explicit answer) confirmed the workspace; its files are in the bundle. The import drops (j) like ``proposed`` (X02);
every other import reason (unverified bundle, instruction files, a session over the target window …) stays.

End to end with the real modules (``tests/integration/harness.Home``, ``jobs.runner_for``): the session's working
folder ``notes/today`` of the repository ``~/proj`` holds no tracked file (R36a ⇒ the repository is the workspace and
(j) is raised); the export is confirmed with ``yes``; the import runs with ``--yes`` and no person."""

from __future__ import annotations

from pathlib import Path

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine import plan as planning
from ctx_handover.engine.model import Mode, Options, Role, SessionRef
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.fakes.sessiondb import SessionBuilder


@pytest.fixture
def two_homes(tmp_path):
    require_upstream()
    from ctx_handover.tests.integration.harness import Home

    jobs._reset_for_tests()
    yield Home(tmp_path, "laptop"), Home(tmp_path, "server")
    jobs._reset_for_tests()


def _run(home, mode: Mode, sid: str, options: Options, bundle: Path, confirm=None) -> tuple[str, int, list[dict]]:
    """``plan_sync`` (with ``confirm`` as the person) + ``run_foreground``; one participant pair per job. Returns the
    job, its exit code and the questions asked."""
    asked: list[dict] = []

    def person(info: dict):
        asked.append(info)
        return confirm(info) if confirm is not None else None

    with home.active():
        runner = jobs.runner_for(None)
        make, pairs = runner._apis_factory, {}
        runner._apis_factory = lambda row: pairs.setdefault(row["job_id"], make(row))
        try:
            job_id, final = runner.plan_sync(
                mode,
                None,
                options,
                session_ref=SessionRef(id=sid),
                calling_session=None,
                session_key=None,
                bundle_path=bundle,
                confirm=person if confirm is not None else None,
            )
            if final is None:
                return job_id, jobs.exit_code(runner.ledger.job(job_id, Role.CONDUCTOR)), asked
            return job_id, runner.run_foreground(job_id), asked
        finally:
            runner._apis_factory = make


def _ctx(home, job_id: str) -> dict:
    out: dict = {}
    for e in home.ledger().events(job_id, Role.CONDUCTOR):
        if isinstance((e["data"] or {}).get("ctx"), dict):
            out.update(e["data"]["ctx"])
    return out


def _export_with_a_confirmed_workspace(laptop, *, agents: bool = False) -> tuple[Path, Path]:
    repo = RepoBuilder.init(laptop.home / "proj")
    repo.write("README.md", "# app\n")
    if agents:
        repo.write("AGENTS.md", "Always run the tests.\n")
    repo.write("src/app.py", "print('app')\n")
    repo.commit_all("first")
    today = repo.path("notes/today")
    today.mkdir(parents=True)
    (today / "draft.md").write_text("the session's own draft\n")  # untracked, not ignored: R36a asks (j)
    b = SessionBuilder(laptop.db(), clock=FakeClock())
    sid = b.create(title="Notes", cwd=str(today))
    b.user(sid, "write the draft")
    b.assistant(sid, "done")
    bundle = laptop.root / "out" / "notes.ctxh"
    bundle.parent.mkdir(parents=True)
    job_x, rc, asked = _run(laptop, Mode.EXPORT, sid, Options(refine="off", yes=True), bundle, lambda info: "yes")
    errors = [e["data"] for e in laptop.ledger().events(job_x, Role.CONDUCTOR) if e["kind"] == "error"]
    assert rc == 0, errors
    assert [planning.CONFIRM_WORKSPACE_DIFFERS in q["reasons"] for q in asked] == [True], asked
    return bundle, Path(repo.root)


def test_g5_import_does_not_ask_the_workspace_the_export_confirmed(two_homes):
    """★ R36a (integrator decision): ``import --yes`` without a person imports a bundle whose export confirmed
    reason (j); the draft from the session's folder arrives.

    Red on ``08cfe0f0``: the import ended at the confirmation — ``plan_sync`` returned no plan, the job ``aborted``
    with ``ended_by: cancel`` and reasons ``['workspace_differs']`` (exit 1), nothing imported.

    Mutant: ``IMPORT_CONFIRMED`` holds only ``proposed`` (the old filter)."""
    laptop, server = two_homes
    laptop.identity()
    server.identity()
    bundle, _repo = _export_with_a_confirmed_workspace(laptop)
    job_i, rc, asked = _run(
        server, Mode.IMPORT, "", Options(refine="off", yes=True, trust_unsigned=True), bundle, confirm=None
    )
    row = server.ledger().job(job_i, Role.CONDUCTOR)
    assert rc == 0 and row["state"] == "done", (row["state"], row.get("last_error"), _ctx(server, job_i))
    assert not asked
    assert planning.CONFIRM_WORKSPACE_DIFFERS not in (_ctx(server, job_i).get("reasons") or [])
    drafts = [p for p in server.home.rglob("draft.md") if p.read_text() == "the session's own draft\n"]
    assert drafts, "the confirmed workspace did not arrive"


def _ended_reasons(home, job_id: str) -> list[str] | None:
    for e in home.ledger().events(job_id, Role.CONDUCTOR):
        data = e["data"] or {}
        if data.get("event") == "planning_ended":
            return list(data.get("reasons") or [])
    return None


def test_g5_import_keeps_every_other_reason(two_homes):
    """★ Only (j) is dropped at the import: the same kind of bundle with an ``AGENTS.md`` in the workspace, imported
    unverified (``trust_unsigned``, no paired origin), still raises (g) ``instruction_files`` (DESIGN §8.7, SEC 4.3);
    without ``--yes`` and without a person the import ends at the confirmation with exactly that reason — (j) is not
    among them, nothing is imported.

    Red on ``08cfe0f0``: ``AssertionError`` — the reasons were ``['instruction_files', 'workspace_differs']``.
    Mutant: the import filter drops every reason (``reasons = []``) → the import goes ahead (``done``)."""
    laptop, server = two_homes
    laptop.identity()
    server.identity()
    bundle, _repo = _export_with_a_confirmed_workspace(laptop, agents=True)
    job_i, rc, _asked = _run(
        server, Mode.IMPORT, "", Options(refine="off", yes=False, trust_unsigned=True), bundle, confirm=None
    )
    row = server.ledger().job(job_i, Role.CONDUCTOR)
    assert row["state"] == "aborted" and rc != 0, (row["state"], row.get("last_error"))
    assert _ended_reasons(server, job_i) == [planning.CONFIRM_INSTRUCTIONS], _ended_reasons(server, job_i)
    assert not [p for p in server.home.rglob("draft.md")]
