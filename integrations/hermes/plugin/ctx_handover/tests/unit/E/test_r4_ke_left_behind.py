"""Review finding R3-L2 (the third review §3; its round-3 report „Neue Lücken“ 2): a file
of a tree item that the policy refuses as ``hermes`` (a hard link to a Hermes credential store, L9) went into the pack
item's ``skipped`` list. The conductor reports only ``excluded`` as left behind (``conductor._left_behind``: final
message, note, status) — so the user never learnt that a workspace file is missing. ``hermes`` now counts among the
reasons a tree entry is listed as ``excluded`` (``materialize._LEFT_BEHIND``), like ``secret`` and ``symlink``; so does
the ``secret`` refusal of a linked file the bounded identity walk could not verify (R3-M2).

The production source path: ``materialize.pack_plan_item`` → ``filetree.scan`` → ``policy.check_source_file``."""

from __future__ import annotations

import os
from pathlib import Path

from ctx_handover.engine import materialize, policy
from ctx_handover.engine.model import Include, ItemKind, ItemRole, Origin, PlanItem


def _tree_item(root: Path) -> PlanItem:
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


def _pack(root: Path, git, store, cfg, tmp_path) -> materialize.PackedItem:
    return materialize.pack_plan_item(
        _tree_item(root), git=git, store=store, cfg=cfg, workdir=tmp_path / "w", created_at=1.0
    )


def test_r4_ke_l2_hermes_hardlink_is_listed_as_left_behind(git, user_home, hermes_home, tmp_path, cfg, store):
    """★ ``proj/env.txt`` is a hard link to the active Hermes home's ``.env``: refused (``hermes``, R19) and listed in
    the pack item's ``excluded`` — what the final message and the note name. Before the fix: only in ``skipped``."""
    env = hermes_home / ".env"
    env.write_text("OPENAI_API_KEY=sk-test\n", encoding="utf-8")
    root = user_home / "proj"
    root.mkdir()
    (root / "main.py").write_text("print(1)\n", encoding="utf-8")
    os.link(env, root / "env.txt")
    packed = _pack(root, git, store, cfg, tmp_path)
    assert ("env.txt", "hermes") in {(e["path"], e["why"]) for e in packed.doc["excluded"]}
    assert ("env.txt", "hermes") in {(n.path, n.why) for n in packed.excluded}
    assert "env.txt" not in {e["path"] for e in packed.doc["skipped"]}
    assert packed.doc["entries"] >= 1


def test_r4_ke_l2_unverified_hardlink_is_listed_as_left_behind(
    git, user_home, hermes_home, tmp_path, cfg, store, monkeypatch
):
    """The fail-closed refusal of R3-M2 reaches the report too: the identity walk ran out (bound 300, a cache of 400
    entries in ``~/.kube``), so a linked file whose other name is unknown is ``secret`` and listed in ``excluded``."""
    cache = user_home / ".kube" / "cache"
    cache.mkdir(parents=True)
    for i in range(400):
        (cache / f"r{i:03d}.json").write_bytes(b"{}")
    outside = user_home / "data" / "orig.txt"
    outside.parent.mkdir()
    outside.write_text("shared\n", encoding="utf-8")
    root = user_home / "proj"
    root.mkdir()
    os.link(outside, root / "copy.txt")
    monkeypatch.setattr(policy, "LINKED_SCAN_MAX", 300)
    packed = _pack(root, git, store, cfg, tmp_path)
    assert ("copy.txt", "secret") in {(e["path"], e["why"]) for e in packed.doc["excluded"]}
