"""Round G′ of cluster E: undo failures and the orphaned ``index.lock`` end ``needs_attention`` (README limit,
DESIGN §3.4), ``FakeGitProbe.is_partial`` (contract V30) and the success sweep — a git or block-store answer counts
as success only when it is explicitly positive (a failed, killed or short answer never reads as "nothing there").

Every test here was red on the code before G′. Failures are injected with
``FailingGit`` (a real git whose named subcommand dies) — the situations behind them are real: a git process
killed by the OOM killer or a cancel, a peer that stores fewer blocks than it was sent."""

from __future__ import annotations

import errno
import io
import os
from pathlib import Path

import pytest

from ctx_handover.engine import contracts, filetree, gitapply, gitops, gitpack, materialize
from ctx_handover.engine.errors import BlockCorrupt, Cancelled, ConflictError
from ctx_handover.engine.gitops import GitCommandError
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.model import DestMode, Include, ItemKind, ItemRole, Origin, PlanItem
from ctx_handover.engine.paths import rules_for
from ctx_handover.tests.fakes.git_fake import DirBlockStore, FakeGitProbe
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import FailingGit, append_repo_config, apply_fresh, build_mixed, pack

LINUX = rules_for("linux", False)
JOB = "01JC0GPE00000000000000000A"
KEY = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAB3NzaC1yc2EAAAADAQABAAABAQ\n-----END OPENSSH PRIVATE KEY-----\n"


def _repo(root: Path) -> RepoBuilder:
    r = RepoBuilder.init(root)
    r.write("a.txt", "a\n")
    r.write("notes.txt", "plain\n")
    r.commit_all("c1")
    return r


def _clean_target(git, store, cfg, src: RepoBuilder, staging: Path, dest: Path) -> RepoBuilder:
    """A target repository with the source's committed history only (clean, same identity)."""
    _, item = pack(git, src.root, staging, cfg, store, item_id="seed")
    t = RepoBuilder(apply_fresh(git, store, item, dest, cfg))
    t.run("stash", "clear")
    t.run("reset", "-q", "--hard")
    t.run("clean", "-qfdx")
    return t


def _updated(git, user_home, staging, cfg, store, handover_root):
    """``(source, target, item, state)``: an update of a clean target that adds a commit, a branch and stash
    entries (``build_mixed``), prepared but not committed."""
    src = build_mixed(user_home / "src")
    tgt = _clean_target(git, store, cfg, src, staging, handover_root / "app")
    src.write("committed.txt", "c\n")
    src.add("committed.txt")
    src.commit("advance")
    src.branch("topic")
    basis = gitops.GitProbe(git).ref_tips(str(tgt.root))
    _, item = pack(git, src.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, tgt.root, cfg=cfg, qdir=staging / "q")
    return src, tgt, item, state


# ---------------------------------------------------------------------------------------------------------
# V30: FakeGitProbe.is_partial; the real probe reads the promisor flag as a git boolean
# ---------------------------------------------------------------------------------------------------------


def test_fake_git_probe_satisfies_the_protocol_with_is_partial():
    """V30 — the scripted probe is a ``contracts.GitProbe`` and answers ``is_partial`` like ``gitops.GitProbe``:
    for the repository root and every path inside it, never for a sibling with a common prefix."""
    probe = FakeGitProbe(partial={"/w/repo"})
    assert isinstance(probe, contracts.GitProbe)
    assert probe.is_partial("/w/repo") is True
    assert probe.is_partial("/w/repo/sub/dir") is True
    assert probe.is_partial("/w/repo-other") is False
    assert probe.is_partial("/w") is False
    assert ("is_partial", ("/w/repo",)) in probe.calls


@pytest.mark.parametrize("value", ["yes", "on", "1", "TRUE"])
def test_is_partial_reads_the_promisor_flag_as_a_git_boolean(git, user_home, value):
    """W107 — git treats ``remote.<name>.promisor = yes/on/1`` as a partial clone; so must the probe (a missed
    partial clone is packed as git and fails on its missing objects instead of travelling as a file tree)."""
    r = _repo(user_home / "p")
    (r.root / "sub").mkdir()
    append_repo_config(r.root, f'[remote "origin"]\n\turl = https://example.invalid/r.git\n\tpromisor = {value}\n')
    probe = gitops.GitProbe(git)
    assert probe.is_partial(str(r.root)) is True
    assert probe.is_partial(str(r.root / "sub")) is True


