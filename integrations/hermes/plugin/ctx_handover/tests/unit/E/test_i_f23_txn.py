"""Lab finding L3-F23 (BRIEF D10, R4, R19, R29, DESIGN §9.7): every fallback the workspace
transaction builds — a conflict found while PREPARE updates or returns an item, a fresh destination that is taken
when c1 renames — comes from one rule, ``pathmap.fallback_dest`` with the target's policy: the sibling
``<dest>-handover-<stamp>`` only while it lies under ``allow_roots`` and passes the target check, else
``<workspace_root>/<basename>-handover-<stamp>`` (the schema default is ``<HERMES_HOME>/handover-workspaces`` when
it lies in the Hermes home, R29). A destination outside ``allow_roots`` (a return-trip origin, a recorded or typed
path) never gets a fallback beside it.

Red before the fix: ``PolicyDenied: fallback destination not allowed (not_allowed_root)`` from PREPARE, and at c1 a
fallback beside the destination that the target policy refuses."""

from __future__ import annotations

import time
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitops, materialize
from ctx_handover.engine.errors import ConflictError
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.model import (
    DestMode,
    Include,
    ItemKind,
    ItemRole,
    MapRow,
    Origin,
    PlanItem,
    TargetDecision,
)
from ctx_handover.engine.policy import check_target_dest
from ctx_handover.tests.fakes.git_fake import RecordingJournal
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint
from ctx_handover.tests.unit.E._support import AT, apply_fresh, make_cfg, pack

STAMP = time.strftime("%Y%m%d-%H%M%S", time.localtime(AT))
JOB = "01JOB000000000000000000F23XY"
NOTES = "SERVER-ONLY NOTES\n"
USER_DATA = "TARGET USER DATA\n"


def _plan_item(kind: ItemKind, source: str, item_id: str) -> PlanItem:
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=source,
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=1000,
    )


def _txn(tmp_path: Path, git, store, cfg, **kw) -> materialize.WorkspaceTxn:
    return materialize.WorkspaceTxn(
        JOB, tmp_path / "txn", git, store, RecordingJournal(), cfg=cfg, now=lambda: AT, **kw
    )


def _exclude(repo: RepoBuilder, *patterns: str) -> None:
    info = repo.root / ".git" / "info"
    info.mkdir(exist_ok=True)
    (info / "exclude").write_text("".join(p + "\n" for p in patterns), encoding="utf-8")


def _commit_all(txn: materialize.WorkspaceTxn) -> tuple[str, DestMode]:
    for _, forward, _ in txn.commit_steps():
        forward()
    ((_, final, mode),) = txn.dests()
    return final, mode


def _allowed_without_record(path: str, cfg) -> bool:
    return check_target_dest(Path(path), cfg, recorded=False, local_choice=False).allowed


def test_i_f23_txn_return_conflict_at_an_origin_outside_the_roots(git, user_home, tmp_path, staging, cfg, store):
    """★ Return trip (R4) to ``~/server`` (outside ``allow_roots``), the server ignores ``notes.txt`` and the laptop
    sends its own: PREPARE refuses the return (B2) and the item lands in ``<workspace_root>/server-handover-<stamp>``;
    the origin is byte-identical; the warning names origin and fallback (mutant: the sibling beside the origin)."""
    handover = user_home / "hermes-handover"
    server = RepoBuilder.init(user_home / "server")
    server.write("a.txt", "a\n")
    server.commit_all("c1")
    _exclude(server, "notes.txt")
    server.write("notes.txt", NOTES)
    out, item = pack(git, server.root, staging, cfg, store, item_id="out")
    laptop = RepoBuilder(apply_fresh(git, store, item, handover / "laptop", cfg))
    laptop.write("notes.txt", "laptop notes\n")
    laptop.write("b.txt", "b\n")
    basis = gitops.GitProbe(git).ref_tips(str(server.root))
    _, back = pack(git, laptop.root, staging, cfg, store, basis=basis, item_id="back")
    before = dir_fingerprint(server.root)
    assert not _allowed_without_record(str(server.root) + f"-handover-{STAMP}", cfg)

    txn = _txn(tmp_path, git, store, cfg)
    row = MapRow(str(server.root), "peer", str(laptop.root), "out", state=materialize.map_state_of(out.state))
    decision = TargetDecision("back", str(server.root), DestMode.RETURN, "return-mapping", None)
    txn.prepare_item(_plan_item(ItemKind.GIT_REPO, str(laptop.root), "back"), decision, back, out_row=row)
    final, mode = _commit_all(txn)

    assert (final, mode) == (str(handover / f"server-handover-{STAMP}"), DestMode.FRESH)
    assert _allowed_without_record(final, cfg)
    assert (Path(final) / "notes.txt").read_text() == "laptop notes\n"
    assert dir_fingerprint(server.root) == before
    assert any(str(server.root) in w and final in w for w in txn.warnings), txn.warnings
    assert not list(user_home.glob("server-handover-*"))


