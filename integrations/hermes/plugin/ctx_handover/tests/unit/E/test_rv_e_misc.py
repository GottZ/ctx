"""Review round rv-e, Medium/Low findings in cluster E files: M23 (the index tree I is capped like W, before
anything is checked out) and L16 (a fresh file-tree commit whose destination and fallback both exist raises a
``ConflictError`` and leaves the partial 0700)."""

from __future__ import annotations

import os

import pytest

from ctx_handover.engine import filetree, gitapply, gitops
from ctx_handover.engine.errors import ConflictError, PayloadTooLarge
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import LINUX, make_cfg, pack


def test_rv_e_m23_index_tree_capped(git, user_home, hermes_home, staging, store, handover_root):
    """★ M23: a small pack whose index tree I has more entries than ``max_files`` (W is tiny) is refused before
    the checkout; nothing remains (mutant: only W counted)."""
    cfg = make_cfg(user_home, hermes_home, max_files=500)
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "a\n")
    src.commit_all("c1")
    spec = gitops.GitEnvSpec(profile="source", ceiling=src.root.parent)
    blob = git.out(["hash-object", "-w", "--stdin"], cwd=src.root, spec=spec, input=b"x")
    sub_body = b"".join(b"100644 f%04d\x00" % i + bytes.fromhex(blob) for i in range(40))
    sub = git.out(["hash-object", "-w", "-t", "tree", "--stdin"], cwd=src.root, spec=spec, input=sub_body)
    root_body = b"".join(b"40000 d%04d\x00" % i + bytes.fromhex(sub) for i in range(40))
    big = git.out(["hash-object", "-w", "-t", "tree", "--stdin"], cwd=src.root, spec=spec, input=root_body)
    git.run(["read-tree", big], cwd=src.root, spec=spec)
    _, item = pack(git, src.root, staging, make_cfg(user_home, hermes_home), store)
    w_entries = gitops.GitProbe(git, profile="received").ls_tree_sizes(str(src.root), item["worktree_tree"])[1]
    assert w_entries <= cfg.max_files
    partial = handover_root / ".ctxh-app"
    with pytest.raises(PayloadTooLarge) as exc:
        gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert exc.value.details["field"] == "max_files"
    assert not partial.exists()
    assert list(handover_root.iterdir()) == []


def test_rv_e_l16_tree_fresh_fallback_exists_too(user_home, cfg, store, handover_root):
    """★ L16: destination and fallback both appeared ⇒ ``ConflictError(dest_appeared)``, the partial stays and is
    0700 again (mutant: the raw ``OSError`` of the second rename propagates with the partial left open)."""
    root = user_home / "tree"
    root.mkdir()
    (root / "a.txt").write_text("a\n")
    entries, _ = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    dest, fb = handover_root / "t", handover_root / "t-fb"
    partial = handover_root / ".ctxh-t"
    state = filetree.prepare_fresh(
        store, filetree.pack_tree(entries, root, store), partial, LINUX, dest=dest, fallback=fb
    )
    for d in (dest, fb):
        d.mkdir()
        (d / "theirs.txt").write_text("theirs\n")
    with pytest.raises(ConflictError) as exc:
        filetree.commit(state)
    assert exc.value.details["kind"] == "dest_appeared"
    assert os.stat(partial).st_mode & 0o777 == 0o700
    assert (partial / "a.txt").read_text() == "a\n"
    assert [p.name for p in fb.iterdir()] == ["theirs.txt"]


def test_rv_e_l17_user_attributes_file_not_applied_to_received_tree(git, user_home, staging, cfg, store, handover_root):
    """★ L17: the target user's global attributes file (``$XDG_CONFIG_HOME/git/attributes``, read even with the
    global config nulled) selects no conversion for a received tree — the files arrive byte for byte (mutant:
    ``core.attributesFile=/dev/null`` dropped from the received profile)."""
    src = RepoBuilder.init(user_home / "src")
    src.write("a.txt", "line one\nline two\n")
    src.commit_all("c1")
    src.write("b.txt", "untracked\n")
    _, item = pack(git, src.root, staging, cfg, store)
    xdg = os.environ.get("XDG_CONFIG_HOME") or str(user_home / ".config")
    attributes = os.path.join(xdg, "git", "attributes")
    os.makedirs(os.path.dirname(attributes), exist_ok=True)
    with open(attributes, "w", encoding="utf-8") as fh:
        fh.write("*.txt text eol=crlf\n")
    partial = handover_root / ".ctxh-app"
    state = gitapply.prepare_fresh(git, store, item, partial, rules=LINUX, cfg=cfg, dest=handover_root / "app")
    assert (partial / "a.txt").read_bytes() == b"line one\nline two\n"
    assert (partial / "b.txt").read_bytes() == b"untracked\n"
    gitapply.discard(git, state)
