"""D: deterministic collector (DESIGN §8.2, I1, SEC 5.3, BX M13) — real SessionDB transcripts, real files.

Transcripts are written through the core's own paths (``SessionBuilder`` → ``append_message``) and read back with
``export_session_lineage(include_inactive=True)``, the shape the source's export reads. Repositories are plain
``git init`` (no commits; LAYER1-COMMON git rule), probed with a ceiling directory so the probe never escapes
into the checkout that hosts pytest's basetemp.
"""

from __future__ import annotations

import os
import stat as stat_mod
from pathlib import Path

from ctx_handover.engine import collector, plan
from ctx_handover.engine.model import (
    GitInfo,
    HermesPaths,
    Include,
    ItemKind,
    Options,
    Origin,
    PlanHints,
    RefineReport,
    Weight,
)
from ctx_handover.tests.fakes.planning_fake import (
    Busy,
    CeilingGitProbe,
    HermesFileStub,
    StaticGitProbe,
    fake_redact,
    git_init,
    static_discover,
)
from ctx_handover.tests.unit.D.conftest import write


def _hermes(lay) -> HermesPaths:
    return HermesPaths(home=str(lay.hermes_home), document_cache=str(lay.hermes_home / "cache" / "documents"))


def _collect(t, lay, git=None, **kw):
    kw.setdefault("discover", static_discover([]))
    return collector.collect(
        t.rows(),
        t.session_row(),
        _hermes(lay),
        lay.cfg(),
        git or CeilingGitProbe(lay.home.parent),
        source_os="linux",
        **kw,
    )


def _by_path(cands):
    return {c.path: c for c in cands}


def _git_info(root: str) -> GitInfo:
    return GitInfo(
        head=None,
        branch="main",
        object_format="sha1",
        shallow=False,
        dirty=False,
        in_progress=None,
        lfs=False,
        submodules=(),
        remotes=(),
        root_commits=(),
        untracked_bytes=0,
        untracked_count=0,
        denied_tracked=(),
    )


def _plan(cands, lay, **kw):
    options = kw.pop("options", Options())
    return plan.build_plan(
        "01JCJOB",
        (1,),
        cands,
        None,
        RefineReport(used="off"),
        PlanHints(),
        kw.pop("cfg", lay.cfg(options=options)),
        _git_info,
        Busy(),
        (),
        options,
        1000,
        redact=fake_redact,
        **kw,
    )


def test_collector_primary_workspace_git_and_tree(transcript, lay):
    repo = git_init(lay.project)
    sub = repo / "services" / "api"
    sub.mkdir(parents=True)
    t = transcript(sub)
    cands = _collect(t, lay)
    primary = cands[0]
    assert primary.kind is ItemKind.GIT_REPO
    assert primary.origin is Origin.CWD
    assert primary.path == os.path.realpath(repo)
    assert "cwd: services/api" in primary.reasons

    plain = lay.home / "plain"
    plain.mkdir()
    t2 = transcript(plain)
    cands2 = _collect(t2, lay)
    assert cands2[0].kind is ItemKind.FILE_TREE
    assert cands2[0].path == os.path.realpath(plain)
    assert cands2[0].origin is Origin.CWD


def test_collector_tracks_terminal_cwd(transcript, lay):
    other = lay.home / "other"
    write(other / "notes.txt")
    write(lay.project / "notes.txt")
    t = transcript(lay.project)
    t.tool("read_file", {"path": "notes.txt"}, {"content": "x"})
    t.terminal("cd ../other", cwd=str(other))
    t.tool("read_file", {"path": "notes.txt"}, {"content": "y"})
    cands = _by_path(_collect(t, lay))
    extra = cands[str(other / "notes.txt")]
    assert extra.kind is ItemKind.EXTRA_FILE
    assert extra.weight is Weight.READ
    assert extra.origin is Origin.TRANSCRIPT
    # the first read stayed inside the workspace: covered, recorded on the primary
    primary = _collect(t, lay)[0]
    assert any(r.startswith("read: notes.txt") for r in primary.reasons)


def test_collector_deterministic(transcript, lay):
    write(lay.notes / "b.md")
    t = transcript(lay.project)
    t.tool("write_file", {"path": str(lay.notes / "b.md"), "content": "x"}, {"resolved_path": str(lay.notes / "b.md")})
    t.tool("read_file", {"path": str(lay.notes / "todo.md")})
    t.terminal(f"ls {lay.notes}")
    first = _collect(t, lay)
    second = _collect(t, lay)
    assert first == second
    assert first[1:] == sorted(first[1:], key=collector.sort_key)
    p1 = plan.with_plan_id(_final(_plan(first, lay)))
    p2 = plan.with_plan_id(_final(_plan(second, lay)))
    assert p1.plan_id == p2.plan_id


def _final(resource_plan):
    from ctx_handover.engine.model import FinalPlan

    return FinalPlan(plan_id="", plan=resource_plan, decisions=(), need_bytes=0, free_bytes={}, model_change=None)