def test_i_f23_txn_update_conflict_at_a_typed_dest_outside_the_roots(git, user_home, tmp_path, staging, cfg, store):
    """★ Update mode at ``~/work/app`` (typed on this host, outside ``allow_roots``) with an ignored file in the
    way: ``<workspace_root>/app-handover-<stamp>``, the copy at ``~/work/app`` untouched (mutant: the sibling)."""
    handover = user_home / "hermes-handover"
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    _, seed = pack(git, src.root, staging, cfg, store, item_id="seed")
    tgt = RepoBuilder(apply_fresh(git, store, seed, user_home / "work" / "app", cfg))
    tgt.run("stash", "clear")
    tgt.run("reset", "-q", "--hard")
    tgt.run("clean", "-qfdx")
    _exclude(tgt, "local.cfg")
    tgt.write("local.cfg", USER_DATA)
    src.write("local.cfg", "from source\n")
    _, item = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(tgt.root)), item_id="u")
    before = dir_fingerprint(tgt.root)

    txn = _txn(tmp_path, git, store, cfg)
    decision = TargetDecision("u", str(tgt.root), DestMode.UPDATE, "user", None)
    txn.prepare_item(_plan_item(ItemKind.GIT_REPO, str(src.root), "u"), decision, item)
    final, mode = _commit_all(txn)

    assert (final, mode) == (str(handover / f"app-handover-{STAMP}"), DestMode.FRESH)
    assert (Path(final) / "local.cfg").read_text() == "from source\n"
    assert dir_fingerprint(tgt.root) == before
    assert not list((user_home / "work").glob("app-handover-*"))


@pytest.mark.parametrize("mode", [DestMode.UPDATE, DestMode.RETURN])
def test_i_f23_txn_tree_conflict_outside_the_roots(git, user_home, tmp_path, cfg, store, mode):
    """★ File trees: an update/return at ``~/notes`` (outside ``allow_roots``) without a usable fingerprint goes to
    ``<workspace_root>/notes-handover-<stamp>`` (mutant: the sibling, refused ⇒ ``PolicyDenied``)."""
    handover = user_home / "hermes-handover"
    src = user_home / "src-notes"
    src.mkdir()
    (src / "n.md").write_text("from source\n")
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    titem = {"item_id": "t1", "kind": "file_tree", "manifest": filetree.pack_tree(entries, src, store)}
    dest = user_home / "notes"
    dest.mkdir()
    (dest / "n.md").write_text("mine\n")
    before = dir_fingerprint(dest)

    txn = _txn(tmp_path, git, store, cfg)
    txn.prepare_item(
        _plan_item(ItemKind.FILE_TREE, str(src), "t1"),
        TargetDecision("t1", str(dest), mode, "return-mapping", None),
        titem,
    )
    final, got_mode = _commit_all(txn)

    assert (final, got_mode) == (str(handover / f"notes-handover-{STAMP}"), DestMode.FRESH)
    assert (Path(final) / "n.md").read_text() == "from source\n"
    assert dir_fingerprint(dest) == before


def test_i_f23_txn_fresh_dest_taken_at_commit_never_falls_back_beside_it(git, user_home, tmp_path, cfg, store):
    """★ c1 of a fresh item at a recorded destination outside ``allow_roots`` (the origin of a return trip that was
    gone at negotiation): something appears there between PREPARE and c1. The partial lies beside the destination,
    only a rename in that directory is possible, and a sibling there is outside ``allow_roots`` ⇒ the step ends
    with ``dest_appeared``; nothing is written beside the destination, what appeared keeps its bytes, the partial
    stays private and ``discard`` removes it (mutants: the sibling of the old rule ⇒ ``~/origin-handover-…``; a
    rename across directories into the existing ``workspace_root`` — a different parent, possibly another file
    system)."""
    handover = user_home / "hermes-handover"
    handover.mkdir()
    src = user_home / "src-tree"
    src.mkdir()
    (src / "a.md").write_text("a\n")
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    titem = {"item_id": "t1", "kind": "file_tree", "manifest": filetree.pack_tree(entries, src, store)}
    dest = user_home / "origin"

    txn = _txn(tmp_path, git, store, cfg)
    decision = TargetDecision("t1", str(dest), DestMode.FRESH, "return-mapping", None)
    txn.prepare_item(_plan_item(ItemKind.FILE_TREE, str(src), "t1"), decision, titem)
    dest.mkdir()
    (dest / "appeared.txt").write_text("someone else\n")
    ((_, forward, _),) = txn.commit_steps()
    with pytest.raises(ConflictError) as exc:
        forward()
    assert exc.value.details["kind"] == "dest_appeared"
    assert sorted(p.name for p in dest.iterdir()) == ["appeared.txt"]
    assert not list(user_home.glob("origin-handover-*"))
    assert list(handover.iterdir()) == []
    txn.discard()
    assert sorted(p.name for p in user_home.iterdir() if p.name.startswith(".ctxh-")) == []


