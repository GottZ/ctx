"""Target decisions and the workspace transaction (DESIGN §8.6, §9.5–§9.7, §10.4, §10.6; INTERFACES §4.3)."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitops, materialize
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.model import (
    Conflict,
    DestMode,
    Include,
    ItemKind,
    ItemRole,
    MapRow,
    Origin,
    PlanItem,
    TargetDecision,
)
from ctx_handover.tests.fakes.git_fake import FakeWorkspaceTxn, RecordingJournal
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.unit.E._support import AT, apply_fresh, assert_same_state, build_mixed, env_facts, pack


def _item(kind: ItemKind, source: Path, *, git_info=None, item_id: str = "w0") -> PlanItem:
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=str(source),
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=1000,
        git=git_info,
    )


def _assess(git, cfg, item, dest, user_home, tmp_path, *, out_row=None, fp=None, recorded=False, local=False):
    return materialize.assess(
        item,
        str(dest),
        git=git,
        probe=gitops.GitProbe(git, profile="received"),
        out_row=out_row,
        tree_fingerprint=fp,
        target_facts=env_facts(user_home, workspace_root=user_home / "hermes-handover"),
        fallback=lambda d: materialize.fallback_dest(d, AT),
        tmp=tmp_path,
        cfg=cfg,
        recorded=recorded,
        local_choice=local,
    )


def test_materialize_assess_modes(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """absent, empty, same repo clean / dirty / handed away, other repo, plain directory, file, denied path."""
    src = build_mixed(user_home / "src")
    info = gitops.GitProbe(git).info(str(src.root), cfg)
    item = _item(ItemKind.GIT_REPO, src.root, git_info=info)
    a = _assess(git, cfg, item, handover_root / "absent", user_home, tmp_path)
    assert (a.mode, a.conflict) == (DestMode.FRESH, None)
    (handover_root / "empty").mkdir()
    assert _assess(git, cfg, item, handover_root / "empty", user_home, tmp_path).mode is DestMode.FRESH
    _, pitem = pack(git, src.root, staging, cfg, store)
    clone = RepoBuilder(apply_fresh(git, store, pitem, handover_root / "same", cfg))
    dirty = _assess(git, cfg, item, clone.root, user_home, tmp_path)
    assert dirty.conflict is not None and dirty.conflict.kind == "dirty"
    assert dirty.conflict.fallback_dest.startswith(str(clone.root) + "-handover-")
    row = MapRow(
        str(clone.root),
        "peer",
        str(src.root),
        "out",
        state={
            "head": pitem["head"]["commit"],
            "index_tree": pitem["index_tree"],
            "worktree_tree": pitem["worktree_tree"],
            "branch": "refs/heads/main",
            "exclude_pathspecs": pitem["exclude_pathspecs"],
        },
    )
    ret = _assess(git, cfg, item, clone.root, user_home, tmp_path, out_row=row, recorded=True)
    assert ret.mode is DestMode.RETURN and ret.conflict is None and ret.git_have
    clone.run("stash", "clear")
    clone.run("reset", "-q", "--hard")
    clone.run("clean", "-qfdx")
    upd = _assess(git, cfg, item, clone.root, user_home, tmp_path)
    assert upd.mode is DestMode.UPDATE and src.head() in upd.git_have
    other = RepoBuilder.init(handover_root / "other")
    other.write("x", "1")
    other.commit_all("unrelated")
    assert _assess(git, cfg, item, other.root, user_home, tmp_path).conflict.kind == "foreign"
    (handover_root / "plain").mkdir()
    (handover_root / "plain" / "f.txt").write_text("x")
    assert _assess(git, cfg, item, handover_root / "plain", user_home, tmp_path).conflict.kind == "not_ours"
    (handover_root / "file").write_text("x")
    assert _assess(git, cfg, item, handover_root / "file", user_home, tmp_path).conflict.kind == "exists_file"
    denied = _assess(git, cfg, item, user_home / ".ssh" / "app", user_home, tmp_path, local=True)
    assert denied.conflict.kind == "not_allowed"
    assert denied.conflict.fallback_dest.startswith(str(handover_root))
    outside = _assess(git, cfg, item, user_home / "projects" / "app", user_home, tmp_path)
    assert outside.conflict.kind == "not_allowed"
    assert (
        _assess(git, cfg, item, user_home / "projects" / "app", user_home, tmp_path, local=True).mode is DestMode.FRESH
    )


def test_materialize_assess_trees(git, user_home, tmp_path, cfg, store, handover_root):
    root = user_home / "tree"
    root.mkdir()
    (root / "a.txt").write_text("a\n")
    entries, _ = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    mid = filetree.pack_tree(entries, root, store)
    dest = handover_root / "tree"
    fp = filetree.commit(
        filetree.prepare_fresh(store, mid, handover_root / ".ctxh-t", cfg.platform_rules(), dest=dest)
    )["fingerprint"]
    item = _item(ItemKind.FILE_TREE, root)
    assert _assess(git, cfg, item, dest, user_home, tmp_path, fp=fp).mode is DestMode.UPDATE
    assert _assess(git, cfg, item, dest, user_home, tmp_path).conflict.kind == "not_ours"
    (dest / "a.txt").write_text("changed\n")
    assert _assess(git, cfg, item, dest, user_home, tmp_path, fp=fp).conflict.kind == "foreign"


def _decision(item_id, dest, mode=DestMode.FRESH, conflict=None, reason="workspace-root"):
    return TargetDecision(item_id=item_id, dest=str(dest), mode=mode, reason=reason, conflict=conflict)


def test_workspace_txn_commit_steps_and_inverse(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """Prepare two items (git fresh + tree fresh), run the c1 steps forward, then their inverses in reverse:
    nothing is visible before c1, everything after, and the inverse puts both back into their partials."""
    src = build_mixed(user_home / "src")
    _, gitem = pack(git, src.root, staging, cfg, store)
    troot = user_home / "notes"
    troot.mkdir()
    (troot / "n.md").write_text("note\n")
    entries, _ = filetree.scan(troot, IgnoreMatcher.for_tree(troot, ()), cfg)
    titem = {"item_id": "x1", "kind": "file_tree", "manifest": filetree.pack_tree(entries, troot, store)}
    journal = RecordingJournal()
    txn = materialize.WorkspaceTxn("01JOB0000000000000000ABCDEF", tmp_path / "txn", git, store, journal, cfg=cfg)
    txn.prepare_item(_item(ItemKind.GIT_REPO, src.root), _decision("w0", handover_root / "proj" / "app"), gitem)
    txn.prepare_item(_item(ItemKind.FILE_TREE, troot, item_id="x1"), _decision("x1", handover_root / "notes"), titem)
    assert not (handover_root / "proj" / "app").exists() and not (handover_root / "notes").exists()
    steps = txn.commit_steps()
    assert [s[0] for s in steps] == ["c1:w0", "c1:x1"]
    for _, forward, _ in steps:
        forward()
    assert_same_state(
        src, RepoBuilder(handover_root / "proj" / "app"), excluded=[b".env"], ignored=[b"ignored", b"build.log"]
    )
    assert (handover_root / "notes" / "n.md").read_text() == "note\n"
    assert [d[:2] for d in txn.dests()] == [
        ("w0", str(handover_root / "proj" / "app")),
        ("x1", str(handover_root / "notes")),
    ]
    assert set(txn.map_states()) == {"w0", "x1"}
    for _, _, inverse in reversed(steps):
        inverse()
    assert not (handover_root / "notes").exists() and not (handover_root / "proj" / "app").exists()
    txn.discard()
    assert list(handover_root.iterdir()) == []
    assert "c1:w0" in journal.steps("done") and "c1-inverse:x1" in journal.steps("done")


def test_sec_peer_cannot_overwrite_dirty_target(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """A peer's update decision for a dirty repository lands in a fresh directory; the user's work stays [T3, I5]."""
    src = build_mixed(user_home / "src")
    _, seed = pack(git, src.root, staging, cfg, store, item_id="seed")
    tgt = RepoBuilder(apply_fresh(git, store, seed, handover_root / "app", cfg))
    before = dir_fingerprint(tgt.root)
    src.write("later.txt", "x\n")
    _, item = pack(git, src.root, staging, cfg, store, item_id="w0", basis=gitops.GitProbe(git).ref_tips(str(tgt.root)))
    txn = materialize.WorkspaceTxn(
        "01JOB000000000000000000DIRTY", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg, now=lambda: AT
    )
    txn.prepare_item(
        _item(ItemKind.GIT_REPO, src.root), _decision("w0", tgt.root, DestMode.UPDATE, reason="suggested"), item
    )
    for _, forward, _ in txn.commit_steps():
        forward()
    ((_, final, mode),) = txn.dests()
    assert final != str(tgt.root) and final.startswith(str(tgt.root) + "-handover-") and mode is DestMode.FRESH
    assert dir_fingerprint(tgt.root) == before
    assert not (Path(final) / ".git" / "objects" / "info" / "alternates").exists()
    assert_same_state(src, RepoBuilder(Path(final)), excluded=[b".env"], ignored=[b"ignored", b"build.log"])


def test_sec_extra_file_not_into_existing_dir(git, user_home, tmp_path, cfg, store, handover_root):
    """★ a remote-driven extra file never lands in an existing directory the plugin did not create: it goes to
    ``<workspace_root>/_handover/<job6>/…`` (mutant: extras written at their mapped path) [SEC 2.2]."""
    existing = handover_root / "notes"
    existing.mkdir()
    (existing / "mine.txt").write_text("user\n")
    src_file = user_home / "notes" / "todo.txt"
    src_file.parent.mkdir()
    src_file.write_text("todo\n")
    item = _item(ItemKind.EXTRA_FILE, src_file, item_id="e1")
    dest = existing / "todo.txt"
    job6 = "abcdef"

    def fb(d: str) -> str:
        return str(handover_root / "_handover" / job6 / "notes" / Path(d).name)

    verdict = materialize.assess(
        item,
        str(dest),
        git=git,
        probe=gitops.GitProbe(git, profile="received"),
        out_row=None,
        tree_fingerprint=None,
        target_facts=env_facts(user_home, workspace_root=handover_root),
        fallback=fb,
        tmp=tmp_path,
        cfg=cfg,
        recorded=False,
        local_choice=False,
    )
    assert verdict.conflict is not None and verdict.conflict.kind == "remote_extra"
    pitem = {"item_id": "e1", **filetree.pack_file(src_file, store)}
    txn = materialize.WorkspaceTxn(
        "01JOB00000000000000000ABCDEF", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg
    )
    txn.prepare_item(item, TargetDecision("e1", str(dest), verdict.mode, "home-relative", verdict.conflict), pitem)
    for _, forward, _ in txn.commit_steps():
        forward()
    assert sorted(p.name for p in existing.iterdir()) == ["mine.txt"]
    landed = handover_root / "_handover" / job6 / "notes" / "todo.txt"
    assert landed.read_text() == "todo\n"
    local = materialize.assess(
        item,
        str(dest),
        git=git,
        probe=gitops.GitProbe(git, profile="received"),
        out_row=None,
        tree_fingerprint=None,
        target_facts=env_facts(user_home, workspace_root=handover_root),
        fallback=fb,
        tmp=tmp_path,
        cfg=cfg,
        recorded=False,
        local_choice=True,
    )
    assert local.conflict is None and local.mode is DestMode.FRESH


def test_extra_file_identical_is_skipped(git, user_home, tmp_path, cfg, store, handover_root):
    src_file = user_home / "todo.txt"
    src_file.write_text("same\n")
    dest = handover_root / "todo.txt"
    dest.write_text("same\n")
    pitem = {"item_id": "e1", **filetree.pack_file(src_file, store)}
    txn = materialize.WorkspaceTxn(
        "01JOB00000000000000000SAMEXX", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg
    )
    txn.prepare_item(_item(ItemKind.EXTRA_FILE, src_file, item_id="e1"), _decision("e1", dest, reason="user"), pitem)
    assert txn.commit_steps() == [] and txn.dests()[0][2] is DestMode.SKIP


def test_place_hermes_files(git, user_home, hermes_home, tmp_path, cfg, store):
    """c3: files land only inside a session-file area, never over a different file; the inverse removes them."""
    good = hermes_home / "cache" / "spillover" / "call_1.txt"
    clash = hermes_home / "cache" / "spillover" / "call_2.txt"
    clash.parent.mkdir(parents=True)
    clash.write_text("different\n")
    bad = hermes_home / "config.yaml"
    obj = gitops.put_bytes(store, b"tool output\n", "application/octet-stream").object_id
    txn = materialize.WorkspaceTxn(
        "01JOB0000000000000000HERMES", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg
    )
    files = [
        ({"kind": "hermes_file", "object": obj, "hermes_kind": "spillover"}, good),
        ({"kind": "hermes_file", "object": obj, "hermes_kind": "spillover"}, clash),
        ({"kind": "hermes_file", "object": obj}, bad),
    ]
    forward, inverse = txn.place_hermes_files(files)
    forward()
    assert good.read_text() == "tool output\n" and clash.read_text() == "different\n" and not bad.exists()
    assert {(Path(n.path).name, n.why) for n in txn.skipped} == {
        ("call_2.txt", "exists"),
        ("config.yaml", "target_denied"),
    }
    inverse()
    assert not good.exists() and clash.exists()


def test_fake_workspace_txn_fault_injection():
    """The fake for cluster A records steps and raises where told."""
    fake = FakeWorkspaceTxn("j", fail={"c1:w1": RuntimeError("boom")})
    for item_id in ("w0", "w1"):
        fake.prepare_item(
            _item(ItemKind.GIT_REPO, Path("/x"), item_id=item_id), _decision(item_id, "/d/" + item_id), {}
        )
    steps = fake.commit_steps()
    steps[0][1]()
    with pytest.raises(RuntimeError):
        steps[1][1]()
    steps[0][2]()
    assert fake.log == ["prepare:w0", "prepare:w1", "c1:w0", "c1:w1", "c1-inverse:w0"]


def test_conflict_fallback_used_for_fresh(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """A decision with a conflict materialises at the conflict's fallback, not at the first choice."""
    src = build_mixed(user_home / "src")
    _, gitem = pack(git, src.root, staging, cfg, store)
    first = handover_root / "app"
    first.mkdir()
    (first / "keep").write_text("k")
    fb = handover_root / "app-handover-x"
    txn = materialize.WorkspaceTxn(
        "01JOB000000000000000000CONFL", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg
    )
    txn.prepare_item(
        _item(ItemKind.GIT_REPO, src.root),
        _decision("w0", first, conflict=Conflict("not_ours", "content", str(fb))),
        gitem,
    )
    for _, forward, _ in txn.commit_steps():
        forward()
    assert (fb / ".git").is_dir() and sorted(os.listdir(first)) == ["keep"]


