"""Layer-2 contracts of the workspace cluster: object ids (K-1/X01), probe ceiling and partial clones
(W088/W107), the map-row state (X09), the pack-item entry (X12), the in-memory stash sniff (W093) and the index
copy behind every state computation."""

from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitops, gitpack, jsonutil, materialize
from ctx_handover.engine.errors import BadRequest, BlockCorrupt, CapabilityMissing
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.model import (
    DestMode,
    GitState,
    Include,
    ItemKind,
    ItemRole,
    MapRow,
    ObjectManifest,
    Origin,
    PlanItem,
    TargetDecision,
)
from ctx_handover.tests.fakes.git_fake import RecordingJournal
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.unit.E._support import AT, append_repo_config, assert_same_state, env_facts, make_cfg

MEDIA = "application/octet-stream"


def _item(kind: ItemKind, source: Path, item_id: str = "w0", **kw) -> PlanItem:
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=str(source),
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=1000,
        **kw,
    )


def _pack(git, item, store, cfg, tmp_path, **kw) -> materialize.PackedItem:
    return materialize.pack_plan_item(
        item, git=git, store=store, cfg=cfg, workdir=tmp_path / "pack" / item.item_id, created_at=AT, **kw
    )


# ---------------------------------------------------------------------------------------------------------
# K-1 / X01 — one object-id rule, readers accept 524 288-byte blocks only
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("size", [0, 1, jsonutil.OBJECT_BLOCK_SIZE, jsonutil.OBJECT_BLOCK_SIZE * 2 + 7])
def test_object_ids_follow_k1(store, size):
    """``put_bytes`` and ``put_stream`` write the manifest ``jsonutil.object_manifest`` describes (an empty object
    has no blocks) and its id is ``jsonutil.object_id`` of that document."""
    data = os.urandom(size)
    doc, blocks = jsonutil.object_manifest(data, MEDIA)
    m = gitops.put_bytes(store, data, MEDIA)
    assert m.object_id == jsonutil.object_id(doc) == gitops.object_id_of(m)
    assert list(m.blocks) == [bid for bid, _ in blocks] and m.block_size == jsonutil.OBJECT_BLOCK_SIZE
    view = memoryview(data)
    pos = 0

    def read(n: int) -> bytes:
        nonlocal pos
        chunk = bytes(view[pos : pos + n])
        pos += len(chunk)
        return chunk

    assert gitops.put_stream(store, read, media=MEDIA).object_id == m.object_id
    assert gitops.read_object(store, m.object_id, max_size=size + 1) == data


def test_reader_accepts_only_k1_block_size(store):
    """★ X01 — a manifest with another block size is refused even when its id is computed correctly over it, and
    an id that does not match its manifest is a hash mismatch (mutant: the layer-1 reader rule ``0 < block_size ≤
    4 MiB``)."""
    data = os.urandom(3 * 1024 * 1024)
    big_blocks = []
    for start in range(0, len(data), 4 * 1024 * 1024):
        raw = data[start : start + 4 * 1024 * 1024]
        bid = jsonutil.sha256_hex(raw)
        store.write_blocks([(bid, raw)])
        big_blocks.append(bid)
    doc = jsonutil.object_manifest_doc(jsonutil.sha256_hex(data), len(data), 4 * 1024 * 1024, big_blocks, MEDIA)
    forged = ObjectManifest(
        doc["sha256"], doc["size"], doc["block_size"], tuple(big_blocks), MEDIA, jsonutil.object_id(doc)
    )
    store.add_manifest(forged)
    with pytest.raises(BadRequest) as exc:
        gitops.object_manifest(store, forged.object_id)
    assert exc.value.details["field"] == "manifest.block_size"
    with pytest.raises(BadRequest):
        b"".join(gitops.iter_object(store, forged.object_id))
    good = gitops.put_bytes(store, b"payload", MEDIA)
    wrong = "0" * 64
    store.add_manifest(ObjectManifest(good.sha256, good.size, good.block_size, good.blocks, MEDIA, wrong))
    with pytest.raises(BlockCorrupt):
        gitops.object_manifest(store, wrong)