def test_is_partial_false_flag_and_partialclone_extension(git, user_home):
    """``promisor = false`` is a full clone; ``extensions.partialClone`` alone marks a partial clone; an
    unreadable flag counts as partial (fail closed: a file tree always travels)."""
    r = _repo(user_home / "p")
    append_repo_config(r.root, '[remote "origin"]\n\turl = https://example.invalid/r.git\n\tpromisor = false\n')
    probe = gitops.GitProbe(git)
    assert probe.is_partial(str(r.root)) is False
    q = _repo(user_home / "q")
    append_repo_config(q.root, "[extensions]\n\tpartialClone = origin\n")
    q.run("config", "core.repositoryformatversion", "1")
    assert probe.is_partial(str(q.root)) is True
    bad = _repo(user_home / "bad")
    append_repo_config(bad.root, '[remote "origin"]\n\turl = https://example.invalid/r.git\n\tpromisor = maybe\n')
    assert probe.is_partial(str(bad.root)) is True


# ---------------------------------------------------------------------------------------------------------
# Success sweep, source side (gitops.compute_state, gitpack)
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("born", [True, False], ids=["reset", "rm-cached"])
def test_failed_reset_of_a_staged_secret_stops_the_pack(git, user_home, staging, cfg, store, born):
    """★ A staged denied file is taken out of the index copy before the index tree is written
    (``compute_state``); when that git call fails, the pack must fail — not ship the index with the secret."""
    r = RepoBuilder.init(user_home / "s")
    if born:
        r.write("a.txt", "a\n")
        r.commit_all("c1")
    r.write(".env", "API_KEY=1\n")
    r.add(".env")
    failing = FailingGit(("reset", "-q") if born else ("rm", "-q", "--cached"))
    try:
        res, _ = pack(failing, r.root, staging, cfg, store)
    except GitCommandError:
        assert failing.failed
        return
    leaked = ".env" in gitops.GitProbe(git, profile="received").ls_tree_paths(
        str(staging / "git" / "w0" / "repo"), res.state.index_tree
    )
    pytest.fail(f"a failed git {failing.fail[0]} counted as success (staged secret in the index tree: {leaked})")


@pytest.mark.parametrize(("sub", "staged"), [("diff-files", False), ("diff-index", True)])
def test_failed_change_listing_stops_the_pack(git, user_home, staging, cfg, store, sub, staged):
    """★ Changed tracked files are content-sniffed for keys (``_scan_tracked``); a git ``diff-files``/
    ``diff-index`` that fails must stop the pack instead of reading as "nothing changed"."""
    r = _repo(user_home / "s")
    r.write("notes.txt", KEY)
    if staged:
        r.add("notes.txt")
    real, _ = pack(git, r.root, staging / "real", cfg, store)
    assert "notes.txt" in [n.path for n in real.excluded]
    failing = FailingGit((sub,))
    try:
        res, _ = pack(failing, r.root, staging / "fail", cfg, store)
    except GitCommandError:
        assert failing.failed
        return
    pytest.fail(f"a failed git {sub} counted as 'no changes' (excluded: {[n.path for n in res.excluded]})")


def test_unreadable_stash_blob_is_not_harmless(git, user_home):
    """★ The stash sniff reads the head of a blob; a ``cat-file`` that fails (missing object) must raise, not read
    as an empty, harmless blob. Small and large blobs still read normally."""
    r = _repo(user_home / "s")
    small = r.out("hash-object", "-w", str(r.root / "notes.txt"))
    (r.root / "big.bin").write_bytes(b"x" * (3 * 8192))
    big = r.out("hash-object", "-w", str(r.root / "big.bin"))
    (r.root / "key.txt").write_text(KEY)
    key = r.out("hash-object", "-w", str(r.root / "key.txt"))
    assert gitpack._blob_is_secret(git, r.root, small) is False
    assert gitpack._blob_is_secret(git, r.root, big) is False
    assert gitpack._blob_is_secret(git, r.root, key) is True
    with pytest.raises(GitCommandError):
        gitpack._blob_is_secret(git, r.root, "0123456789abcdef0123456789abcdef01234567")


