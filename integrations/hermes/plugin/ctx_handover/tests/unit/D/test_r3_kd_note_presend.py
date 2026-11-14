"""No silent omission (R30 form) for two more cases — hook outputs on the target (M10) and a refused
``--include`` on the sender (L3-F24).

* **M10** (the second review §4): hook outputs travel, but the target places them under the
  *new* session's folder (``hook_outputs/<new id>/<name>``, contract X17: kind and basename only) while the
  conversation names ``hook_outputs/<old segment id>/<name>``; the path map only says ``HERMES_HOME → HERMES_HOME``. The
  agent on the target followed a path into nothing, and nothing said so. The note now names, for every old folder,
  where its hook outputs are here (same file names — Hermes names them by uuid,
  ``tools/hook_output_spill.py:97``): the exact folder when the target's path map has an entry of kind
  ``hermes_file`` for it (``hermes_files.hook_outputs_dir`` of the new session), else this session's folder below the
  target's ``hook_outputs``.
* **L3-F24** (lab): a folder the user named with ``--include`` outside ``$HOME``/``export_roots`` was refused by the
  policy (``outside home``) and appeared only in ``status`` and in the note on the target — the sender's first message
  said nothing. ``plan.presend_summary(..., include=…)`` now returns ``include_refused`` (path and why of every
  ``--include``/``--include-secret`` path the plan did not take), for the surfaces to show before anything is locked.
"""

from __future__ import annotations

import os
from pathlib import Path

from ctx_handover.engine import hermes_files, note, plan
from ctx_handover.engine.model import (
    HermesPaths,
    Include,
    ItemKind,
    ItemRole,
    Options,
    Origin,
    PathMapEntry,
    PlanHints,
    RefineReport,
)
from ctx_handover.tests.fakes.planning_fake import (
    Busy,
    StaticGitProbe,
    decision,
    env_facts,
    fake_redact,
    final_plan,
    plan_item,
    static_discover,
)
from ctx_handover.tests.unit.D.conftest import write

NOW = 1_762_697_000.0
SRC_HOME = "/root/.hermes"
TGT_HOME = "/Users/u/.hermes"


def _hook(item_id: str, sid: str, name: str):
    item = plan_item(
        item_id,
        f"{SRC_HOME}/hook_outputs/{sid}/{name}",
        kind=ItemKind.HERMES_FILE,
        role=ItemRole.EXTRA,
        origin=Origin.TRANSCRIPT,
        hermes_kind="hook_outputs",
    )
    return item, decision(item, TGT_HOME)


def _compose(items, extra_map=()):
    workspace = plan_item("w0", "/srv/app", kind=ItemKind.FILE_TREE)
    return note.compose(
        job_id="01JC5ABCDEFGHJKMNPQRSTVWXY",
        source=env_facts("server"),
        target=env_facts("laptop", home="/Users/u"),
        delta={},
        path_map=(
            PathMapEntry("/srv/app", "/Users/u/app", "workspace"),
            PathMapEntry(SRC_HOME, TGT_HOME, "hermes_home"),
            *extra_map,
        ),
        items=[(workspace, decision(workspace, "/Users/u/app")), *items],
        note_parts={},
        model_change=None,
        pack_time=0,
        delta_message=False,
        report_path="/Users/u/.hermes/plugin-data/ctx_handover/reports/01JC5.md",
        redact=fake_redact,
        untrusted_text="",
        origin="verified peer server",
        now=NOW,
        strict=True,
    )


# ---------------------------------------------------------------------------------------------------------
# M10: hook outputs
# ---------------------------------------------------------------------------------------------------------


def test_m10_note_says_where_the_hook_outputs_of_each_old_folder_are():
    """★ Two segments with hook outputs: the note names each old folder and the place here, with the same file names.
    Before the fix the note had only ``/root/.hermes → /Users/u/.hermes`` — the old folder does not exist here."""
    items = [_hook("h0", "20261108_aaaa", "1f2e.txt"), _hook("h1", "20261109_bbbb", "9c8d.txt")]
    text, report = _compose(items)
    for sid in ("20261108_aaaa", "20261109_bbbb"):
        line = next((ln for ln in text.splitlines() if f"{SRC_HOME}/hook_outputs/{sid}" in ln), None)
        assert line is not None, text
        assert f"{TGT_HOME}/hook_outputs/" in line
        assert "same file names" in line
        assert f"{SRC_HOME}/hook_outputs/{sid}" in report


def test_m10_note_names_the_exact_folder_when_the_target_maps_it():
    """★ With a path-map entry of kind ``hermes_file`` (old folder → ``hermes_files.hook_outputs_dir`` of the new
    session) the line names that folder. Before the fix the entry was not rendered at all."""
    here = f"{TGT_HOME}/hook_outputs/20261109_newtip"
    entry = PathMapEntry(f"{SRC_HOME}/hook_outputs/20261108_aaaa", here, "hermes_file")
    text, _report = _compose([_hook("h0", "20261108_aaaa", "1f2e.txt")], extra_map=(entry,))
    line = next(ln for ln in text.splitlines() if f"{SRC_HOME}/hook_outputs/20261108_aaaa" in ln)
    assert f"→ {here}" in line, line


