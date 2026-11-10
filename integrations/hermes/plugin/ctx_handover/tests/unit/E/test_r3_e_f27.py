"""Lab finding L3-F27 (rest of review H9): a link the policy leaves out of a file tree is reported — in the pack
item's ``excluded`` list, which the conductor turns into ``left_behind`` (report, note, status; R30 form) — and a
link whose target leads through a left-out link goes with it, so a chain travels whole or not at all, never as a
dangling half. Both on the source (``filetree.scan`` via ``materialize.pack_plan_item``) and at the target
(``filetree.prepare_fresh``/``prepare_update`` for a manifest of any peer; DESIGN §10.3, REVIEW H9)."""

from __future__ import annotations

import os
from pathlib import Path

from ctx_handover.engine import filetree, gitops, materialize
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.jsonutil import canonical_bytes
from ctx_handover.engine.model import Include, ItemKind, ItemRole, Origin, PlanItem
from ctx_handover.tests.unit.E._support import LINUX


def _lab_tree(root: Path) -> None:
    """The lab's tree (``test_rv_h9_symlink_chain_does_not_lead_out_of_the_received_tree``) plus a longer chain and
    a chain through a link that stays."""
    (root / "a" / "b" / "c").mkdir(parents=True)
    (root / "sub").mkdir()
    (root / "data.txt").write_text("data\n")
    (root / "sub" / "f.txt").write_text("f\n")
    os.symlink("../../..", root / "a" / "b" / "c" / "u")  # the root itself: stays
    os.symlink("a/b/c/u/../../../..", root / "escape-link")  # leads out: left out
    os.symlink("escape-link/escape-probe.txt", root / "escape-chain")  # through it
    os.symlink("escape-chain", root / "chain-2")  # through the chain
    os.symlink("sub", root / "v")  # stays
    os.symlink("v/f.txt", root / "via-v")  # through a link that stays: stays
    os.symlink("missing.txt", root / "dangling")  # the user's own dangling link: stays as it is


LEFT_OUT = {"escape-link", "escape-chain", "chain-2"}
KEPT_LINKS = {"a/b/c/u", "v", "via-v", "dangling"}


def _plan_item(root: Path) -> PlanItem:
    return PlanItem(
        item_id="t",
        kind=ItemKind.FILE_TREE,
        source_path=str(root),
        role=ItemRole.PRIMARY,
        origin=Origin.USER,
        include=Include.YES,
        reasons=("include",),
        size_estimate=1000,
    )


def test_r3_e_f27_source_reports_left_out_links_and_their_chains(git, user_home, tmp_path, cfg, store):
    """★ L3-F27 at the source: ``escape-link`` (leads out of the tree) and the links through it (``escape-chain``,
    ``chain-2``) are not in the manifest and stand in the pack item's ``excluded`` list with why ``symlink`` — the list
    the conductor reports as left behind; the links that resolve inside stay (mutants: the refused link only in
    ``skipped``, which nobody reports; chains kept as dangling links)."""
    root = user_home / "rvh9tree"
    root.mkdir()
    _lab_tree(root)
    packed = materialize.pack_plan_item(
        _plan_item(root), git=git, store=store, cfg=cfg, workdir=tmp_path / "w", created_at=1.0
    )
    excluded = {(e["path"], e["why"]) for e in packed.doc["excluded"]}
    assert {(p, "symlink") for p in LEFT_OUT} <= excluded
    assert {(n.path, n.why) for n in packed.excluded} >= {(p, "symlink") for p in LEFT_OUT}
    assert not {p for p, _ in excluded} & KEPT_LINKS
    entries = filetree.load_manifest(store, packed.doc["manifest"], max_files=1000)
    links = {e["path"] for e in entries if e["type"] == "symlink"}
    assert links == KEPT_LINKS
    assert packed.doc["entries"] == len(entries)
    assert "escape-link" not in str(packed.doc["skipped"])


def test_r3_e_f27_scan_drops_whole_chains(user_home, cfg):
    """The scan itself: what it leaves out of the entries it names in its left list, once each."""
    root = user_home / "t"
    root.mkdir()
    _lab_tree(root)
    entries, left = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    assert {e.path for e in entries if e.type == "symlink"} == KEPT_LINKS
    gone = [(n.path, n.why) for n in left if n.path in LEFT_OUT]
    assert sorted(gone) == sorted((p, "symlink") for p in LEFT_OUT)


