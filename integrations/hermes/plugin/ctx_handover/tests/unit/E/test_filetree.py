"""File trees without git (DESIGN §9.6, §10.3, §10.5; SEC 2.3, 2.5, 2.7)."""

from __future__ import annotations

import os
import socket
import stat
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitops
from ctx_handover.engine.errors import ConflictError, PayloadTooLarge
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.jsonutil import canonical_bytes
from ctx_handover.engine.model import DestMode
from ctx_handover.engine.paths import rules_for
from ctx_handover.tests.fakes.repo_builder import tree_digest

LINUX = rules_for("linux", False)


def _make_tree(root: Path) -> None:
    (root / "src" / "pkg").mkdir(parents=True)
    (root / "src" / "pkg" / "mod.py").write_text("print('x')\n")
    (root / "run.sh").write_text("#!/bin/sh\n")
    os.chmod(root / "run.sh", 0o755)  # noqa: S103 - executable test fixture
    (root / "README.md").write_text("readme\n")
    (root / "node_modules" / "dep").mkdir(parents=True)
    (root / "node_modules" / "dep" / "index.js").write_text("x")
    (root / ".gitignore").write_text("*.tmp\n")
    (root / "scratch.tmp").write_text("t")
    (root / ".env").write_text("SECRET=1\n")
    (root / "id_ed25519").write_text("-----BEGIN OPENSSH PRIVATE KEY-----\n")
    os.symlink("README.md", root / "link-in")
    os.symlink("../outside", root / "link-out")
    os.mkfifo(root / "fifo")
    (root / os.fsdecode(b"n\xe4me.txt")).write_bytes(b"latin\n")
    (root / "new\nline.txt").write_text("nl\n")
    os.utime(root / "README.md", ns=(1_600_000_000_000_000_000, 1_600_000_000_000_000_000))


def _pack(root: Path, cfg, store) -> tuple[list, list, str]:
    entries, left = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    return entries, left, filetree.pack_tree(entries, root, store)


def _manifest(store, entries: list[dict]) -> str:
    doc = {"kind": "ctxh.tree", "v": 1, "entries": entries}
    return gitops.put_bytes(store, canonical_bytes(doc), "application/json").object_id


def test_filetree_scan_ignores_and_specials(user_home, cfg, store):
    """Ignore rules prune (``node_modules``, ``*.tmp``), secrets and specials are reported, links are recorded only
    when they stay inside, non-UTF-8 and newline names are kept [DESIGN §9.6]."""
    root = user_home / "tree"
    root.mkdir()
    _make_tree(root)
    entries, left = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    names = {e.path: e for e in entries}
    assert set(names) >= {
        "src",
        "src/pkg",
        "src/pkg/mod.py",
        "run.sh",
        "README.md",
        "link-in",
        ".gitignore",
        "new\nline.txt",
        "n\\xe4me.txt",
    }
    reasons = {n.path: n.why for n in left}
    assert reasons["node_modules"] == "ignored" and reasons["scratch.tmp"] == "ignored"
    assert reasons[".env"] == "secret" and reasons["id_ed25519"] == "secret"
    assert reasons["fifo"] == "special" and reasons["link-out"] == "symlink"
    assert names["run.sh"].mode == "100755" and names["README.md"].mode == "100644"
    assert names["link-in"].type == "symlink" and names["link-in"].target == "README.md"
    assert names["n\\xe4me.txt"].raw is not None


def test_filetree_roundtrip_bytes_names_modes_mtimes(user_home, cfg, store, handover_root):
    """Pack → fresh → commit reproduces bytes, exec bits, links, mtimes, non-UTF-8 and newline names."""
    root = user_home / "tree"
    root.mkdir()
    _make_tree(root)
    _, _, mid = _pack(root, cfg, store)
    dest = handover_root / "tree"
    state = filetree.prepare_fresh(store, mid, handover_root / ".ctxh-t", LINUX, dest=dest)
    assert (os.stat(handover_root / ".ctxh-t").st_mode & 0o777) == 0o700
    result = filetree.commit(state)
    assert not (handover_root / ".ctxh-t").exists()
    assert result["dest"] == str(dest)
    src_digest = tree_digest(root, exclude=())
    dst_digest = tree_digest(dest, exclude=())
    for skipped in (b"node_modules", b"scratch.tmp", b".env", b"id_ed25519", b"link-out", b"fifo"):
        src_digest = {k: v for k, v in src_digest.items() if not (k == skipped or k.startswith(skipped + b"/"))}
    assert dst_digest == src_digest
    assert os.stat(dest / "README.md").st_mtime_ns == 1_600_000_000_000_000_000
    assert (dest / os.fsdecode(b"n\xe4me.txt")).read_bytes() == b"latin\n"
    assert result["fingerprint"]["files"]["README.md"][0] == "file"