def test_failed_stash_listing_raises(git, user_home):
    """★ ``git stash list`` that fails must not read as "no stash" (the entries would silently stay behind)."""
    r = _repo(user_home / "s")
    r.write("a.txt", "wip\n")
    r.stash("wip")
    assert len(gitpack._stash_entries(git, r.root)) == 1
    with pytest.raises(GitCommandError):
        gitpack._stash_entries(FailingGit(("stash", "list")), r.root)


def test_unreadable_config_is_not_read_as_no_remotes(git, user_home, cfg):
    """★ ``git config --get-regexp`` answers 1 for "no match" (positive: none); any other failure raises —
    in the pack (``_config_sections``) and in the planning facts (``GitProbe.info``)."""
    r = _repo(user_home / "s")
    assert gitpack._config_sections(git, r.root, r"^remote\..*\.url$") == []
    with pytest.raises(GitCommandError):
        gitpack._config_sections(FailingGit(("config", "--local"), rc=3), r.root, r"^remote\..*\.url$")
    assert gitops.GitProbe(git).info(str(r.root), cfg).remotes == ()
    with pytest.raises(GitCommandError):
        gitops.GitProbe(FailingGit(("config", "-z", "--get-regexp"), rc=3)).info(str(r.root), cfg)


def test_block_sink_that_stores_fewer_blocks_than_sent_fails_the_write(tmp_path):
    """★ ``write_blocks`` returns ``(accepted, errors)``; a sink that accepted fewer blocks than it was sent —
    without naming an error (a peer answer without ``rejected``) — must fail the write, not count as stored."""

    class ShortSink(DirBlockStore):
        def write_blocks(self, frames):
            frames = list(frames)
            return super().write_blocks(frames[:-1])

    data = os.urandom(gitops.BLOCK_SIZE + 10)
    gitops.put_bytes(DirBlockStore(tmp_path / "ok"), data, "application/octet-stream")
    with pytest.raises(BlockCorrupt):
        gitops.put_bytes(ShortSink(tmp_path / "short"), data, "application/octet-stream")
    with pytest.raises(BlockCorrupt):
        gitops.put_stream(ShortSink(tmp_path / "short2"), io.BytesIO(data), media="x/y")


def test_pack_cache_without_its_block_list_is_a_miss(tmp_path):
    """★ A cached pack result is reused only when its blocks are known and present; a cache record that names a
    pack object but no blocks (or lacks a key) is a miss, not "nothing to check"."""
    store = DirBlockStore(tmp_path / "store")
    wd = tmp_path / "wd"
    wd.mkdir()
    obj = gitops.put_bytes(store, b"pack bytes", "application/x-git-packed-objects").object_id
    blocks = list(store.manifests([obj])[0].blocks)
    base = {
        "index_commit": "a" * 40,
        "worktree_commit": "b" * 40,
        "pack_object": obj,
        "lfs_manifest": None,
        "lfs_files": [],
        "lfs_warnings": [],
    }
    gitpack._save_cache(wd, "k", {**base, "blocks": blocks})
    assert gitpack._load_cache(wd, "k", store) is not None
    gitpack._save_cache(wd, "k", base)
    assert gitpack._load_cache(wd, "k", store) is None
    gitpack._save_cache(wd, "k", {"blocks": blocks, "pack_object": obj})
    assert gitpack._load_cache(wd, "k", store) is None
    lfs = gitops.put_bytes(store, b'{"entries":[]}', "application/json").object_id
    gitpack._save_cache(wd, "k", {**base, "blocks": blocks, "lfs_manifest": lfs})
    assert gitpack._load_cache(wd, "k", store) is None
    lfs_blocks = list(store.manifests([lfs])[0].blocks)
    gitpack._save_cache(wd, "k", {**base, "blocks": blocks, "lfs_manifest": lfs, "lfs_blocks": lfs_blocks})
    assert gitpack._load_cache(wd, "k", store) is not None


def test_iter_z_does_not_kill_a_git_that_already_closed_its_output(tmp_path, monkeypatch):
    """A git that closed stdout and is about to exit 0 is waited for, not killed (rc -9 would read as a failed
    command — a false failure under host load). Only an early stop of the reader kills the process."""
    script = tmp_path / "fake-git"
    script.write_text("#!/bin/sh\nprintf 'a\\000b\\000'\nexec 1>&-\nsleep 0.3\nexit 0\n")
    os.chmod(script, 0o755)  # noqa: S103 - executable test fixture
    g = gitops.Git(str(script))
    assert list(g.iter_z(["ls-files", "-z"], cwd=None, spec=gitops.SOURCE)) == [b"a", b"b"]
    script.write_text("#!/bin/sh\nprintf 'a\\000'\nexec 1>&-\nexit 3\n")
    with pytest.raises(GitCommandError):
        list(g.iter_z(["ls-files", "-z"], cwd=None, spec=gitops.SOURCE))