def test_r3_e_f27_ignored_link_takes_its_chain(user_home, cfg):
    """A link the ignore rules leave out takes the links through it along (reason ``symlink`` for those)."""
    root = user_home / "t"
    root.mkdir()
    (root / "real").mkdir()
    (root / "real" / "x.txt").write_text("x\n")
    (root / ".gitignore").write_text("cache-link\n")
    os.symlink("real", root / "cache-link")
    os.symlink("cache-link/x.txt", root / "through-ignored")
    entries, left = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    assert {e.path for e in entries if e.type == "symlink"} == set()
    assert ("through-ignored", "symlink") in {(n.path, n.why) for n in left}


def _manifest(store, entries: list[dict]) -> str:
    doc = {"kind": "ctxh.tree", "v": 1, "entries": entries}
    return gitops.put_bytes(store, canonical_bytes(doc), "application/json").object_id


def _d(p: str) -> dict:
    return {"path": p, "type": "dir", "mode": "040755", "size": 0, "mtime_ns": 0}


def _ln(p: str, t: str) -> dict:
    return {"path": p, "type": "symlink", "mode": "120000", "size": 0, "mtime_ns": 0, "target": t}


TARGET_ENTRIES = [
    _d("a"),
    _d("a/b"),
    _d("a/b/c"),
    _d("sub"),
    _ln("a/b/c/u", "../../.."),
    _ln("l", "a/b/c/u/../../../.."),  # refused by the policy
    _ln("k", "l/etc/passwd"),  # through l
    _ln("m", "k"),  # to k
    _ln("v", "sub"),
    _ln("sub/w", "../../out"),  # refused
    _ln("x", "v/w/y"),  # through v (stays) to sub/w (refused)
    _ln("p", "q"),  # a loop: no left-out link on its way, it stays as sent
    _ln("q", "p"),
]


def test_r3_e_f27_target_never_creates_half_chains(tmp_path, store):
    """★ L3-F27 at the target (a peer's manifest): ``l`` and ``sub/w`` are refused by the policy; ``k``, ``m`` and ``x``
    lead through them — also through the link ``v`` that stays — and are skipped and reported with them (why
    ``symlink``) instead of being created dangling (mutant: chains not followed). Links that resolve inside stay."""
    dest = tmp_path / "w" / "x" / "y" / "z" / "proj" / "t"
    dest.parent.mkdir(parents=True)
    state = filetree.prepare_fresh(store, _manifest(store, TARGET_ENTRIES), dest.parent / ".ctxh-s", LINUX, dest=dest)
    filetree.commit(state)
    skipped = {(s.path, s.why) for s in state.skipped}
    assert {("l", "symlink"), ("sub/w", "symlink"), ("k", "symlink"), ("m", "symlink"), ("x", "symlink")} <= skipped
    made = {str(p.relative_to(dest)) for p in dest.rglob("*") if p.is_symlink()}
    assert made == {"a/b/c/u", "v", "p", "q"}
    assert set(state.fingerprint["files"]) == {filetree.fp_key(os.fsencode(p)) for p in made}


def test_r3_e_f27_target_update_drops_chains_too(user_home, tmp_path, cfg, store, handover_root):
    """Update mode reads the same entry check: a chain through a refused link is not staged."""
    root = user_home / "tree"
    root.mkdir()
    (root / "a.txt").write_text("a\n")
    entries, _ = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    dest = handover_root / "tree"
    st = filetree.prepare_fresh(
        store, filetree.pack_tree(entries, root, store), handover_root / ".ctxh-f", LINUX, dest=dest
    )
    fp = filetree.commit(st)["fingerprint"]
    manifest = _manifest(store, [_ln("l", "../../out"), _ln("k", "l/x")])
    state = filetree.prepare_update(store, manifest, dest, fp, LINUX, tmp_path / "bk")
    assert {("l", "symlink"), ("k", "symlink")} <= {(s.path, s.why) for s in state.skipped}
    filetree.commit(state)
    assert not os.path.lexists(dest / "k") and not os.path.lexists(dest / "l")