def test_m10_no_hook_outputs_no_line():
    text, _report = _compose([])
    assert "hook_outputs" not in text


def test_m10_skipped_hook_output_is_not_promised():
    """A hook output the target skips (mode ``skip``) is not announced as being here."""
    item, dec = _hook("h0", "20261108_aaaa", "1f2e.txt")
    from dataclasses import replace

    from ctx_handover.engine.model import DestMode

    text, _report = _compose([(item, replace(dec, mode=DestMode.SKIP))])
    assert f"{SRC_HOME}/hook_outputs/20261108_aaaa" not in text


def test_m10_hook_outputs_dir_matches_the_placement():
    """``hermes_files.hook_outputs_dir`` is the folder ``target_path`` places hook outputs in (one rule)."""
    target = HermesPaths(home=TGT_HOME, document_cache=f"{TGT_HOME}/cache/documents")
    placed = hermes_files.target_path("hook_outputs", "x.txt", "0" * 64, target, "20261109_new/../tip")
    assert placed.parent == hermes_files.hook_outputs_dir(target, "20261109_new/../tip")


# ---------------------------------------------------------------------------------------------------------
# L3-F24: --include the policy refused
# ---------------------------------------------------------------------------------------------------------


def _plan_with(lay, include: tuple[str, ...], tmp_root: Path):
    cands = []
    rp = plan.build_plan(
        "01JCJOB",
        (1,),
        cands,
        None,
        RefineReport(used="off"),
        PlanHints(include=include),
        lay.cfg(),
        lambda root: None,
        Busy(),
        (),
        Options(),
        1000,
        redact=fake_redact,
    )
    return final_plan(rp)


def test_f24_presend_names_a_refused_include(lay, tmp_path):
    """★ The lab case: ``--include <dir outside $HOME>``, ``export_roots`` empty. The plan refuses it
    (``outside home``); the summary before sending now says so. Before the fix ``presend_summary`` had no such key
    (and no ``include`` keyword)."""
    outside = tmp_path / "elsewhere" / "tree"
    write(outside / "data.txt", "x\n")
    assert not str(outside).startswith(str(lay.home))
    final = _plan_with(lay, (str(outside),), tmp_path)
    summary = plan.presend_summary(final, include=(str(outside),))
    assert summary["include_refused"] == [{"path": os.path.normpath(outside), "why": plan.WHY_OUTSIDE_HOME}]
    assert summary["include_refused_more"] == 0
    assert "include_refused" in plan.PRESEND_KEYS


def test_f24_missing_and_relative_includes_are_named(lay, tmp_path):
    """A path that does not exist and one that is not absolute (the plan skips it without a refusal row) are named
    too."""
    missing = lay.home / "gone"
    final = _plan_with(lay, (str(missing), "rel/dir"), tmp_path)
    refused = plan.presend_summary(final, include=(str(missing), "rel/dir"))["include_refused"]
    assert {"path": str(missing), "why": "missing"} in refused
    assert {"path": "rel/dir", "why": plan.WHY_NOT_ABSOLUTE} in refused


def test_f24_taken_includes_are_not_named(lay, tmp_path):
    """An include that became an item is no refusal; without ``include`` the key is an empty list."""
    notes = lay.notes / "todo.md"
    final = _plan_with(lay, (str(notes),), tmp_path)
    assert [i.source_path for i in final.plan.items if i.include is Include.YES] == [os.path.realpath(notes)]
    assert plan.presend_summary(final, include=(str(notes),))["include_refused"] == []
    assert plan.presend_summary(final)["include_refused"] == []


def test_f24_include_inside_the_workspace_is_covered_not_refused(transcript, lay, tmp_path):
    """An include inside the workspace is covered by it (no item of its own) — not a refusal."""
    t = transcript(lay.project)
    from ctx_handover.engine import collector

    cands = collector.collect(
        t.rows(),
        t.session_row(),
        HermesPaths(home=str(lay.hermes_home), document_cache=str(lay.hermes_home / "cache" / "documents")),
        lay.cfg(),
        StaticGitProbe(),
        source_os="linux",
        discover=static_discover([]),
    )
    inner = lay.project / "src" / "main.py"
    rp = plan.build_plan(
        "01JCJOB",
        (1,),
        cands,
        None,
        RefineReport(used="off"),
        PlanHints(include=(str(inner),)),
        lay.cfg(),
        lambda root: None,
        Busy(),
        (),
        Options(),
        1000,
        redact=fake_redact,
    )
    assert plan.presend_summary(final_plan(rp), include=(str(inner),))["include_refused"] == []