def test_head_info_failure_is_neither_unborn_nor_detached(git, user_home):
    """★ ``symbolic-ref -q`` answers 1 for a detached HEAD and ``rev-parse --verify -q`` 1 for an unborn branch;
    a failed (killed) call is neither — read as unborn it skips the staged-change sniff of the pack."""
    r = _repo(user_home / "s")
    assert gitops.head_info(git, r.root) == (r.head(), "refs/heads/main")
    u = RepoBuilder.init(user_home / "u")
    assert gitops.head_info(git, u.root) == (None, "refs/heads/main")
    r.run("checkout", "-q", "--detach")
    assert gitops.head_info(git, r.root) == (r.head(), None)
    for sub in (("symbolic-ref",), ("rev-parse", "--verify")):
        with pytest.raises(GitCommandError):
            gitops.head_info(FailingGit(sub), r.root)


def test_unreadable_local_filter_config_is_not_read_as_no_filters(git, user_home):
    """★ The received profile empties the destination's own filter drivers by name; a failed read of their names
    must raise, not read as "no filters" (1 = none is the only empty answer)."""
    r = _repo(user_home / "s")
    append_repo_config(r.root, '[filter "x"]\n\tsmudge = cat\n')
    assert gitops.local_filter_names(git, r.root) == ["x"]
    assert gitops.local_filter_names(git, _repo(user_home / "t").root) == []
    with pytest.raises(GitCommandError):
        gitops.local_filter_names(FailingGit(("config", "--file"), rc=3), r.root)


def test_file_vanishing_between_scan_and_pack_is_reported_not_counted(
    git, user_home, cfg, store, tmp_path, monkeypatch
):
    """★ ``pack_tree`` skips a file that is gone (or no longer regular) when it is read; that skip must reach the
    pack-document item as ``skipped`` (``missing``) and leave the item's counts — not vanish while ``entries``,
    ``bytes`` and the fingerprint still count the file."""
    root = user_home / "notes"
    root.mkdir()
    for rel, text in {"keep.md": "keep\n", "gone.md": "gone soon\n"}.items():
        (root / rel).write_text(text)
    real_scan = filetree.scan

    def scan_then_delete(*a, **kw):
        out = real_scan(*a, **kw)
        (root / "gone.md").unlink()
        return out

    monkeypatch.setattr(filetree, "scan", scan_then_delete)
    item = PlanItem(
        item_id="t1",
        kind=ItemKind.FILE_TREE,
        source_path=str(root),
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=100,
    )
    packed = materialize.pack_plan_item(item, git=git, store=store, cfg=cfg, workdir=tmp_path / "w", created_at=1.0)
    assert ("gone.md", "missing") in [(n.path, n.why) for n in packed.skipped]
    assert {"path": "gone.md", "why": "missing"} in packed.doc["skipped"]
    assert (packed.doc["entries"], packed.doc["bytes"]) == (1, len("keep\n"))
    assert sorted(packed.state["tree"]["files"]) == ["keep.md"]


# ---------------------------------------------------------------------------------------------------------
# Success sweep, target side (gitapply rollback, step records)
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("sub", [("stash", "drop"), ("for-each-ref",), ("stash", "list")])
def test_rollback_that_cannot_finish_raises(git, user_home, staging, cfg, store, handover_root, sub):
    """★ The update inverse drops the stash entries and deletes the refs the forward step added; a git call
    there that fails must make the inverse fail (orchestration: ``needs_attention``), never end as "undone"."""
    _src, tgt, item, state = _updated(git, user_home, staging, cfg, store, handover_root)
    before_refs = dict(tgt.refs())
    before_stash = tgt.stash_list()
    gitapply.commit(git, state, item)
    assert "refs/heads/topic" in tgt.refs()
    failing = FailingGit(sub)
    try:
        gitapply.rollback(failing, state, item)
    except GitCommandError:
        assert failing.failed
        return
    pytest.fail(
        f"rollback ended as undone after a failed git {' '.join(sub)}: refs restored "
        f"{tgt.refs() == before_refs}, stash restored {tgt.stash_list() == before_stash}"
    )