# ---------------------------------------------------------------------------------------------------------
# W088 / W107 — GitProbe discovery with a ceiling, partial clones
# ---------------------------------------------------------------------------------------------------------


def test_probe_discovery_climbs_to_root_below_ceiling(git, user_home, tmp_path):
    """``toplevel``/``common_dir`` of a subdirectory name its work tree (``contracts.GitProbe``); with a ceiling a
    directory outside every repository below it has none, even when the ceiling lies in a checkout (W088)."""
    repo = RepoBuilder.init(user_home / "proj")
    repo.write("src/pkg/mod.py", "x = 1\n")
    repo.commit_all("c1")
    probe = gitops.GitProbe(git, ceiling=tmp_path)
    sub = repo.root / "src" / "pkg"
    assert probe.toplevel(str(sub)) == str(repo.root)
    assert probe.common_dir(str(sub)) == str(repo.root / ".git")
    assert probe.is_shallow(str(sub)) is False and probe.is_partial(str(sub)) is False
    plain = user_home / "notes" / "deep"
    plain.mkdir(parents=True)
    assert probe.toplevel(str(plain)) is None and probe.common_dir(str(plain)) is None
    assert probe.toplevel(str(repo.root / "src" / "pkg" / "mod.py")) is None
    received = gitops.GitProbe(git, profile="received", ceiling=tmp_path)
    assert received.toplevel(str(sub)) == str(repo.root)
    with pytest.raises(ValueError):
        gitops.GitProbe(git, ceiling="relative/dir")


def test_partial_clone_travels_as_file_tree(git, user_home, cfg, store, tmp_path):
    """A promisor remote marks a partial clone: ``is_partial`` (public, W107) sees it from any subdirectory and
    the pack entry refuses it like a shallow clone (``capability_missing: git.full_history``)."""
    repo = RepoBuilder.init(user_home / "partial")
    repo.write("a/b.txt", "x\n")
    repo.commit_all("c1")
    append_repo_config(repo.root, '[remote "origin"]\n\turl = https://example.invalid/r.git\n\tpromisor = true\n')
    probe = gitops.GitProbe(git, ceiling=tmp_path)
    assert probe.is_partial(str(repo.root / "a")) is True
    with pytest.raises(CapabilityMissing) as exc:
        _pack(git, _item(ItemKind.GIT_REPO, repo.root), store, cfg, tmp_path)
    assert exc.value.details["capability"] == "git.full_history"


# ---------------------------------------------------------------------------------------------------------
# Index copy: same-second, same-size edits after ``git add`` (all state computations)
# ---------------------------------------------------------------------------------------------------------


def test_worktree_state_sees_same_second_same_size_edit(git, user_home, tmp_path):
    """★ A file staged and then rewritten with the same size in the same second (git compares whole seconds) is
    "racily clean": only the index file's own mtime tells git to look at the content. The state computation copies
    the index with that mtime, so the work-tree tree carries the new content (mutant: the copy gets a fresh mtime —
    ``shutil.copyfile`` — and the tree silently keeps the staged blob)."""
    t = 1_700_000_000
    repo = RepoBuilder.init(user_home / "racy")
    repo.write("main.py", "print(1)\n")
    os.utime(repo.path("main.py"), (t, t))
    repo.commit_all("c1")
    repo.write("main.py", "print(2)\n")
    os.utime(repo.path("main.py"), (t, t))
    os.utime(repo.root / ".git" / "index", (t, t))
    expected = RepoBuilder.init(user_home / "expected")
    expected.write("main.py", "print(2)\n")
    expected.commit_all("c1")
    state = gitops.worktree_state(git, str(repo.root), (), tmp_path / "state")
    assert state.worktree_tree == expected.rev("HEAD^{tree}")
    assert state.index_tree == repo.rev("HEAD^{tree}")