def test_workspace_txn_snapshot_restore_in_new_process(git, user_home, tmp_path, staging, cfg, store, handover_root):
    """A transaction journaled after PREPARE is rebuilt from its JSON snapshot (as a new process would) and its
    c1 steps run forward and back from there (DESIGN §3.6: continue in the journaled direction)."""
    import json

    src = build_mixed(user_home / "src")
    _, seed = pack(git, src.root, staging, cfg, store, item_id="seed")
    tgt = RepoBuilder(apply_fresh(git, store, seed, handover_root / "upd", cfg))
    tgt.run("stash", "clear")
    tgt.run("reset", "-q", "--hard")
    tgt.run("clean", "-qfdx")

    def observe():
        return tgt.head(), tgt.refs(), tgt.index_entries(), tgt.status(), tgt.worktree_digest(), tgt.stash_list()

    before = observe()
    src.write(os.fsdecode(b"untracked-\xff.txt"), "odd name\n")
    _, upd = pack(git, src.root, staging, cfg, store, item_id="w1", basis=gitops.GitProbe(git).ref_tips(str(tgt.root)))
    _, fresh = pack(git, src.root, staging, cfg, store, item_id="w0")
    first = materialize.WorkspaceTxn(
        "01JOB000000000000000000SNAPX", tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg
    )
    first.prepare_item(_item(ItemKind.GIT_REPO, src.root), _decision("w0", handover_root / "fresh"), fresh)
    first.prepare_item(
        _item(ItemKind.GIT_REPO, src.root, item_id="w1"),
        _decision("w1", tgt.root, DestMode.UPDATE, reason="return-mapping"),
        upd,
    )
    snap = json.loads(json.dumps(first.snapshot()))
    again = materialize.WorkspaceTxn.restore(snap, tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg)
    steps = again.commit_steps()
    for _, forward, _ in steps:
        forward()
    assert_same_state(src, RepoBuilder(handover_root / "fresh"), excluded=[b".env"], ignored=[b"ignored", b"build.log"])
    assert_same_state(src, tgt, excluded=[b".env"], ignored=[b"ignored", b"build.log"])
    third = materialize.WorkspaceTxn.restore(
        json.loads(json.dumps(again.snapshot())), tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg
    )
    for _, _, inverse in reversed(third.commit_steps()):
        inverse()
    assert observe() == before and not (handover_root / "fresh").exists()