def test_i_f23_txn_fresh_dest_under_the_roots_keeps_the_sibling(git, user_home, tmp_path, cfg, store):
    """The unchanged half of the rule (DESIGN §9.7, V35): a destination whose parent lies under ``allow_roots``
    falls back to its sibling ``<dest>-handover-<stamp>`` — here a nested one, so the sibling is not
    ``<workspace_root>/<basename>-…`` (guards against a rule that always moves to the root)."""
    handover = user_home / "hermes-handover"
    src = user_home / "src-tree"
    src.mkdir()
    (src / "a.md").write_text("a\n")
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    titem = {"item_id": "t1", "kind": "file_tree", "manifest": filetree.pack_tree(entries, src, store)}
    dest = handover / "team" / "proj"

    txn = _txn(tmp_path, git, store, cfg)
    txn.prepare_item(
        _plan_item(ItemKind.FILE_TREE, str(src), "t1"),
        TargetDecision("t1", str(dest), DestMode.FRESH, "workspace-root", None),
        titem,
    )
    dest.mkdir(parents=True)
    (dest / "x").write_text("x\n")
    final, _ = _commit_all(txn)
    assert final == str(handover / "team" / f"proj-handover-{STAMP}")


def test_i_f23_txn_uses_the_configured_workspace_root(git, user_home, tmp_path, store, hermes_home):
    """★ ``WorkspaceTxn(workspace_root=…)`` (additive; ``LocalTarget`` passes ``Options.workspace_root``): with
    ``workspace_root: ~/received`` and both roots allowed, the conflict fallback of ``~/notes`` lies in
    ``~/received`` (mutant: the setting ignored ⇒ the schema default ``~/hermes-handover``)."""
    cfg = make_cfg(user_home, hermes_home, allow_roots=("~/hermes-handover", "~/received"), workspace_root="~/received")
    src = user_home / "src-notes"
    src.mkdir()
    (src / "n.md").write_text("from source\n")
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    titem = {"item_id": "t1", "kind": "file_tree", "manifest": filetree.pack_tree(entries, src, store)}
    dest = user_home / "notes"
    dest.mkdir()
    (dest / "n.md").write_text("mine\n")

    txn = _txn(tmp_path, git, store, cfg, workspace_root="~/received")
    txn.prepare_item(
        _plan_item(ItemKind.FILE_TREE, str(src), "t1"),
        TargetDecision("t1", str(dest), DestMode.UPDATE, "return-mapping", None),
        titem,
    )
    final, _ = _commit_all(txn)
    assert final == str(user_home / "received" / f"notes-handover-{STAMP}")


def test_i_f23_txn_docker_layout_falls_back_into_the_reserved_directory(git, tmp_path, store):
    """★ R29: HOME = HERMES_HOME (official image) — the default root is ``<HERMES_HOME>/handover-workspaces``; a
    conflict at a recorded destination outside it lands there and the target policy accepts it (mutant: the
    policy's homes not handed to the rule ⇒ ``~/hermes-handover`` of this process)."""
    data = tmp_path / "opt-data"
    data.mkdir()
    cfg = make_cfg(data, data)
    reserved = data / "handover-workspaces"
    assert [Path(r) for r in cfg.allow_roots] == [reserved]
    src = tmp_path / "src-notes"
    src.mkdir()
    (src / "n.md").write_text("from source\n")
    entries, _ = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    titem = {"item_id": "t1", "kind": "file_tree", "manifest": filetree.pack_tree(entries, src, store)}
    dest = tmp_path / "srv" / "notes"
    dest.mkdir(parents=True)
    (dest / "n.md").write_text("mine\n")

    txn = _txn(tmp_path, git, store, cfg)
    txn.prepare_item(
        _plan_item(ItemKind.FILE_TREE, str(src), "t1"),
        TargetDecision("t1", str(dest), DestMode.RETURN, "return-mapping", None),
        titem,
    )
    final, _ = _commit_all(txn)
    assert final == str(reserved / f"notes-handover-{STAMP}")
    assert _allowed_without_record(final, cfg)