def test_filetree_rejects_unknown_type_and_setuid(user_home, store, handover_root):
    """★ entries of unknown type, with setuid/setgid/sticky modes, escaping links or ``.git`` names are skipped and
    reported; valid neighbours land, nothing special is created (mutant: tree entries not validated) [SEC 2.3]."""
    ok = gitops.put_bytes(store, b"ok\n", "application/octet-stream").object_id
    entries = [
        {"path": "fine.txt", "type": "file", "mode": "100644", "size": 3, "mtime_ns": 0, "object": ok},
        {"path": "suid", "type": "file", "mode": "104755", "size": 3, "mtime_ns": 0, "object": ok},
        {"path": "sgid", "type": "file", "mode": "102755", "size": 3, "mtime_ns": 0, "object": ok},
        {"path": "sticky", "type": "dir", "mode": "041777", "size": 0, "mtime_ns": 0},
        {"path": "fifo", "type": "fifo", "mode": "010644", "size": 0, "mtime_ns": 0},
        {"path": "dev", "type": "chardev", "mode": "020644", "size": 0, "mtime_ns": 0},
        {"path": "up", "type": "symlink", "mode": "120000", "size": 0, "mtime_ns": 0, "target": "../../etc"},
        {"path": "abs", "type": "symlink", "mode": "120000", "size": 0, "mtime_ns": 0, "target": "/etc/passwd"},
        {"path": ".GIT/config", "type": "file", "mode": "100644", "size": 3, "mtime_ns": 0, "object": ok},
        {"path": "a/../b", "type": "file", "mode": "100644", "size": 3, "mtime_ns": 0, "object": ok},
    ]
    state = filetree.prepare_fresh(
        store, _manifest(store, entries), handover_root / ".ctxh-x", LINUX, dest=handover_root / "t"
    )
    assert {n.path for n in state.skipped} == {e["path"] for e in entries[1:]}
    filetree.commit(state)
    found = []
    for dirpath, dirnames, filenames in os.walk(handover_root / "t"):
        for name in dirnames + filenames:
            st = os.lstat(os.path.join(dirpath, name))
            found.append(name)
            assert not st.st_mode & (stat.S_ISUID | stat.S_ISGID | stat.S_ISVTX)
            assert stat.S_ISREG(st.st_mode) or stat.S_ISDIR(st.st_mode)
    assert found == ["fine.txt"]


def test_filetree_fresh_symlinks_last_and_contained(user_home, store, handover_root):
    """Links are created after every file and directory; a file whose path runs through a link entry is written
    into a real directory, the link is then refused — nothing is ever written through a link."""
    ok = gitops.put_bytes(store, b"inner\n", "application/octet-stream").object_id
    outside = user_home / "outside"
    outside.mkdir()
    entries = [
        {"path": "a-link", "type": "symlink", "mode": "120000", "size": 0, "mtime_ns": 0, "target": "z-dir/f.txt"},
        {"path": "ln", "type": "symlink", "mode": "120000", "size": 0, "mtime_ns": 0, "target": "z-dir"},
        {"path": "ln/x.txt", "type": "file", "mode": "100644", "size": 6, "mtime_ns": 0, "object": ok},
        {"path": "z-dir", "type": "dir", "mode": "040755", "size": 0, "mtime_ns": 0},
        {"path": "z-dir/f.txt", "type": "file", "mode": "100644", "size": 6, "mtime_ns": 0, "object": ok},
    ]
    dest = handover_root / "t"
    state = filetree.prepare_fresh(store, _manifest(store, entries), handover_root / ".ctxh-l", LINUX, dest=dest)
    filetree.commit(state)
    assert (dest / "a-link").is_symlink() and (dest / "a-link").read_text() == "inner\n"
    assert (dest / "ln").is_dir() and not (dest / "ln").is_symlink()
    assert (dest / "ln" / "x.txt").read_text() == "inner\n"
    assert ("ln", "conflict") in {(n.path, n.why) for n in state.skipped} or not (dest / "ln").is_symlink()
    assert list(outside.iterdir()) == []


def _fresh_tree(user_home, cfg, store, handover_root):
    root = user_home / "tree"
    root.mkdir(exist_ok=True)
    (root / "a.txt").write_text("a\n")
    (root / "b.txt").write_text("b\n")
    (root / "sub").mkdir(exist_ok=True)
    (root / "sub" / "c.txt").write_text("c\n")
    _, _, mid = _pack(root, cfg, store)
    dest = handover_root / "tree"
    state = filetree.prepare_fresh(store, mid, handover_root / ".ctxh-f", LINUX, dest=dest)
    return root, dest, filetree.commit(state)["fingerprint"]


