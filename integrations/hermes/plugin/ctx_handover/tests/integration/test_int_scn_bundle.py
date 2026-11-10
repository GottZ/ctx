"""Mandatory scenario 3 (layer 2, DESIGN §5.4, O3, contract X23, V24): export into a bundle file at the default path
through the CLI surface (``hermes handover export``), import in a second home (``hermes handover import``); and
``--keep``, which exports a copy and leaves the source active, with the import note saying so.

Visible results: the bundle file lies at ``<workspace_root>/<title-slug>-<YYYYmmdd-HHMMSS>.ctxh.tar`` and the
export message names that full path; the imported session has the source's model history and the workspace
bytes; without ``--keep`` the source carries the primary lock (``exported``), with ``--keep`` it is untouched and
the note of the imported session says the session stays active on the source."""

from __future__ import annotations

import re
from pathlib import Path

from ctx_handover.tests.integration._int3_helpers import (
    arrival,
    bounded,
    repo_view,
    session_in,
    surface,
    wip_repo,
)
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks

BUNDLE_NAME = re.compile(r"^bundle-me-\d{8}-\d{6}\.ctxh\.tar$")


def _export(home, sid: str, *flags: str):
    call, _ = surface(home, " ".join(["export", "--session", sid, "--yes", "--json", *flags]))
    return call


def _bundles(home) -> list[Path]:
    root = home.home / "hermes-handover"
    return sorted(p for p in root.iterdir() if p.name.endswith(".ctxh.tar")) if root.is_dir() else []


def _imported(home, call) -> tuple[str, dict]:
    job_id = call.data.get("job_id")
    row = arrival(home, job_id) if job_id else None
    assert row is not None, ("no arrival row after the import", call.exit_code, call.out)
    return row["session_id"], row


def test_int_scn_export_default_path_then_import_in_second_home(online):
    """Scenario 3a: ``hermes handover export --session <id> --yes`` without a path writes one ``.ctxh.tar`` at the
    default path and locks the source; ``hermes handover import <file> --yes`` on the server continues it there."""
    server, laptop, _listener, _peer = online
    bounded(laptop)
    bounded(server)
    repo = wip_repo(laptop.home / "proj")
    view = repo_view(repo.root)
    sid = session_in(laptop, repo.root, "Bundle me")

    call = _export(laptop, sid)
    assert call.exit_code == 0, ("export failed", call.out)
    bundles = _bundles(laptop)
    assert len(bundles) == 1 and BUNDLE_NAME.match(bundles[0].name), [p.name for p in bundles]
    assert str(bundles[0]) in "\n".join(call.out), ("the export message does not name the full path", call.out)
    marks = session_marks(laptop.db(), sid)
    assert marks["ended"] and marks["archived"] and marks["hidden"], marks  # export locks the source (O3)

    imp, _ = surface(server, f"import {bundles[0]} --yes --json")
    assert imp.exit_code == 0, ("import failed", imp.out)
    tip, row = _imported(server, imp)
    assert lineage_hashes(server.db(), tip) == lineage_hashes(laptop.db(), sid)
    assert not session_marks(server.db(), tip)["hidden"]
    got = repo_view(server.home / "hermes-handover" / "proj")
    assert got == view, "workspace at the import target differs from the exported one"
    assert "imported elsewhere" in (row.get("note") or ""), "DESIGN §5.4: every bundle import note says so"


def test_int_scn_export_keep_is_a_copy(online):
    """Scenario 3b: ``--keep`` exports a copy (O3): the source stays active and keeps its title; the imported
    session's note says that the session stays active on the source."""
    server, laptop, _listener, _peer = online
    bounded(laptop)
    bounded(server)
    work = laptop.home / "notes-only"
    work.mkdir()
    (work / "plan.md").write_text("# plan\n", encoding="utf-8")
    sid = session_in(laptop, work, "Keep me")

    call = _export(laptop, sid, "--keep")
    assert call.exit_code == 0, ("export --keep failed", call.out)
    marks = session_marks(laptop.db(), sid)
    assert not (marks["ended"] or marks["archived"] or marks["hidden"]), marks
    assert marks["title"] == "Keep me"
    bundles = _bundles(laptop)
    assert len(bundles) == 1, [p.name for p in bundles]

    imp, _ = surface(server, f"import {bundles[0]} --yes --json")
    assert imp.exit_code == 0, ("import failed", imp.out)
    tip, row = _imported(server, imp)
    assert lineage_hashes(server.db(), tip) == lineage_hashes(laptop.db(), sid)
    note = row.get("note") or ""
    assert "stays active" in note and "copy" in note, note