def test_unreadable_step_record_is_not_read_as_absent(tmp_path):
    """★ A step record that exists but cannot be read (torn, foreign) must not read as "this step never began":
    a git step would then rerun without its resume check. Only a missing record is absent."""
    log = materialize._StepLog(tmp_path)
    log.put("c1:w1:forward", {})
    assert log.get("c1:w1:forward") == {"name": "c1:w1:forward"}
    assert log.get("c1:w9:forward") is None
    log.put("c3:a", {"target": "x"})
    assert [d["name"] for d in log.items("c3:")] == ["c3:a"]
    log._path("c1:w1:forward").write_bytes(b'{"name": "c1:w1:forw')
    with pytest.raises(ConflictError) as got:
        log.get("c1:w1:forward")
    assert got.value.details.get("needs_attention") is True
    log._path("c3:a").write_bytes(b"\x00garbage")
    with pytest.raises(ConflictError):
        log.items("c3:")


def test_extra_file_of_an_old_snapshot_is_ours_only_by_content(git, cfg, store, user_home, handover_root, tmp_path):
    """★ A snapshot of format 1 carries no file identity; a resumed extra-file step must not take any file at the
    destination for its own — forward (counted as linked) or inverse (moved away, then discarded): only a regular
    file with the packed content is ours."""
    src = user_home / "todo.txt"
    src.write_text("todo\n")
    e1 = {"item_id": "e1", **filetree.pack_file(src, store)}
    dest = handover_root / "todo.txt"
    dest.write_text("someone else's file\n")
    txn = materialize.WorkspaceTxn(JOB, tmp_path / "staging", git, store, lambda *a: None, cfg=cfg)
    temp = handover_root / ".todo.txt.ctxh-0000aa"
    p = materialize._Prepared("e1", "extra", DestMode.FRESH, dest, e1, temp, [], ident=None)
    with pytest.raises(ConflictError):
        txn._extra_forward(p)
    txn._inverse(p)()
    assert dest.read_text() == "someone else's file\n" and not temp.exists()
    dest.write_text("todo\n")
    assert txn._extra_forward(p) == dest
    txn._inverse(p)()
    assert not dest.exists() and temp.read_text() == "todo\n"


# ---------------------------------------------------------------------------------------------------------
# needs_attention: a forward step whose undo fails (orphaned index.lock included)
# ---------------------------------------------------------------------------------------------------------


class _KilledReadTree(gitops.Git):
    """Git whose work-tree ``read-tree -u -m`` is killed mid-way by a cancel: it leaves ``index.lock`` behind,
    as a killed git does (it cannot remove its lock)."""

    def __init__(self, lock: Path):
        super().__init__()
        self.lock = lock

    def run(self, args, **kw):
        if "read-tree" in args and "-u" in args and "-m" in args:
            self.lock.write_bytes(b"DIRC half-written index")
            raise Cancelled("cancelled while git ran")
        return super().run(args, **kw)


def test_git_killed_inside_the_update_leaves_index_lock_needs_attention(
    git, user_home, staging, cfg, store, handover_root
):
    """★ README limit — a git killed in the middle of the update's ``read-tree`` leaves ``.git/index.lock``; the
    step cannot undo itself (git refuses the locked index). It must say so — ``ConflictError`` with
    ``details.needs_attention`` naming the lock — instead of re-raising the cancel as if nothing had changed. The
    lock is left alone (it may belong to a live git), nothing else is touched."""
    _src, tgt, item, state = _updated(git, user_home, staging, cfg, store, handover_root)
    head, refs = tgt.head(), tgt.refs()
    lock = tgt.root / ".git" / "index.lock"
    with pytest.raises(ConflictError) as got:
        gitapply.commit(_KilledReadTree(lock), state, item)
    err = got.value
    assert err.details.get("needs_attention") is True
    assert "index.lock" in err.message and str(tgt.root) in err.message
    assert lock.read_bytes() == b"DIRC half-written index"
    assert (tgt.head(), tgt.refs()) == (head, refs)


