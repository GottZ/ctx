"""Round G‴ of cluster E — L3-F08: a target restarted in TARGET_PREPARING resumes (DESIGN §3.6 "resume from the
journal", §3.5/I9), and an abort after a PREPARE that was cut short leaves nothing behind (§3.2, §3.6 "expiry runs
the target abort (delete … partials …)").

PREPARE writes outside the staging: the partial directory of a fresh item (``<dest parent>/.ctxh-<job6>``), the
staged file of an extra file (``.<name>.ctxh-<job6>``) and the staged files of a tree update beside their
destination. Before the fix a second PREPARE of the same job failed for good on its own partial
(``FileExistsError``, retried as ``internal`` until the job parked), a tree update fell back to a fresh copy because
its own staged files looked like a foreign change, and an abort without a journaled snapshot left all of it in the
user's directories. Now PREPARE records what it is about to create under ``staging/ws-steps/`` (before it exists,
its identity right after): what this job left is dropped and prepared again, what it did not leave is never
touched (``ConflictError(kind=partial_exists)`` with a clear message), and ``discard`` drops the recorded leftovers
also when no snapshot names them. Sweep (same class): ``gitapply.transfer_pack``'s temp name carried only the pid
— a restarted process with the same pid (containers) failed on the dead process's temp file.

The process tests run ``_x11_driver`` (real processes, ``os._exit`` at the named point)."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitapply, gitops, materialize
from ctx_handover.engine.errors import ConflictError
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.model import DestMode, Include, ItemKind, ItemRole, Origin, PlanItem, TargetDecision
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.unit.E._support import LINUX, assert_same_state, build_mixed, pack
from ctx_handover.tests.unit.E.test_x11_resume import FORWARD_CRASHES, Scenario

JOB = "01JC0G3E0000000000000ABCDE"
JOB6 = JOB[-6:].lower()


class _Cut(BaseException):
    """A process that dies at this point (in-process stand-in: nothing after it runs, no cleanup of the dead
    process's own making is expected)."""


def _plan_item(kind: ItemKind, source: Path, item_id: str = "w0") -> PlanItem:
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=str(source),
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=1000,
    )


def _txn(git, store, cfg, staging: Path) -> materialize.WorkspaceTxn:
    return materialize.WorkspaceTxn(JOB, staging, git, store, lambda *a: None, cfg=cfg, now=lambda: 1_762_646_400.0)


def _git_item(git, user_home, staging, cfg, store):
    src = build_mixed(user_home / "src")
    _, item = pack(git, src.root, staging / "src-pack", cfg, store)
    return src, _plan_item(ItemKind.GIT_REPO, src.root), item


def _tree_item(user_home, cfg, store, name: str = "notes"):
    src = user_home / name
    for rel, text in {"a.md": "a\n", "sub/b.md": "b\n"}.items():
        (src / rel).parent.mkdir(parents=True, exist_ok=True)
        (src / rel).write_text(text)
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    pitem = {"item_id": "t0", "kind": "file_tree", "manifest": filetree.pack_tree(entries, src, store)}
    return src, _plan_item(ItemKind.FILE_TREE, src, "t0"), pitem


def _fresh(item: PlanItem, dest: Path) -> TargetDecision:
    return TargetDecision(item.item_id, str(dest), DestMode.FRESH, "workspace-root")


def _leftovers(root: Path) -> list[str]:
    return sorted(str(p.relative_to(root)) for p in root.rglob("*") if ".ctxh-" in p.name)