def test_filetree_update_only_if_fingerprint_matches(user_home, cfg, store, handover_root, tmp_path):
    """★ update mode only over exactly what the plugin wrote; any foreign change ⇒ conflict and nothing touched
    (mutant: fingerprint comparison always succeeds) [SEC 2.7, I5]."""
    root, dest, fp = _fresh_tree(user_home, cfg, store, handover_root)
    (root / "a.txt").write_text("a2\n")
    (root / "new.txt").write_text("n\n")
    (root / "b.txt").unlink()
    _, _, mid = _pack(root, cfg, store)
    (dest / "sub" / "c.txt").write_text("edited by the target user\n")
    before = tree_digest(dest, exclude=())
    with pytest.raises(ConflictError) as exc:
        filetree.prepare_update(store, mid, dest, fp, LINUX, tmp_path / "bk")
    assert exc.value.details["kind"] == "foreign"
    assert tree_digest(dest, exclude=()) == before
    (dest / "sub" / "c.txt").write_text("c\n")
    os.utime(dest / "sub" / "c.txt", ns=(1, 1))
    state = filetree.prepare_update(store, mid, dest, fp, LINUX, tmp_path / "bk")
    assert state.mode is DestMode.UPDATE
    filetree.commit(state)
    assert (dest / "a.txt").read_text() == "a2\n" and (dest / "new.txt").exists() and not (dest / "b.txt").exists()
    assert not [p for p in dest.rglob(".*ctxh-*")]


def test_filetree_update_rechecks_before_replace(user_home, cfg, store, handover_root, tmp_path):
    """A file changed between prepare and commit stops the commit before anything is replaced [SEC 2.7]."""
    root, dest, fp = _fresh_tree(user_home, cfg, store, handover_root)
    (root / "a.txt").write_text("a2\n")
    (root / "b.txt").write_text("b2\n")
    _, _, mid = _pack(root, cfg, store)
    state = filetree.prepare_update(store, mid, dest, fp, LINUX, tmp_path / "bk")
    (dest / "b.txt").write_text("typed after prepare\n")
    with pytest.raises(ConflictError):
        filetree.commit(state)
    assert (dest / "a.txt").read_text() == "a\n"
    assert (dest / "b.txt").read_text() == "typed after prepare\n"
    filetree.discard(state)
    assert not [p for p in dest.rglob(".*ctxh-*")]


def test_filetree_update_rollback_restores_backups(user_home, cfg, store, handover_root, tmp_path):
    root, dest, fp = _fresh_tree(user_home, cfg, store, handover_root)
    before = tree_digest(dest, exclude=())
    (root / "a.txt").write_text("a2\n")
    (root / "sub" / "c.txt").unlink()
    (root / "deeper").mkdir()
    (root / "deeper" / "d.txt").write_text("d\n")
    _, _, mid = _pack(root, cfg, store)
    state = filetree.prepare_update(store, mid, dest, fp, LINUX, tmp_path / "bk")
    filetree.commit(state)
    assert (dest / "deeper" / "d.txt").exists() and not (dest / "sub" / "c.txt").exists()
    filetree.rollback(state)
    assert tree_digest(dest, exclude=()) == before


def test_sec_tree_manifest_entry_cap_on_target(store, handover_root):
    """max_files is enforced on the received manifest before anything is written [SEC 2.5]."""
    entries = [{"path": f"f{i}", "type": "dir", "mode": "040755", "size": 0, "mtime_ns": 0} for i in range(11)]
    partial = handover_root / ".ctxh-cap"
    with pytest.raises(PayloadTooLarge):
        filetree.prepare_fresh(store, _manifest(store, entries), partial, LINUX, dest=handover_root / "t", max_files=10)
    assert not partial.exists()


def test_filetree_case_collisions_on_case_insensitive_target(store, handover_root):
    ok = gitops.put_bytes(store, b"x", "application/octet-stream").object_id
    entries = [
        {"path": "README", "type": "file", "mode": "100644", "size": 1, "mtime_ns": 0, "object": ok},
        {"path": "Readme", "type": "file", "mode": "100644", "size": 1, "mtime_ns": 0, "object": ok},
    ]
    state = filetree.prepare_fresh(
        store, _manifest(store, entries), handover_root / ".ctxh-c", rules_for("darwin", True), dest=handover_root / "t"
    )
    assert [(n.path, n.why) for n in state.skipped] == [("Readme", "case_collision")]


def test_filetree_corrupt_block_never_lands(user_home, cfg, store, handover_root):
    """A file whose content does not match its manifest is never left behind (the partial is removed)."""
    root = user_home / "tree"
    root.mkdir()
    (root / "a.txt").write_bytes(os.urandom(600_000))
    entries, _ = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    mid = filetree.pack_tree(entries, root, store)
    doc = gitops.read_json_object(store, mid)
    obj = doc["entries"][0]["object"]
    store.corrupt(store.manifests([obj])[0].blocks[1])
    partial = handover_root / ".ctxh-bad"
    with pytest.raises(Exception):  # noqa: B017 - BlockCorrupt
        filetree.prepare_fresh(store, mid, partial, LINUX, dest=handover_root / "t")
    assert not partial.exists()


def test_filetree_scan_skips_socket(user_home, cfg, monkeypatch):
    root = user_home / "tree"
    root.mkdir()
    # AF_UNIX addresses are limited to ~108 bytes: bind by a short relative name from inside the tree, so a long
    # ``--basetemp`` does not matter (the scan itself uses the absolute root)
    monkeypatch.chdir(root)
    sock = socket.socket(socket.AF_UNIX)
    try:
        sock.bind("s.sock")
        _, left = filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)
    finally:
        sock.close()
    assert [(n.path, n.why) for n in left] == [("s.sock", "special")]