def test_rollback_refuses_an_orphaned_index_lock_with_a_clear_message(
    git, user_home, staging, cfg, store, handover_root
):
    """The update inverse never runs git against a locked index: it names the lock and what to do."""
    _src, tgt, item, state = _updated(git, user_home, staging, cfg, store, handover_root)
    gitapply.commit(git, state, item)
    lock = tgt.root / ".git" / "index.lock"
    lock.write_bytes(b"orphan")
    with pytest.raises(ConflictError) as got:
        gitapply.rollback(git, state, item, partial=True)
    assert got.value.details.get("kind") == "index_lock"
    assert got.value.details.get("needs_attention") is True
    assert "no git process" in got.value.message
    assert lock.read_bytes() == b"orphan"


def _tree_update(user_home, cfg, store, handover_root, tmp_path):
    src = user_home / "notes"
    (src / "sub").mkdir(parents=True)
    for rel, text in {"n1.md": "one\n", "n2.md": "two\n", "sub/n3.md": "three\n"}.items():
        (src / rel).write_text(text)
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    dest = handover_root / "notes"
    fp = filetree.commit(
        filetree.prepare_fresh(
            store, filetree.pack_tree(entries, src, store), handover_root / ".ctxh-n", LINUX, dest=dest
        )
    )["fingerprint"]
    for rel, text in {"n1.md": "one, changed\n", "n2.md": "two, changed\n"}.items():
        (src / rel).write_text(text)
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    state = filetree.prepare_update(
        store, filetree.pack_tree(entries, src, store), dest, fp, LINUX, tmp_path / "backup", tag="gpe001"
    )
    return dest, state


def test_tree_update_whose_undo_fails_needs_attention(user_home, cfg, store, handover_root, tmp_path, monkeypatch):
    """★ A tree update that fails half-way (disk full on the second swap) undoes itself; when that undo fails too
    (a backup cannot be moved back), the step must report ``needs_attention`` — not re-raise the first error as if
    the destination were untouched (the orchestration would then roll back the other steps and call it undone)."""
    _dest, state = _tree_update(user_home, cfg, store, handover_root, tmp_path)
    real_replace, real_move = os.replace, filetree._move
    swaps = {"n": 0}

    def replace(src, dst, *a, **kw):
        if ".ctxh-gpe001" in os.fspath(src):
            swaps["n"] += 1
            if swaps["n"] == 2:
                raise OSError(errno.ENOSPC, "No space left on device")
        return real_replace(src, dst, *a, **kw)

    def move(src, dst):
        if Path(src).parent == tmp_path / "backup":
            raise OSError(errno.EIO, "Input/output error")
        return real_move(src, dst)

    monkeypatch.setattr(os, "replace", replace)
    monkeypatch.setattr(filetree, "_move", move)
    with pytest.raises(ConflictError) as got:
        filetree.commit(state)
    monkeypatch.undo()
    assert got.value.details.get("needs_attention") is True
    assert "No space left" in got.value.message and "Input/output error" in got.value.message


def test_session_file_step_whose_undo_fails_needs_attention(
    git, user_home, hermes_home, cfg, store, tmp_path, monkeypatch
):
    """★ c3 places session files and undoes itself on a failure; an undo that cannot remove a placed file must
    report ``needs_attention`` (the file stays in the session area) instead of the first error."""
    targets = [hermes_home / "cache" / "spillover" / "sx" / f"call_{n}.txt" for n in (1, 2)]
    files = []
    for n, target in enumerate(targets, 1):
        obj = gitops.put_bytes(store, f"tool output {n}\n".encode(), "application/octet-stream").object_id
        files.append(({"kind": "hermes_file", "object": obj, "hermes_kind": "spillover"}, target))
    txn = materialize.WorkspaceTxn(JOB, tmp_path / "staging", git, store, lambda *a: None, cfg=cfg)
    forward, _inverse = txn.place_hermes_files(files)
    real_iter, real_unlink = materialize.iter_object, os.unlink
    calls = {"n": 0}

    def iter_object(*a, **kw):
        calls["n"] += 1
        if calls["n"] == 2:
            raise OSError(errno.EIO, "store read failed")
        return real_iter(*a, **kw)

    def unlink(path, *a, **kw):
        if Path(path) == targets[0]:
            raise PermissionError(errno.EACCES, "Permission denied")
        return real_unlink(path, *a, **kw)

    monkeypatch.setattr(materialize, "iter_object", iter_object)
    monkeypatch.setattr(os, "unlink", unlink)
    with pytest.raises(ConflictError) as got:
        forward()
    monkeypatch.undo()
    assert got.value.details.get("needs_attention") is True
    assert targets[0].read_text() == "tool output 1\n"