# ---------------------------------------------------------------------------------------------------------
# The partial directory itself (gitapply / filetree): a clear refusal instead of FileExistsError
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("kind", ["git", "tree"])
def test_prepare_fresh_refuses_an_existing_partial_with_a_clear_message(
    git, user_home, staging, cfg, store, handover_root, kind
):
    """★ L3-F08 (lower half) — ``prepare_fresh`` never writes into or removes a directory that is already at the
    partial path: it refuses with ``ConflictError(kind=partial_exists)`` naming the path and what to do (before:
    ``FileExistsError``, an ``internal`` error the conductor retried until the job parked)."""
    partial = handover_root / ".ctxh-in-the-way"
    partial.mkdir()
    (partial / "keep.txt").write_text("someone's\n")
    with pytest.raises(ConflictError) as got:
        if kind == "git":
            _, _, item = _git_item(git, user_home, staging, cfg, store)
            gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
        else:
            _, _, pitem = _tree_item(user_home, cfg, store)
            filetree.prepare_fresh(store, pitem["manifest"], partial, LINUX, dest=handover_root / "notes")
    err = got.value
    assert err.details.get("kind") == "partial_exists"
    assert str(partial) in err.message and "remove it" in err.message
    assert err.retryable is False
    assert sorted(os.listdir(partial)) == ["keep.txt"] and (partial / "keep.txt").read_text() == "someone's\n"


# ---------------------------------------------------------------------------------------------------------
# WorkspaceTxn: own leftovers are dropped and prepared again, foreign ones are refused untouched
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("kind", ["git", "tree"])
def test_second_prepare_of_the_same_job_drops_the_partial_the_first_one_left(
    git, user_home, tmp_path, cfg, store, handover_root, kind
):
    """★ L3-F08 — the first PREPARE of the job prepared the item and its process died before it journaled the
    snapshot (the target restarted in TARGET_PREPARING); the next PREPARE of the same job in a new process drops
    that partial, prepares again and commits — the result equals an uninterrupted run, nothing is left beside the
    destination (before: ``FileExistsError: … .ctxh-<job6>``)."""
    staging = tmp_path / "staging"
    if kind == "git":
        src, item, pitem = _git_item(git, user_home, tmp_path, cfg, store)
    else:
        src, item, pitem = _tree_item(user_home, cfg, store)
    dest = handover_root / "dest"
    first = _txn(git, store, cfg, staging)
    first.prepare_item(item, _fresh(item, dest), pitem)
    assert _leftovers(handover_root) == [f".ctxh-{JOB6}"]
    second = _txn(git, store, cfg, staging)
    second.prepare_item(item, _fresh(item, dest), pitem)
    for _name, forward, _inverse in second.commit_steps():
        forward()
    second.discard()
    if kind == "git":
        assert_same_state(src, RepoBuilder(dest), excluded=[b".env"], ignored=[b"ignored", b"build.log"])
    else:
        assert tree_digest(dest) == tree_digest(src)
    assert sorted(os.listdir(handover_root)) == ["dest"]
    assert not (staging / materialize.STEP_DIR).exists()


@pytest.mark.parametrize("what", ["unrecorded", "other_directory", "symlink"])
def test_a_directory_this_job_did_not_leave_at_the_partial_path_is_refused_untouched(
    git, user_home, tmp_path, cfg, store, handover_root, what
):
    """★ L3-F08 "foreign ⇒ clear message" — something at ``<dest parent>/.ctxh-<job6>`` that this job's records do
    not name with its identity (no record; a record of a directory that is gone and was replaced; a symlink to a
    directory) is neither written into nor removed: ``ConflictError(kind=partial_exists)`` names it."""
    staging = tmp_path / "staging"
    _, item, pitem = _tree_item(user_home, cfg, store)
    dest = handover_root / "dest"
    partial = handover_root / f".ctxh-{JOB6}"
    elsewhere = user_home / "elsewhere"
    elsewhere.mkdir()
    (elsewhere / "mine.txt").write_text("keep\n")
    if what == "unrecorded":
        partial.mkdir()
        (partial / "mine.txt").write_text("keep\n")
    elif what == "other_directory":
        _txn(git, store, cfg, staging).prepare_item(item, _fresh(item, dest), pitem)
        os.rename(partial, handover_root / "moved-away")
        partial.mkdir()
        (partial / "mine.txt").write_text("keep\n")
    else:
        partial.symlink_to(elsewhere)
    with pytest.raises(ConflictError) as got:
        _txn(git, store, cfg, staging).prepare_item(item, _fresh(item, dest), pitem)
    assert got.value.details.get("kind") == "partial_exists"
    assert str(partial) in got.value.message
    if what == "symlink":
        assert partial.is_symlink() and os.readlink(partial) == str(elsewhere)
    else:
        assert sorted(os.listdir(partial)) == ["mine.txt"]
    assert sorted(os.listdir(elsewhere)) == ["mine.txt"]
    assert not dest.exists()


