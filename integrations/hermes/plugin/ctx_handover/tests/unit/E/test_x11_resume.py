"""Contract X11: a commit that a process left at any point continues in another process (DESIGN §3.4, §3.6).

Every case runs real processes (``_x11_driver``): the first prepares a job with every kind of workspace step — a
fresh git repository, a git update of a clean repository, a git return trip onto a repository that is unchanged
since it was handed away (dirty work and an excluded secret included), a file-tree update over the plugin's last
write, a single extra file, two session files (c3) — and dies with ``os._exit`` at the named point; the next one
rebuilds
the transaction from the newest journaled snapshot and finishes (or rolls back). The result must equal an
uninterrupted run, and the journal must show that the second process ran only the steps the first one had not
finished.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from ctx_handover.engine import filetree, gitops, materialize
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.jsonutil import to_wire
from ctx_handover.engine.model import (
    DestMode,
    Include,
    ItemKind,
    ItemRole,
    MapRow,
    Origin,
    PlanItem,
    TargetDecision,
)
from ctx_handover.tests.fakes.git_fake import DirBlockStore
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, dir_fingerprint, tree_digest
from ctx_handover.tests.unit.E._support import apply_fresh, assert_same_state, build_mixed, pack

JOB = "01JOBX11000000000000000000"


def _item(kind: ItemKind, source: Path, item_id: str) -> PlanItem:
    return PlanItem(
        item_id=item_id,
        kind=kind,
        source_path=str(source),
        role=ItemRole.PRIMARY if item_id == "w0" else ItemRole.EXTRA,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=1000,
    )


def _write_tree(root: Path, files: dict[str, str]) -> None:
    for rel, text in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)


class Scenario:
    """Sources, a prepared-for target and the driver spec (see module docstring)."""

    def __init__(self, git, user_home: Path, hermes_home: Path, tmp_path: Path, cfg, handover_root: Path):
        self.tmp = tmp_path
        self.root = handover_root
        self.hermes = hermes_home
        store = DirBlockStore(tmp_path / "store")
        src_staging = tmp_path / "src-staging"
        self.s1 = build_mixed(user_home / "s1")
        _, w0 = pack(git, self.s1.root, src_staging, cfg, store, item_id="w0")
        self.s2 = RepoBuilder.init(user_home / "s2")
        self.s2.write("a.txt", "one\n")
        self.s2.write("d/b.txt", "two\n")
        self.s2.commit_all("c1")
        _, seed = pack(git, self.s2.root, src_staging, cfg, store, item_id="seed")
        self.tgt = RepoBuilder(apply_fresh(git, store, seed, handover_root / "upd", cfg))
        self.s2.write("a.txt", "one more\n")
        self.s2.write("new/c.txt", "three\n")
        self.s2.commit_all("c2")
        self.s2.write("a.txt", "dirty\n")
        self.s2.write("untracked.txt", "u\n")
        basis = gitops.GitProbe(git).ref_tips(str(self.tgt.root))
        _, w1 = pack(git, self.s2.root, src_staging, cfg, store, item_id="w1", basis=basis)
        # return trip: the server repository "ret" went to the laptop with its dirty work; the laptop worked on
        self.ret = RepoBuilder.init(handover_root / "ret")
        self.ret.write("main.py", "print(1)\n")
        self.ret.write("lib/util.py", "x = 1\n")
        self.ret.commit_all("server c1")
        self.ret.write("main.py", "print(2)\n")
        self.ret.write("notes.txt", "wip\n")
        self.ret.write(".env", "TOKEN=server-only\n")
        res_out, out_item = pack(git, self.ret.root, src_staging, cfg, store, item_id="out")
        self.laptop = RepoBuilder(apply_fresh(git, store, out_item, user_home / "laptop", cfg))
        self.laptop.write("lib/util.py", "x = 2\n")
        self.laptop.commit_all("laptop c2")
        self.laptop.write("main.py", "print(3)\n")
        self.laptop.write("lib/new.py", "y = 1\n")
        (self.laptop.root / "notes.txt").unlink()
        basis = gitops.GitProbe(git).ref_tips(str(self.ret.root))
        _, w2 = pack(git, self.laptop.root, src_staging, cfg, store, item_id="w2", basis=basis)
        out_row = MapRow(
            str(self.ret.root), "peer", str(self.laptop.root), "out", state=materialize.map_state_of(res_out.state)
        )
        self.notes_src = user_home / "notes"
        _write_tree(self.notes_src, {"n1.md": "first\n", "n2.md": "second\n", "sub/n4.md": "fourth\n"})
        entries, _ = filetree.scan(self.notes_src, IgnoreMatcher.for_tree(self.notes_src, ()), cfg)
        mid = filetree.pack_tree(entries, self.notes_src, store)
        notes = handover_root / "notes"
        fp = filetree.commit(
            filetree.prepare_fresh(store, mid, handover_root / ".ctxh-n", cfg.platform_rules(), dest=notes)
        )["fingerprint"]
        _write_tree(self.notes_src, {"n1.md": "first, changed\n", "n3.md": "third\n"})
        (self.notes_src / "n2.md").unlink()
        entries, _ = filetree.scan(self.notes_src, IgnoreMatcher.for_tree(self.notes_src, ()), cfg)
        t1 = {"item_id": "t1", "kind": "file_tree", "manifest": filetree.pack_tree(entries, self.notes_src, store)}
        todo = user_home / "todo.txt"
        todo.write_text("todo\n")
        e1 = {"item_id": "e1", **filetree.pack_file(todo, store)}
        self.hfiles = [hermes_home / "cache" / "spillover" / "sx" / f"call_{n}.txt" for n in (1, 2)]
        hermes = []
        for n, target in enumerate(self.hfiles, 1):
            obj = gitops.put_bytes(store, f"tool output {n}\n".encode(), "application/octet-stream").object_id
            hermes.append([{"kind": "hermes_file", "object": obj, "hermes_kind": "spillover"}, str(target)])
        in_row = MapRow(str(notes), "peer", str(self.notes_src), "in", state=materialize.map_state_of(fp))
        items = [
            (
                _item(ItemKind.GIT_REPO, self.s1.root, "w0"),
                handover_root / "fresh",
                DestMode.FRESH,
                "workspace-root",
                w0,
                None,
            ),
            (_item(ItemKind.GIT_REPO, self.s2.root, "w1"), self.tgt.root, DestMode.UPDATE, "return-mapping", w1, None),
            (
                _item(ItemKind.GIT_REPO, self.laptop.root, "w2"),
                self.ret.root,
                DestMode.RETURN,
                "return-mapping",
                w2,
                out_row,
            ),
            (_item(ItemKind.FILE_TREE, self.notes_src, "t1"), notes, DestMode.UPDATE, "return-mapping", t1, in_row),
            (_item(ItemKind.EXTRA_FILE, todo, "e1"), handover_root / "todo.txt", DestMode.FRESH, "user", e1, None),
        ]
        store.dump_manifests(tmp_path / "manifests.json")
        self.spec = {
            "job_id": JOB,
            "staging": str(tmp_path / "staging"),
            "store": str(tmp_path / "store"),
            "manifests": str(tmp_path / "manifests.json"),
            "journal": str(tmp_path / "journal.jsonl"),
            "home": str(user_home),
            "hermes_home": str(hermes_home),
            "items": [
                {
                    "item": to_wire(item),
                    "decision": to_wire(TargetDecision(item.item_id, str(dest), mode, reason)),
                    "pack": pitem,
                    "out_row": to_wire(row) if row is not None else None,
                }
                for item, dest, mode, reason, pitem, row in items
            ],
            "hermes": hermes,
        }
        self.before = self.observe()

    def observe(self) -> tuple:
        """Everything a rollback must restore: the update repository's observable state, the plugin's earlier
        tree write byte for byte (mtimes included), what else lies in the handover root and the session area."""
        tgt, ret = self.tgt, self.ret
        notes = {k: v for k, v in dir_fingerprint(self.root / "notes").items()}
        mtimes = {p.name: p.stat().st_mtime_ns for p in sorted((self.root / "notes").rglob("*")) if p.is_file()}
        spill = (
            sorted(str(p.relative_to(self.hermes)) for p in (self.hermes / "cache").rglob("*"))
            if (self.hermes / "cache").exists()
            else []
        )
        return (
            tgt.head(),
            tgt.refs(),
            tgt.index_entries(),
            tgt.status(),
            tgt.worktree_digest(),
            tgt.stash_list(),
            ret.head(),
            ret.refs(),
            ret.index_entries(),
            ret.status(),
            ret.worktree_digest(),
            notes,
            mtimes,
            sorted(os.listdir(self.root)),
            spill,
        )

    def run(self, crash: dict | None = None, *, fail_last: bool = False) -> int:
        spec = dict(self.spec, crash=crash, fail_last=fail_last)
        path = self.tmp / "spec.json"
        path.write_text(json.dumps(spec))
        env = dict(os.environ, PYTHONPATH=os.pathsep.join(p for p in sys.path if p), PYTHONDONTWRITEBYTECODE="1")
        proc = subprocess.run(  # noqa: S603 - fixed argv, this interpreter
            [sys.executable, "-m", "ctx_handover.tests.unit.E._x11_driver", str(path)],
            env=env,
            capture_output=True,
            timeout=300,
        )
        assert proc.returncode in (0, 17), proc.stderr.decode(errors="replace")[-3000:]
        return proc.returncode

    def events(self) -> list[dict]:
        return [json.loads(x) for x in Path(self.spec["journal"]).read_text().splitlines() if x]

    def assert_committed(self) -> None:
        assert_same_state(
            self.s1, RepoBuilder(self.root / "fresh"), excluded=[b".env"], ignored=[b"ignored", b"build.log"]
        )
        assert_same_state(self.s2, self.tgt)
        assert_same_state(self.laptop, self.ret, excluded=[b".env"])
        assert (self.ret.root / ".env").read_text() == "TOKEN=server-only\n"
        assert tree_digest(self.root / "notes") == tree_digest(self.notes_src)
        assert (self.root / "todo.txt").read_text() == "todo\n"
        assert [p.read_text() for p in self.hfiles] == ["tool output 1\n", "tool output 2\n"]
        assert sorted(os.listdir(self.root)) == ["fresh", "notes", "ret", "todo.txt", "upd"]
        assert sorted(os.listdir(self.hfiles[0].parent)) == ["call_1.txt", "call_2.txt"]
        assert not (self.tmp / "staging" / materialize.STEP_DIR).exists()

    def assert_only_missing_steps_rerun(self, first_pid: int) -> None:
        """No step the first process journaled as done appears again (intent or done) from a later process."""
        evs = self.events()
        done_first = {e["step"] for e in evs if e["kind"] == "done" and e["pid"] == first_pid}
        later = [e for e in evs if e["pid"] != first_pid and e["kind"] in ("intent", "done")]
        assert done_first.isdisjoint({e["step"] for e in later if e.get("direction") == "forward"})
        forward_done = [e["step"] for e in evs if e["kind"] == "done" and e.get("direction") == "forward"]
        assert len(forward_done) == len(set(forward_done))


@pytest.fixture
def scenario(git, user_home, hermes_home, tmp_path, cfg, handover_root):
    return Scenario(git, user_home, hermes_home, tmp_path, cfg, handover_root)


FORWARD_CRASHES = {
    "between_steps": {"at": "intent", "step": "c1:t1"},
    "git_fresh_after_rename": {"at": "journal", "kind": "done", "step": "c1:w0"},
    "git_update_after_read_tree": {"at": "call", "target": "gitapply._apply_refs", "nth": 1, "when": "before"},
    "git_return_after_read_tree": {"at": "call", "target": "gitapply._apply_refs", "nth": 2, "when": "before"},
    "tree_update_after_first_backup": {"at": "call", "target": "filetree._move", "nth": 1, "when": "after"},
    "extra_file_after_link": {"at": "journal", "kind": "done", "step": "c1:e1"},
    "session_file_after_first": {
        "at": "call",
        "target": "materialize.WorkspaceTxn._place_one",
        "nth": 1,
        "when": "after",
    },
}


@pytest.mark.parametrize("where", sorted(FORWARD_CRASHES))
def test_x11_crash_then_other_process_completes(scenario, where):
    """★ X11 — a process dies inside or between the commit's workspace steps; a second process finishes the
    commit from the journaled snapshot, running only the missing steps, and the result equals an uninterrupted
    commit (mutants: no identity check for a resumed fresh/extra step; ``resume=False`` for a git update step;
    a backup slot that exists is checked and moved again)."""
    assert scenario.run(FORWARD_CRASHES[where]) == 17
    first_pid = scenario.events()[-1]["pid"]
    assert scenario.events()[-1]["kind"] != "finished"
    assert scenario.run() == 0
    assert scenario.events()[-1]["outcome"] == "committed"
    scenario.assert_committed()
    scenario.assert_only_missing_steps_rerun(first_pid)


ROLLBACK_CASES = {
    "git_update_crash_then_failure": (FORWARD_CRASHES["git_update_after_read_tree"], None),
    "git_return_crash_then_failure": (FORWARD_CRASHES["git_return_after_read_tree"], None),
    "session_file_crash_then_failure": (FORWARD_CRASHES["session_file_after_first"], None),
    "crash_inside_git_inverse": (
        None,
        # calls: PREPARE of w0, forward of w1, forward of w2, then the inverse of w2 after its work-tree merge
        {"at": "call", "target": "gitapply._update_refs", "nth": 4, "when": "before"},
    ),
}


@pytest.mark.parametrize("case", sorted(ROLLBACK_CASES))
def test_x11_rollback_across_processes_restores_target(scenario, case):
    """★ X11 — a commit that fails after a crash, or whose rollback itself is cut short, is rolled back by the next
    process from the journal: the target is exactly as before PREPARE (update repository, the plugin's earlier
    tree write with its mtimes, the handover root, the session-file area) and every inverse ran once (mutants:
    session files placed by the dead process not recorded; an interrupted git inverse rerun as a two-way merge)."""
    first_crash, rollback_crash = ROLLBACK_CASES[case]
    if first_crash is not None:
        assert scenario.run(first_crash) == 17
        assert scenario.run(fail_last=True) == 0
    else:
        assert scenario.run(rollback_crash, fail_last=True) == 17
        assert scenario.events()[-1]["direction"] == "rollback"
        assert scenario.run(fail_last=True) == 0
    assert scenario.events()[-1]["outcome"] == "rolled_back"
    assert scenario.observe() == scenario.before
    inverses = [e["step"] for e in scenario.events() if e["kind"] == "done" and e.get("direction") == "rollback"]
    assert len(inverses) == len(set(inverses))
    assert not (scenario.tmp / "staging" / materialize.STEP_DIR).exists()
