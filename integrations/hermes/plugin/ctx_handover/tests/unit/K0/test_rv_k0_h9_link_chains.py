"""rv-k0, review finding H9 (``review-code/REVIEW.md`` §2, g5 H2) and L11 (g4 L-3): link targets of a peer's tree.

DESIGN §10.3: symlinks of a received tree are created "only relative and inside the root". ``check_tree_entry``
judged each target on its own, lexically: ``a/b/c/u -> ../../..`` and ``l -> a/b/c/u/../../../..`` both stay inside
on paper, but the kernel resolves ``u`` first, so ``l`` ends four levels above the root and ``k -> l/etc/passwd``
reaches any absolute file (reviewer probe ``g5-git/probes/test_zz_g5_probe.py::test_probe_tree_symlink_chain_
escapes_root``). The plugin never writes through links; the damage is done by whoever later writes into the tree.

Rule now: ``..`` may only lead a target. Every directory of a received tree is a real directory (``filetree``
refuses a link as a parent), so leading ``..`` climb real directories exactly as counted, and the components after
them only descend — through a link at worst, whose own target obeys the same rule. A component made only of dots
and spaces that is not ``.`` or ``..`` is refused too (Windows drops trailing dots and spaces). L11: the ``.git``
check of link targets is ``paths.is_dotgit`` (the one of ``check_rel``: case, HFS-ignorable code points, ``git~1``,
trailing dots/spaces, NTFS stream suffix).
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitops, paths, policy
from ctx_handover.engine.errors import PolicyDenied
from ctx_handover.engine.jsonutil import canonical_bytes
from ctx_handover.engine.policy import Verdict
from ctx_handover.tests.fakes.git_fake import DirBlockStore

LINUX = paths.rules_for("linux", False)
ZWNJ = chr(0x200C)
ZWJ = chr(0x200D)


def _d(path: str) -> dict:
    return {"path": path, "type": "dir", "mode": "040755", "size": 0, "mtime_ns": 0}


def _ln(path: str, target: str) -> dict:
    return {"path": path, "type": "symlink", "mode": "120000", "size": 0, "mtime_ns": 0, "target": target}


PROBE3 = [
    _d("a"),
    _d("a/b"),
    _d("a/b/c"),
    _ln("a/b/c/u", "../../.."),
    _ln("l", "a/b/c/u/../../../.."),
    _ln("k", "l/etc/passwd"),
]


@pytest.mark.parametrize(
    ("path", "target"),
    [
        ("l", "a/b/c/u/../../../.."),
        ("x", "a/../b"),
        ("d/x", "../a/../../etc"),
        ("d/x", "e/../.."),
        ("x", "u/.."),
    ],
)
def test_dotdot_after_a_name_is_refused(path, target):
    """★ H9: ``..`` behind a name resolves against what that name really is (maybe a link of the same tree). Before
    the fix every one of these was ``Verdict(True, None)``."""
    assert policy.check_tree_entry(_ln(path, target)) == Verdict(False, "symlink")


@pytest.mark.parametrize(
    ("path", "target"),
    [
        ("a/b/c/u", "../../.."),
        ("k", "l/etc/passwd"),
        ("d/l", "../a"),
        ("d/e/l", "../../a/b"),
        ("l", "./a/./b"),
        ("l", "a"),
    ],
)
def test_leading_dotdot_and_plain_descents_stay_allowed(path, target):
    assert policy.check_tree_entry(_ln(path, target)) == Verdict(True, None)


@pytest.mark.parametrize("target", [".. /etc", "... ", "d/.../x", ". /x", "../.. /x", ".. "])
def test_dot_and_space_only_components_are_refused(target):
    """★ A component of dots and spaces other than ``.``/``..``: Windows strips trailing dots and spaces, so
    ``.. `` may act as ``..`` there. Before the fix ``.. /etc`` was ``Verdict(True, None)``."""
    assert policy.check_tree_entry(_ln("d/l", target)).allowed is False


@pytest.mark.parametrize(
    "target",
    [
        f".g{ZWNJ}it/config",
        f"x/.G{ZWJ}IT/hooks/pre-commit",
        ".git./config",
        ".git::$DATA",
        "GIT~1/config",
        ".Git /hooks",
    ],
)
def test_link_into_git_in_any_spelling_is_refused(target):
    """★ L11: one ``.git`` rule for entry paths and link targets (``paths.is_dotgit``). Before the fix the HFS and NTFS
    spellings passed."""
    assert policy.check_tree_entry(_ln("l", target)) == Verdict(False, "symlink")


def test_is_dotgit_is_the_check_rel_rule():
    for name in (".git", ".GIT", f".g{ZWNJ}it", "git~1", ".git.", ".git ", ".git::$DATA"):
        assert paths.is_dotgit(name), name
        with pytest.raises(PolicyDenied):
            paths.check_rel(f"a/{name}/b", LINUX)
    for name in ("git", ".gitignore", ".github", "x.git"):
        assert not paths.is_dotgit(name), name


def _manifest(store: DirBlockStore, entries: list[dict]) -> str:
    doc = {"kind": "ctxh.tree", "v": 1, "entries": entries}
    return gitops.put_bytes(store, canonical_bytes(doc), "application/json").object_id


def _inside(root: Path, path: Path) -> bool:
    real, base = os.path.realpath(path), os.path.realpath(root)
    return real == base or real.startswith(base + os.sep)


def test_probe3_chain_through_the_production_tree_path(tmp_path):
    """★ The reviewer's probe 3 through ``filetree.prepare_fresh`` + ``commit``: no link of the placed tree resolves
    outside its root, and the climbing link is reported as not transferred (``symlink``). Before the fix ``l`` was
    created and ``realpath(l)`` lay four levels above the root."""
    store = DirBlockStore(tmp_path / "store")
    dest = tmp_path / "w" / "x" / "y" / "z" / "proj" / "t"
    dest.parent.mkdir(parents=True)
    state = filetree.prepare_fresh(store, _manifest(store, PROBE3), dest.parent / ".ctxh-s", LINUX, dest=dest)
    filetree.commit(state)
    links = [p for p in dest.rglob("*") if p.is_symlink()]
    assert links, "the in-tree links are still created"
    for link in links:
        assert _inside(dest, link), (link, os.readlink(link), os.path.realpath(link))
    assert not os.path.lexists(dest / "l")
    assert ("l", "symlink") in {(s.path, s.why) for s in state.skipped}