@pytest.mark.parametrize("touched", [False, True], ids=["empty", "written_into_since"])
def test_partial_created_but_not_yet_identified_counts_as_own_only_while_empty(
    git, user_home, tmp_path, cfg, store, handover_root, monkeypatch, touched
):
    """★ The window between ``mkdir`` of the partial and the record of its identity: the process dies there. The
    record (written before ``mkdir``) names the path; an empty directory there is this job's and is replaced, one
    that holds anything is not (nothing of this job is written before the identity is recorded)."""
    staging = tmp_path / "staging"
    _, item, pitem = _tree_item(user_home, cfg, store)
    dest = handover_root / "dest"
    real_put = materialize._StepLog.put
    calls = {"n": 0}

    def put(self, name, data):
        calls["n"] += 1
        if calls["n"] == 2:  # 1: the path before mkdir, 2: its identity after mkdir
            raise _Cut
        return real_put(self, name, data)

    monkeypatch.setattr(materialize._StepLog, "put", put)
    with pytest.raises(_Cut):
        _txn(git, store, cfg, staging).prepare_item(item, _fresh(item, dest), pitem)
    monkeypatch.undo()
    partial = handover_root / f".ctxh-{JOB6}"
    assert partial.is_dir() and os.listdir(partial) == []
    if touched:
        (partial / "late.txt").write_text("not the job's\n")
        with pytest.raises(ConflictError) as got:
            _txn(git, store, cfg, staging).prepare_item(item, _fresh(item, dest), pitem)
        assert got.value.details.get("kind") == "partial_exists"
        assert (partial / "late.txt").read_text() == "not the job's\n"
        return
    txn = _txn(git, store, cfg, staging)
    txn.prepare_item(item, _fresh(item, dest), pitem)
    for _name, forward, _inverse in txn.commit_steps():
        forward()
    assert sorted(os.listdir(handover_root)) == ["dest"]


def test_discard_without_a_snapshot_drops_what_a_cut_prepare_created(
    git, user_home, tmp_path, cfg, store, handover_root
):
    """★ L3-F08 (abort side) — the job's PREPARE was cut after it created parent directories, a partial and a
    staged extra file, before any snapshot was journaled; the abort rebuilds a transaction without items and
    discards it. Everything PREPARE created goes — partial, staged file, the parents it made — and nothing else
    (before: all of it stayed in the user's directories)."""
    staging = tmp_path / "staging"
    _, item, pitem = _git_item(git, user_home, tmp_path, cfg, store)
    todo = user_home / "todo.txt"
    todo.write_text("todo\n")
    e1 = {"item_id": "e1", **filetree.pack_file(todo, store)}
    e_item = _plan_item(ItemKind.EXTRA_FILE, todo, "e1")
    (handover_root / "keep.txt").write_text("the user's\n")
    before = sorted(str(p.relative_to(handover_root)) for p in handover_root.rglob("*"))
    first = _txn(git, store, cfg, staging)
    first.prepare_item(item, _fresh(item, handover_root / "a" / "b" / "app"), pitem)
    first.prepare_item(e_item, TargetDecision("e1", str(handover_root / "x" / "todo.txt"), DestMode.FRESH, "user"), e1)
    assert _leftovers(handover_root) == [f"a/b/.ctxh-{JOB6}", f"x/.todo.txt.ctxh-{JOB6}"]
    _txn(git, store, cfg, staging).discard()
    assert sorted(str(p.relative_to(handover_root)) for p in handover_root.rglob("*")) == before
    assert not (staging / materialize.STEP_DIR).exists()


# ---------------------------------------------------------------------------------------------------------
# Sweep: the same class at other fixed names (tree update staged files, transfer_pack temp file)
# ---------------------------------------------------------------------------------------------------------