def test_collector_extra_paths_and_env_references(transcript, lay):
    docs = lay.home / "docs"
    write(docs / "readme.txt")
    t = transcript(lay.project)
    t.tool(
        "write_file",
        {"path": str(lay.notes / "todo.md"), "content": "x"},
        {"resolved_path": str(lay.notes / "todo.md"), "bytes_written": 1},
    )
    t.tool("read_file", {"path": "/etc/hostname"}, {"content": "h"})
    t.terminal(f"/usr/bin/env python3 {lay.project}/src/main.py > /dev/null; ls ~/docs")
    cands = _by_path(_collect(t, lay))
    todo = cands[str(lay.notes / "todo.md")]
    assert (todo.kind, todo.weight) == (ItemKind.EXTRA_FILE, Weight.WRITE)
    assert str(docs) in cands and cands[str(docs)].kind is ItemKind.EXTRA_DIR
    assert cands[str(docs)].weight is Weight.MENTION
    assert "/usr/bin/env" not in cands and "/dev/null" not in cands
    if os.path.exists("/etc/hostname"):
        assert cands["/etc/hostname"].weight is Weight.READ
        resource_plan = _plan(list(cands.values()), lay)
        assert ("/etc/hostname", "outside home") in {(n.path, n.why) for n in resource_plan.not_transferred}
        assert "/etc/hostname" not in {i.source_path for i in resource_plan.items}


def test_collector_msys_paths_on_windows_source(lay):
    """A Windows source writes MSYS forms into terminal commands; they are classified as ``C:\\…`` paths."""
    files = {
        "C:\\Users\\x\\proj": stat_mod.S_IFDIR | 0o755,
        "C:\\Users\\x\\notes\\f.txt": stat_mod.S_IFREG | 0o644,
        "C:\\Users\\x\\notes\\g.txt": stat_mod.S_IFREG | 0o644,
        "C:\\Users\\x\\notes\\h.txt": stat_mod.S_IFREG | 0o644,
    }

    def lstat(p):
        if p not in files:
            raise FileNotFoundError(p)
        return os.stat_result((files[p], 0, 0, 1, 0, 0, 10, 0, 0, 0))

    fs = collector.FsOps(lstat=lstat, realpath=lambda p: p)
    rows = [
        _call_row("1", "read_file", {"path": "/c/Users/x/notes/f.txt"}),
        _call_row("2", "terminal", {"command": "cat /cygdrive/c/Users/x/notes/g.txt /mnt/c/Users/x/notes/h.txt"}),
    ]
    cfg = lay.cfg(home=Path("C:\\Users\\x"), case_insensitive=True, target_os="windows")
    cands = collector.collect(
        rows,
        {"id": "s1", "cwd": "C:\\Users\\x\\proj"},
        HermesPaths(home="C:\\Users\\x\\.hermes", document_cache="C:\\Users\\x\\.hermes\\cache\\documents"),
        cfg,
        StaticGitProbe(),
        source_os="windows",
        discover=static_discover([]),
        fs=fs,
    )
    paths = {c.path: c for c in cands}
    assert paths["C:\\Users\\x\\proj"].kind is ItemKind.FILE_TREE
    assert paths["C:\\Users\\x\\notes\\f.txt"].weight is Weight.READ
    assert paths["C:\\Users\\x\\notes\\g.txt"].weight is Weight.MENTION
    assert "C:\\Users\\x\\notes\\h.txt" in paths