# ---------------------------------------------------------------------------------------------------------
# W093 — stash blobs sniffed in memory, always
# ---------------------------------------------------------------------------------------------------------


def test_filter_stash_sniffs_blobs_without_scratch(git, user_home, cfg):
    """A stash entry whose untracked commit adds a private key under an innocent name is withheld also when the
    caller passes no scratch directory (``policy.sniff_bytes``; layer 1 sniffed only with a scratch file)."""
    repo = RepoBuilder.init(user_home / "stashy")
    repo.write("app.py", "print(1)\n")
    repo.commit_all("c1")
    repo.write("notes.txt", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n")
    repo.stash("wip with key", untracked=True)
    entries = [{"commit": c, "message": m} for c, m in repo.stash_list()]
    kept, withheld = gitpack.filter_stash(git, str(repo.root), entries, cfg)
    assert kept == [] and [n.why for n in withheld] == ["stash_secret"]
    assert not any((user_home / "stashy" / ".git").rglob("sniff.bin"))


# ---------------------------------------------------------------------------------------------------------
# X09 — MapRow.state, one producer, readers for every form
# ---------------------------------------------------------------------------------------------------------

SHA_A, SHA_B, SHA_C = "a" * 40, "b" * 40, "c" * 40


def test_map_state_forms_and_readers():
    """``map_state_of`` turns a ``GitState``, its wire form and the layer-1 nested form into the flat form and a
    fingerprint into ``{"tree": fp}``; it is idempotent; the readers take every form, bad input is refused by the
    producer and ignored by the readers."""
    gs = GitState(SHA_A, SHA_B, SHA_C, "refs/heads/main", (":(exclude,literal).env",))
    flat = {
        "head": SHA_A,
        "index_tree": SHA_B,
        "worktree_tree": SHA_C,
        "branch": "refs/heads/main",
        "exclude_pathspecs": [":(exclude,literal).env"],
    }
    assert materialize.map_state_of(gs) == flat
    assert materialize.map_state_of(jsonutil.to_wire(gs)) == flat
    nested = {"git": {**jsonutil.to_wire(gs), "branch": None}, "branch": "refs/heads/main"}
    assert materialize.map_state_of(nested) == flat
    assert materialize.map_state_of(flat) == flat
    assert materialize.map_state_of(gs, "refs/heads/other")["branch"] == "refs/heads/other"
    fp = {"v": 1, "files": {"a.txt": ["file", 1, 2, "d" * 64]}}
    assert materialize.map_state_of(fp) == {"tree": fp} == materialize.map_state_of({"tree": fp})
    assert materialize.map_state_of(None) is None
    for bad in ({"index_tree": "x", "worktree_tree": SHA_C}, {"foo": 1}, {**flat, "exclude_pathspecs": "x"}):
        with pytest.raises(BadRequest):
            materialize.map_state_of(bad)
        assert materialize.git_state_of(bad) is None
    stored = json.loads(json.dumps(flat))
    assert materialize.git_state_of(stored) == gs
    assert materialize.git_state_of(nested) == gs
    assert materialize.tree_fingerprint_of({"tree": fp}) == fp == materialize.tree_fingerprint_of(fp)
    assert materialize.tree_fingerprint_of(flat) is None and materialize.git_state_of({"tree": fp}) is None


def test_x09_return_trip_through_map_state(git, user_home, hermes_home, tmp_path, store, handover_root):
    """★ X09 — server → laptop → server through the one producer: the server's ``out`` row holds
    ``pack_plan_item(...).state`` (stored as JSON like a ledger row), the laptop's ``in`` row ``map_states()``;
    back on the server ``assess`` finds return mode from that row and the transaction lands the laptop's work
    exactly, the server-only secret untouched (mutant: ``map_state_of`` drops the recorded exclusions, so the
    server's own secret makes the repository look changed)."""
    cfg = make_cfg(user_home, hermes_home)
    srv = RepoBuilder.init(handover_root / "srv")
    srv.write("main.py", "print(1)\n")
    srv.write("lib/util.py", "x = 1\n")
    srv.commit_all("server c1")
    srv.write("main.py", "print(2)  # wip\n")
    srv.write("todo.txt", "uncommitted\n")
    srv.write(".env", "TOKEN=server-only\n")
    out = _pack(git, _item(ItemKind.GIT_REPO, srv.root), store, cfg, tmp_path)
    assert set(out.state) == {"head", "index_tree", "worktree_tree", "branch", "exclude_pathspecs"}
    assert out.doc["kind"] == "git_repo"
    out_row = MapRow(str(srv.root), "laptop", str(user_home / "laptop"), "out", state=out.state)
    out_row = jsonutil.from_wire(MapRow, json.loads(jsonutil.canonical_bytes(out_row)))

    lap_txn = materialize.WorkspaceTxn(
        "01JOBX09000000000000000001", tmp_path / "t1", git, store, RecordingJournal(), cfg=cfg
    )
    lap_item = _item(ItemKind.GIT_REPO, srv.root)
    lap_txn.prepare_item(lap_item, TargetDecision("w0", str(user_home / "laptop"), DestMode.FRESH, "user"), out.doc)
    for _, forward, _ in lap_txn.commit_steps():
        forward()
    in_state = lap_txn.map_states()["w0"]
    assert in_state == materialize.map_state_of(out.state) and in_state["branch"] == "refs/heads/main"
    laptop = RepoBuilder(user_home / "laptop")
    laptop.write("lib/util.py", "x = 2\n")
    laptop.commit_all("laptop c2")
    laptop.write("main.py", "print(3)\n")
    laptop.write("lib/new.py", "y = 1\n")
    (laptop.root / "todo.txt").unlink()

    back = _pack(
        git,
        _item(ItemKind.GIT_REPO, laptop.root),
        store,
        cfg,
        tmp_path / "back",
        basis_candidates=gitops.GitProbe(git).ref_tips(str(srv.root)),
    )
    srv_item = _item(ItemKind.GIT_REPO, laptop.root, git=gitops.GitProbe(git).info(str(laptop.root), cfg))
    a = materialize.assess(
        srv_item,
        str(srv.root),
        git=git,
        probe=gitops.GitProbe(git, profile="received"),
        out_row=out_row,
        tree_fingerprint=None,
        target_facts=env_facts(user_home, workspace_root=handover_root),
        fallback=lambda d: materialize.fallback_dest(d, AT),
        tmp=tmp_path / "assess",
        cfg=cfg,
        recorded=True,
        local_choice=False,
    )
    assert (a.mode, a.conflict) == (DestMode.RETURN, None)
    txn = materialize.WorkspaceTxn(
        "01JOBX09000000000000000002", tmp_path / "t2", git, store, RecordingJournal(), cfg=cfg
    )
    txn.prepare_item(srv_item, TargetDecision("w0", str(srv.root), a.mode, "return-mapping"), back.doc, out_row=out_row)
    for _, forward, _ in txn.commit_steps():
        forward()
    assert txn.dests()[0][1:] == (str(srv.root), DestMode.RETURN)
    assert_same_state(laptop, srv, excluded=[b".env"])
    assert (srv.root / ".env").read_text() == "TOKEN=server-only\n"
    assert out.state["exclude_pathspecs"] == list(out.doc["exclude_pathspecs"])


def test_tree_rows_accept_both_forms(git, user_home, tmp_path, cfg, store, handover_root):
    """A file-tree ``in`` row written as ``{"tree": fp}`` (X09) and one with the bare fingerprint (layer 1) both
    give update mode; ``map_states`` writes the X09 form."""
    root = user_home / "tree"
    root.mkdir()
    (root / "a.txt").write_text("a\n")
    packed = _pack(git, _item(ItemKind.FILE_TREE, root, "x1"), store, cfg, tmp_path)
    assert packed.state == {
        "tree": filetree.fingerprint(root, filetree.scan(root, IgnoreMatcher.for_tree(root, ()), cfg)[0])
    }
    txn = materialize.WorkspaceTxn(
        "01JOBX09000000000000000003", tmp_path / "t", git, store, RecordingJournal(), cfg=cfg
    )
    dest = handover_root / "tree"
    txn.prepare_item(
        _item(ItemKind.FILE_TREE, root, "x1"), TargetDecision("x1", str(dest), DestMode.FRESH, "user"), packed.doc
    )
    for _, forward, _ in txn.commit_steps():
        forward()
    state = txn.map_states()["x1"]
    assert set(state) == {"tree"}
    for form in (state, state["tree"]):
        a = materialize.assess(
            _item(ItemKind.FILE_TREE, root, "x1"),
            str(dest),
            git=git,
            probe=gitops.GitProbe(git, profile="received"),
            out_row=None,
            tree_fingerprint=json.loads(json.dumps(form)),
            target_facts=env_facts(user_home, workspace_root=handover_root),
            fallback=lambda d: materialize.fallback_dest(d, AT),
            tmp=tmp_path,
            cfg=cfg,
            recorded=True,
            local_choice=False,
        )
        assert (a.mode, a.conflict) == (DestMode.UPDATE, None)


# ---------------------------------------------------------------------------------------------------------
# X12 — one pack-item entry for every kind
# ---------------------------------------------------------------------------------------------------------


def test_pack_entry_git_names_and_lfs_key(git, user_home, cfg, store, tmp_path):
    """git items carry ``lfs_objects``, the case-colliding pairs and the names Windows cannot check out (target
    independent, JSON-safe); the work tree itself is unchanged."""
    repo = RepoBuilder.init(user_home / "names")
    repo.write("README.md", "upper\n")
    repo.write("readme.md", "lower\n")
    repo.write("aux.txt", "reserved\n")
    repo.write("docs/a:b.txt", "colon\n")
    repo.write("trail.", "dot\n")
    repo.write(b"bin\xff.dat", b"x")
    repo.write("ok.txt", "fine\n")
    repo.commit_all("c1")
    packed = _pack(git, _item(ItemKind.GIT_REPO, repo.root), store, cfg, tmp_path)
    doc = packed.doc
    assert doc["kind"] == "git_repo" and "lfs_objects" in doc and doc["item_id"] == "w0"
    assert doc["case_collisions"] == [["README.md", "readme.md"]]
    assert sorted(doc["ntfs_invalid"]) == sorted(
        ["aux.txt", "docs/a:b.txt", "trail.", gitops.enc_path(os.fsdecode(b"bin\xff.dat"))]
    )
    jsonutil.canonical_bytes(doc)
    assert packed.state == materialize.map_state_of(
        GitState(doc["head"]["commit"], doc["index_tree"], doc["worktree_tree"], "refs/heads/main", ())
    )


@pytest.mark.parametrize("kind", [ItemKind.FILE_TREE, ItemKind.EXTRA_DIR])
def test_pack_entry_tree_and_extra_dir(git, user_home, cfg, store, tmp_path, handover_root, kind):
    """A workspace tree and an extra directory are both one ``file_tree`` item; secrets are listed as excluded,
    names and the fingerprint state come along, and the item materialises byte-identical."""
    root = user_home / "proj-tree"
    for rel, text in {"a.txt": "a\n", "B/c.md": "c\n", "b/d.md": "d\n", "con.txt": "x\n", ".env": "K=1\n"}.items():
        (root / rel).parent.mkdir(parents=True, exist_ok=True)
        (root / rel).write_text(text)
    os.chmod(root / "a.txt", 0o755)  # noqa: S103 - executable test fixture
    packed = _pack(git, _item(kind, root, "x1"), store, cfg, tmp_path)
    doc = packed.doc
    assert doc["kind"] == "file_tree" and doc["entries"] == 6 and doc["bytes"] == 8
    assert [e["path"] for e in doc["excluded"]] == [".env"] and doc["skipped"] == []
    assert doc["case_collisions"] == [["B", "b"]] and doc["ntfs_invalid"] == ["con.txt"]
    assert set(packed.state) == {"tree"} and packed.excluded[0].why == "secret"
    txn = materialize.WorkspaceTxn(
        "01JOBX12000000000000000001", tmp_path / "t", git, store, RecordingJournal(), cfg=cfg
    )
    dest = handover_root / "tree"
    txn.prepare_item(_item(kind, root, "x1"), TargetDecision("x1", str(dest), DestMode.FRESH, "user"), doc)
    for _, forward, _ in txn.commit_steps():
        forward()
    want = {k: v for k, v in tree_digest(root).items() if k != b".env"}
    assert tree_digest(dest) == want


def test_pack_entry_single_files(git, user_home, hermes_home, cfg, store, tmp_path):
    """Extra files keep their mode as ``100644``/``100755`` and pass the policy again at pack time; session files
    carry ``hermes_kind`` and ``rel`` and are refused without a known kind, when protected or over the limit."""
    tool = user_home / "bin" / "tool.sh"
    tool.parent.mkdir()
    tool.write_text("#!/bin/sh\n")
    os.chmod(tool, 0o755)  # noqa: S103 - executable test fixture
    note = user_home / "note.txt"
    note.write_text("n\n")
    assert _pack(git, _item(ItemKind.EXTRA_FILE, tool, "e1"), store, cfg, tmp_path).doc["mode"] == "100755"
    plain = _pack(git, _item(ItemKind.EXTRA_FILE, note, "e2"), store, cfg, tmp_path)
    assert plain.doc["mode"] == "100644" and plain.doc["kind"] == "extra_file" and plain.state is None
    assert gitops.read_object(store, plain.doc["object"]) == b"n\n"
    key = user_home / "deploy.txt"
    key.write_text("-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n")
    refused = _pack(git, _item(ItemKind.EXTRA_FILE, key, "e3"), store, cfg, tmp_path)
    assert refused.doc == {"item_id": "e3", "kind": "extra_file", "object": None, "skipped": "secret"}
    link = user_home / "link.txt"
    link.symlink_to(note)
    assert _pack(git, _item(ItemKind.EXTRA_FILE, link, "e4"), store, cfg, tmp_path).doc["skipped"] == "symlink"

    spill = hermes_home / "cache" / "spillover" / "call_1.txt"
    spill.parent.mkdir(parents=True)
    spill.write_text("tool output\n")
    h = _pack(
        git,
        _item(ItemKind.HERMES_FILE, spill, "h0", hermes_kind="spillover"),
        store,
        cfg,
        tmp_path,
        hermes_home=hermes_home,
    )
    assert h.doc["kind"] == "hermes_file" and h.doc["hermes_kind"] == "spillover"
    assert h.doc["rel"] == "cache/spillover/call_1.txt" and h.doc["mode"] == "100644"
    no_kind = _pack(git, _item(ItemKind.HERMES_FILE, spill, "h1"), store, cfg, tmp_path)
    assert no_kind.doc["object"] is None and no_kind.doc["skipped"] == "hermes"
    config = hermes_home / "config.yaml"
    config.write_text("model: x\n")
    protected = _pack(git, _item(ItemKind.HERMES_FILE, config, "h2", hermes_kind="spillover"), store, cfg, tmp_path)
    assert protected.doc["skipped"] == "hermes"
    small = make_cfg(user_home, hermes_home, max_hermes_file_bytes=4)
    big = _pack(git, _item(ItemKind.HERMES_FILE, spill, "h3", hermes_kind="spillover"), store, small, tmp_path)
    assert big.doc["skipped"] == "size"