def _tree_update_setup(user_home, cfg, store, handover_root):
    src, _, pitem0 = _tree_item(user_home, cfg, store)
    dest = handover_root / "notes"
    first = filetree.prepare_fresh(store, pitem0["manifest"], handover_root / ".ctxh-n", LINUX, dest=dest)
    fp = filetree.commit(first)["fingerprint"]
    (src / "a.md").write_text("a, changed\n")
    (src / "c.md").write_text("new\n")
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    return src, dest, fp, filetree.pack_tree(entries, src, store)


def test_tree_update_prepared_again_reuses_its_own_staged_files(user_home, cfg, store, handover_root, tmp_path):
    """★ Sweep (L3-F08 class) — a tree update's PREPARE stages ``.<name>.ctxh-<tag>`` beside the destination files.
    A second PREPARE of the same job (the first one's process died) must not take those for a foreign change (before:
    ``ConflictError(foreign, "new file .a.md.ctxh-…")`` ⇒ the item fell back to a fresh copy beside the user's
    tree, and the staged files stayed); a leftover the second run does not stage again is removed; anyone else's
    file — another tag, another name — is still a foreign change."""
    src, dest, fp, mid = _tree_update_setup(user_home, cfg, store, handover_root)
    filetree.prepare_update(store, mid, dest, fp, LINUX, tmp_path / "b1", tag=JOB6)
    stale = dest / f".gone.md.ctxh-{JOB6}"
    stale.write_text("left by this job's earlier run\n")
    state = filetree.prepare_update(store, mid, dest, fp, LINUX, tmp_path / "b2", tag=JOB6)
    assert not stale.exists()
    filetree.commit(state)
    assert tree_digest(dest) == tree_digest(src)
    assert _leftovers(dest) == []
    (dest / f".a.md.ctxh-{JOB6}x").write_text("someone else's\n")
    with pytest.raises(ConflictError) as got:
        filetree.prepare_update(store, mid, dest, state.fingerprint, LINUX, tmp_path / "b3", tag=JOB6)
    assert got.value.details.get("kind") == "foreign"


def test_transfer_pack_is_not_stopped_by_the_temp_file_of_a_dead_process_with_the_same_pid(
    git, user_home, tmp_path, cfg, store
):
    """★ Sweep (L3-F08 class) — ``transfer_pack`` named its temp file ``.tmp-ctxh-<pid>-<pack>`` and opened it
    ``O_EXCL``: a restarted process with the same pid (a container's main process) failed on the temp file of the
    one that died (before: ``FileExistsError``). The leftover is not touched; the pack lands complete."""
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    _, item = pack(git, src.root, tmp_path / "pack", cfg, store)
    q = gitapply.quarantine_and_verify(git, store, item, tmp_path / "q", alternates=None)
    dest = RepoBuilder.init(user_home / "dest")
    names = sorted(p.name for p in (q / "objects" / "pack").iterdir() if p.name.endswith(".pack"))
    pack_dir = dest.root / ".git" / "objects" / "pack"
    pack_dir.mkdir(parents=True, exist_ok=True)
    stale = pack_dir / f".tmp-ctxh-{os.getpid()}-{names[0]}"
    stale.write_bytes(b"half a pack of a dead process")
    gitapply.transfer_pack(q, dest.root / ".git")
    assert (pack_dir / names[0]).read_bytes() == (q / "objects" / "pack" / names[0]).read_bytes()
    assert stale.read_bytes() == b"half a pack of a dead process"
    dest.run("cat-file", "-e", item["head"]["commit"])


# ---------------------------------------------------------------------------------------------------------
# Process tests: target restarted in TARGET_PREPARING (resume and abort)
# ---------------------------------------------------------------------------------------------------------


PREPARE_CRASHES = {
    # w0 is the scenario's fresh git item and the first one prepared
    "git_partial_just_created": {
        "at": "call",
        "target": "gitops.Git.run",
        "argv": ["init", "-q", "--template="],
        "nth": 1,
        "when": "before",
    },
    "git_partial_checked_out": {"at": "call", "target": "gitapply._store_stash", "nth": 1, "when": "before"},
    "git_partial_complete": {"at": "call", "target": "gitapply.prepare_update", "nth": 1, "when": "before"},
    # t1 (tree update) stages two files, then e1 (extra file) writes its staged file
    "tree_update_staged": {"at": "call", "target": "filetree._write_file", "nth": 1, "when": "after"},
    "extra_file_staged": {"at": "call", "target": "filetree._write_file", "nth": 3, "when": "after"},
}