def _call_row(n: str, name: str, args: dict, session_id: str = "s1") -> dict:
    import json

    return {
        "session_id": session_id,
        "role": "assistant",
        "content": "",
        "tool_calls": [{"id": f"c{n}", "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}],
    }


def test_collector_classifies_after_realpath(transcript, lay):
    """SEC 5.3 / BUILD-ORDER §6 #17: evidence through a link into ``~/.aws`` is classified by its realpath and
    denied; a link inside the workspace pointing outside is not "covered"."""
    aws = lay.home / ".aws"
    write(aws / "credentials", "[default]\naws_access_key_id = AKIAEXAMPLE\n")
    (lay.home / "cloud").symlink_to(aws)
    elsewhere = lay.home / "elsewhere"
    write(elsewhere / "a.txt")
    (lay.project / "ext").symlink_to(elsewhere)
    t = transcript(lay.project)
    t.tool("read_file", {"path": str(lay.home / "cloud" / "credentials")}, {"content": "…"})
    t.tool("write_file", {"path": "ext/a.txt", "content": "x"}, {"resolved_path": str(lay.project / "ext" / "a.txt")})
    cands = _by_path(_collect(t, lay))
    assert str(aws / "credentials") in cands
    assert str(lay.home / "cloud" / "credentials") not in cands
    assert str(elsewhere / "a.txt") in cands
    resource_plan = _plan(list(cands.values()), lay, options=Options(extra_paths="auto"))
    sources = {i.source_path: i for i in resource_plan.items}
    assert str(aws / "credentials") not in sources
    assert (str(aws / "credentials"), "secret") in {(n.path, n.why) for n in resource_plan.not_transferred}
    assert sources[str(elsewhere / "a.txt")].include is Include.YES


def test_collector_hermes_files_via_discover(transcript, lay):
    spill = lay.hermes_home / "cache" / "spillover" / "out-1.txt"
    write(spill, "big output\n")
    t = transcript(lay.project)
    t.tool("read_file", {"path": str(spill)}, {"content": "big"})
    t.tool("read_file", {"path": str(lay.hermes_home / "config.yaml")}, {"content": ""})
    discover = static_discover([HermesFileStub("spillover", str(spill), "cache/spillover/out-1.txt", 11, t.sid)])
    cands = _collect(t, lay, discover=discover)
    hermes = [c for c in cands if c.kind is ItemKind.HERMES_FILE]
    assert [(c.path, c.reasons) for c in hermes] == [(str(spill), ("spillover: cache/spillover/out-1.txt",))]
    assert discover.seen[0]["lineage_ids"] == [t.sid]
    assert not [c for c in cands if c.kind is ItemKind.EXTRA_FILE and str(lay.hermes_home) in c.path]


def test_collector_home_cwd_not_a_covering_workspace(transcript, lay):
    t = transcript(lay.home)
    t.tool("read_file", {"path": str(lay.notes / "todo.md")})
    cands = _collect(t, lay)
    assert cands[0].path == os.path.realpath(lay.home) and cands[0].origin is Origin.CWD
    assert str(lay.notes / "todo.md") in _by_path(cands)
    resource_plan = _plan(cands, lay)
    primary = next(i for i in resource_plan.items if i.origin is Origin.CWD)
    assert primary.include is Include.NO
    assert any(n.path == os.path.realpath(lay.home) for n in resource_plan.not_transferred)


def test_collector_other_repo_becomes_secondary_git_item(transcript, lay):
    lib = git_init(lay.home / "lib")
    write(lib / "pkg" / "mod.py")
    t = transcript(lay.project)
    t.tool("patch", {"path": str(lib / "pkg" / "mod.py")}, {"files_modified": [str(lib / "pkg" / "mod.py")]})
    cands = _by_path(_collect(t, lay))
    item = cands[os.path.realpath(lib)]
    assert item.kind is ItemKind.GIT_REPO and item.weight is Weight.WRITE
    assert any("pkg/mod.py" in r for r in item.reasons)


def test_collector_drops_missing_and_system_paths(transcript, lay):
    t = transcript(lay.project)
    t.tool("read_file", {"path": str(lay.home / "gone.txt")})
    t.terminal("cat /proc/cpuinfo /sys/kernel/x ~ /")
    t.terminal(f"ls {lay.project.parent}/proj/..")
    paths = {c.path for c in _collect(t, lay)}
    assert str(lay.home / "gone.txt") not in paths
    assert not {p for p in paths if p.startswith(("/proc", "/sys"))}
    assert str(lay.home) not in paths and "/" not in paths


def test_collector_without_cwd_has_no_primary(transcript, lay):
    t = transcript(None)
    t.tool("read_file", {"path": str(lay.notes / "todo.md")})
    cands = _collect(t, lay)
    assert all(c.origin is not Origin.CWD for c in cands)
    assert str(lay.notes / "todo.md") in _by_path(cands)


def test_collector_large_session_bounded(transcript, lay):
    """Scale (BRIEF §2: sessions up to 1M tokens): 12 000 rows with tool calls through the core's batch writer are
    collected in one pass, well within the planning budget."""
    import time

    t = transcript(lay.project)
    t.builder.turns(t.sid, 3000, with_tools=True)
    t.tool("write_file", {"path": str(lay.notes / "late.md")}, {"resolved_path": str(lay.notes / "late.md")})
    write(lay.notes / "late.md")
    rows = t.rows()
    assert len(rows) >= 12_000
    start = time.monotonic()
    cands = collector.collect(
        rows,
        t.session_row(),
        _hermes(lay),
        lay.cfg(),
        CeilingGitProbe(lay.home.parent),
        source_os="linux",
        discover=static_discover([]),
    )
    assert time.monotonic() - start < 10
    assert _by_path(cands)[str(lay.notes / "late.md")].weight is Weight.WRITE


def test_collector_skips_ancestors_of_the_workspace(transcript, lay):
    app = lay.home / "code" / "app"
    write(app / "main.go")
    write(lay.home / "code" / "other" / "x.go")
    t = transcript(app)
    t.terminal("ls ~/code ~/code/other")
    paths = {c.path for c in _collect(t, lay)}
    assert str(lay.home / "code") not in paths
    assert str(lay.home / "code" / "other") in paths
