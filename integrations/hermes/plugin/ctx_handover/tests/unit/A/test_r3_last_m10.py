"""M10 on the target: the note names the exact folder the hook outputs of each
old folder are in here.

Hook outputs travel, but ``LocalTarget`` places them by kind and basename only (contract X17) in the arrived session's
own folder, ``hermes_files.hook_outputs_dir`` (``<HERMES_HOME>/hook_outputs/<new session id>``), while the conversation
names ``hook_outputs/<old segment id>/<name>`` of the previous machine. D's note renders one line per old folder; it
names the exact folder only when the target's path map carries an entry of kind ``hermes_file`` for that folder, and
otherwise falls back to ``<home>/hook_outputs/<this session's id>`` — a placeholder the agent cannot open. Before this
package ``LocalTarget`` added no such entry, so every real note carried the placeholder.
"""

from __future__ import annotations

from pathlib import Path

from ctx_handover.engine import hermes_files, local_target
from ctx_handover.engine.jsonutil import from_wire
from ctx_handover.engine.model import FinalPlan, ItemKind, Mode, PathMapEntry, SessionRef
from ctx_handover.engine.policy import HERMES_FILE_KINDS
from ctx_handover.tests.unit.A.test_l2_target_import import (
    IMPORT_OPTIONS,
    _arrived,
    _target_ctx,
    errors,
    make_bundle,
    two_homes,  # noqa: F401  (fixture)
)
from ctx_handover.tests.unit.A.test_l2_target_wiring import item, real_target, server  # noqa: F401  (fixture)

HOOK = b"hook output of the last turn\n"
PLACEHOLDER = "<this session's id>"


def _folder(path: str) -> str:
    cut = max(path.rfind("/"), path.rfind("\\"))
    return path[:cut]


def test_m10_import_note_names_the_folder_the_hook_outputs_are_in(two_homes):  # noqa: F811
    """★ Export → import through the real ``LocalTarget``: the path map holds ``<old folder> → <HERMES_HOME here>/
    hook_outputs/<arrived session id>`` (kind ``hermes_file``), the note's hook line names that folder — and the file
    is there under its old name. Mutant (red proof): no ``hermes_file`` entry (the state before the fix) — the note
    names ``hook_outputs/<this session's id>``."""
    laptop, server_home = two_homes
    _sid, _root, bundle, _digest = make_bundle(laptop, hook_output=HOOK)
    job_id, rc = server_home.run(Mode.IMPORT, None, SessionRef(id=""), options=IMPORT_OPTIONS, bundle_path=bundle)
    assert rc == 0, errors(server_home, job_id)
    tip = _arrived(server_home, job_id)
    ctx = _target_ctx(server_home, job_id)
    final = from_wire(FinalPlan, ctx["final_plan"])
    hooks = [i for i in final.plan.items if i.kind is ItemKind.HERMES_FILE and i.hermes_kind == "hook_outputs"]
    assert len(hooks) == 1, final.plan.items
    old = _folder(hooks[0].source_path)
    here = server_home.hermes_home / "hook_outputs" / tip
    assert (here / "post-turn.txt").read_bytes() == HOOK  # the folder the note names is where the file landed
    entries = [from_wire(PathMapEntry, e) for e in ctx["path_map"]]
    assert PathMapEntry(source=old, target=str(here), kind="hermes_file") in entries, entries
    line = next((ln for ln in ctx["note"].splitlines() if ln.startswith(f"- {old} → ")), None)
    assert line == note_line(old, str(here)), ctx["note"]
    assert PLACEHOLDER not in ctx["note"]
    report = Path(ctx["report_path"]).read_text(encoding="utf-8")
    assert note_line(old, str(here)) in report and PLACEHOLDER not in report


def note_line(old: str, here: str) -> str:
    from ctx_handover.engine import note

    return note.HOOK_LINE.format(old=old, here=here)


def test_m10_one_entry_per_old_folder_of_placed_hook_outputs(server):  # noqa: F811
    """★ ``LocalTarget._hook_folders`` (the unit the prepare step calls after ``_hermes_placement``): one entry per old
    folder — split at either separator, as ``note`` splits it, so a Windows source maps too — only for placed files
    of kind ``hook_outputs``, all to the folder they were placed in — ``hermes_files.hook_outputs_dir`` of the arrived
    session (taken from the placement, so the stand-in ports need no new function). Mutants (red proofs):
    every placed session file mapped (spillover too); the folder split at ``/`` only."""
    target = real_target(server)
    store = target.store
    data = {k: store.put_bytes(k.encode() * 3, "application/octet-stream") for k in "abcde"}
    win = r"C:\Users\u\AppData\Local\hermes\hook_outputs\20261108_win"
    items = {
        "h1": item(
            "h1", ItemKind.HERMES_FILE, "/root/.hermes/hook_outputs/20261108_aaaa/1f2e.txt", hermes_kind="hook_outputs"
        ),
        "h2": item(
            "h2", ItemKind.HERMES_FILE, "/root/.hermes/hook_outputs/20261108_aaaa/9c8d.txt", hermes_kind="hook_outputs"
        ),
        "h3": item("h3", ItemKind.HERMES_FILE, win + r"\77aa.txt", hermes_kind="hook_outputs"),
        "h4": item("h4", ItemKind.HERMES_FILE, "/root/.hermes/cache/spillover/sp.txt", hermes_kind="spillover"),
        "h5": item(
            "h5", ItemKind.HERMES_FILE, "/root/.hermes/hook_outputs/20261109_bbbb/x.txt", hermes_kind="hook_outputs"
        ),
    }
    pack = [
        {"item_id": "h1", "object": data["a"].object_id, "hermes_kind": "hook_outputs", "rel": "1f2e.txt"},
        {"item_id": "h2", "object": data["b"].object_id, "hermes_kind": "hook_outputs", "rel": "9c8d.txt"},
        {"item_id": "h3", "object": data["c"].object_id, "hermes_kind": "hook_outputs", "rel": "77aa.txt"},
        {"item_id": "h4", "object": data["d"].object_id, "hermes_kind": "spillover", "rel": "sp.txt"},
        {"item_id": "h5", "object": data["e"].object_id, "hermes_kind": "spillover", "rel": "x.txt"},
    ]
    tip = "20261109_000000_abcdef"
    with server.active():
        placed, skipped = target._hermes_placement(pack, items, tip)
        assert len(placed) == 4 and skipped == [
            "the session file x.txt was not placed: its kind is unknown"
        ]  # h5: kinds differ
        entries = target._hook_folders(placed, items)  # the unit under test
    here = str(hermes_files.hook_outputs_dir(target.hermes, tip))
    assert entries == [
        PathMapEntry(source="/root/.hermes/hook_outputs/20261108_aaaa", target=here, kind="hermes_file"),
        PathMapEntry(source=win, target=here, kind="hermes_file"),
    ]
    by_id = {e["pack_item"]["item_id"]: e["target"] for e in placed}
    assert {_folder(by_id[i]) for i in ("h1", "h2", "h3")} == {here}  # the folder named is where they are placed


def test_m10_hook_kind_is_the_session_file_kind():
    assert local_target.HOOK_KIND == hermes_files.HOOK_OUTPUTS and local_target.HOOK_KIND in HERMES_FILE_KINDS