@pytest.fixture
def scenario(git, user_home, hermes_home, tmp_path, cfg, handover_root):
    return Scenario(git, user_home, hermes_home, tmp_path, cfg, handover_root)


@pytest.mark.parametrize("where", sorted(PREPARE_CRASHES))
def test_f08_target_restarted_in_preparing_resumes_to_the_uninterrupted_result(scenario, where):
    """★ L3-F08 (process) — the target dies inside PREPARE at the named point (no snapshot journaled); a new process
    prepares the same job again and commits: the result equals an uninterrupted commit, nothing of the first run
    is left beside the destinations, the staging step records are gone (before: the second process died on
    ``FileExistsError: … .ctxh-<job6>`` for the git cases; the tree update went to a fresh fallback copy)."""
    assert scenario.run(PREPARE_CRASHES[where]) == 17
    assert not Path(scenario.spec["journal"]).exists()  # died inside PREPARE: nothing journaled yet
    assert scenario.run() == 0
    assert scenario.events()[-1]["outcome"] == "committed"
    scenario.assert_committed()
    assert _leftovers(scenario.root) == []


@pytest.mark.parametrize("where", sorted(PREPARE_CRASHES))
def test_f08_target_aborted_after_a_cut_prepare_is_as_before(scenario, where):
    """★ L3-F08 (abort side, process) — the target dies inside PREPARE; the job is then aborted (DESIGN §3.6 "abort
    on conductor request", expiry): the abort's discard finds no snapshot and must still remove what PREPARE made —
    the handover root, the plugin's earlier tree write and the update repositories are exactly as before (before:
    the partial ``.ctxh-<job6>``, the staged extra file and the tree update's staged files stayed)."""
    assert scenario.run(PREPARE_CRASHES[where]) == 17
    assert scenario.run(abort=True) == 0
    assert scenario.events()[-1]["outcome"] == "aborted"
    assert scenario.observe() == scenario.before
    assert _leftovers(scenario.root) == []
    assert not (scenario.tmp / "staging" / materialize.STEP_DIR).exists()


def test_f08_commit_crash_cases_still_resume(scenario):
    """Guard: a crash after PREPARE (snapshot journaled) still resumes from the snapshot — the records PREPARE now
    writes do not make the resumed commit refuse its own partial."""
    assert scenario.run(FORWARD_CRASHES["git_fresh_after_rename"]) == 17
    assert scenario.run() == 0
    scenario.assert_committed()


def test_discard_after_a_commit_removes_nothing_of_it(git, user_home, tmp_path, cfg, store, handover_root):
    """Guard: ``discard`` after a commit (the orchestration's cleanup) leaves the committed destination and the
    parent directories PREPARE made for it alone."""
    staging = tmp_path / "staging"
    src, item, pitem = _tree_item(user_home, cfg, store)
    dest = handover_root / "deep" / "dest"
    txn = _txn(git, store, cfg, staging)
    txn.prepare_item(item, _fresh(item, dest), pitem)
    for _name, forward, _inverse in txn.commit_steps():
        forward()
    _txn(git, store, cfg, staging).discard()
    assert tree_digest(dest) == tree_digest(src)
    assert sorted(os.listdir(handover_root / "deep")) == ["dest"]


def test_gitops_partial_exists_names_path_and_destination(tmp_path):
    """The refusal is one shape for every caller (``gitops.partial_exists``)."""
    err = gitops.partial_exists(tmp_path / ".ctxh-abcdef", tmp_path / "app")
    assert isinstance(err, ConflictError) and err.details["kind"] == "partial_exists"
    assert err.details["path"] == str(tmp_path / ".ctxh-abcdef") and err.details["dest"] == str(tmp_path / "app")
