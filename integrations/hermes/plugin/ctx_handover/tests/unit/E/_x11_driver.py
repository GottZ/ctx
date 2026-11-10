"""One process of the X11 crash-resumption test (run as ``python -m ctx_handover.tests.unit.E._x11_driver spec``).

It plays the orchestration's commit operation exactly as ``local_target`` runs it (DESIGN §3.4, §3.6): the
workspace snapshot is journaled after PREPARE and with every step result; a new process rebuilds the transaction
from the newest snapshot (``WorkspaceTxn.restore``), skips steps journaled as done, runs the step whose intent was
journaled again, and on a failure switches the journal to ``rollback`` and runs the inverses of the done steps in
reverse order, skipping inverses already done. The journal is an fsynced JSON-lines file; every event carries the
pid, so the test can prove which process ran which step.

Faults (``spec["crash"]``) end the process with ``os._exit`` — no ``finally``, no flush, like a power cut:
``{"at": "journal", "kind", "step"}`` when the transaction journals that event (inside a step, after its effect),
``{"at": "intent", "step"}`` right after the commit op journaled the intent of a step (between steps),
``{"at": "call", "target": "<module>.<attr>[.<attr>]", "nth", "when": "before"|"after"}`` around the nth call of
a plugin function (inside a step); with ``"argv": [...]`` only calls whose list argument starts with those words
count (``gitops.Git.run`` of one git subcommand). ``spec["fail_last"]`` adds a last step that raises (the commit then
rolls back). ``spec["abort"]`` plays the target abort instead of the commit (DESIGN §3.2, §3.6: a job whose PREPARE
was cut short is aborted, not resumed): the transaction from the newest snapshot — or a new one when PREPARE never
journaled one — is discarded.

``needs_attention`` (DESIGN §3.4): an inverse that raises, and a forward step whose error carries
``details.needs_attention`` (it could not undo its own partial change, e.g. an orphaned ``index.lock``), are journaled
as ``needs_attention`` events; the commit then ends ``needs_attention`` and keeps its staging. ``local_target`` books
the inverse case so today; the forward case is a wish to cluster A.
"""

from __future__ import annotations

import importlib
import json
import os
import sys
from pathlib import Path

from ctx_handover.engine import materialize
from ctx_handover.engine.gitops import Git, reset_process_caches
from ctx_handover.engine.jsonutil import from_wire
from ctx_handover.engine.model import MapRow, PlanItem, TargetDecision
from ctx_handover.tests.fakes.git_fake import DirBlockStore
from ctx_handover.tests.unit.E._support import make_cfg

CRASH_RC = 17


def _crash() -> None:
    os._exit(CRASH_RC)


def _append(journal: Path, event: dict) -> None:
    with open(journal, "a", encoding="utf-8") as fh:
        fh.write(json.dumps({**event, "pid": os.getpid()}) + "\n")
        fh.flush()
        os.fsync(fh.fileno())


def _events(journal: Path) -> list[dict]:
    if not journal.exists():
        return []
    return [json.loads(line) for line in journal.read_text(encoding="utf-8").splitlines() if line]


def _arm_call(crash: dict) -> None:
    module_name, *attrs = crash["target"].split(".")
    owner = importlib.import_module(f"ctx_handover.engine.{module_name}") if module_name != "os" else os
    for attr in attrs[:-1]:
        owner = getattr(owner, attr)
    original = getattr(owner, attrs[-1])
    calls = {"n": 0}

    words = crash.get("argv")

    def counts(args: tuple) -> bool:
        if words is None:
            return True
        argv = next((a for a in args if isinstance(a, list)), None)
        return argv is not None and argv[: len(words)] == words

    def wrapped(*args, **kwargs):
        if not counts(args):
            return original(*args, **kwargs)
        calls["n"] += 1
        if calls["n"] == crash["nth"] and crash.get("when", "before") == "before":
            _crash()
        result = original(*args, **kwargs)
        if calls["n"] == crash["nth"] and crash.get("when") == "after":
            _crash()
        return result

    setattr(owner, attrs[-1], wrapped)


def main(spec_path: str) -> int:
    spec = json.loads(Path(spec_path).read_text(encoding="utf-8"))
    reset_process_caches()
    journal = Path(spec["journal"])
    crash = spec.get("crash")
    if crash and crash["at"] == "call":
        _arm_call(crash)
    store = DirBlockStore(Path(spec["store"]))
    store.load_manifests(Path(spec["manifests"]))
    cfg = make_cfg(Path(spec["home"]), Path(spec["hermes_home"]))
    git = Git()
    staging = Path(spec["staging"])

    def ws_journal(kind: str, step: str, data: dict) -> None:
        if crash and crash["at"] == "journal" and (crash["kind"], crash["step"]) == (kind, step):
            _crash()

    kw = {"cfg": cfg, "now": lambda: 1_762_646_400.0}
    events = _events(journal)
    snap = None
    for ev in events:
        if ev["kind"] == "prepared":
            snap = ev["snapshot"]
        elif ev["kind"] == "done" and isinstance(ev.get("result"), dict) and "ws" in ev["result"]:
            snap = ev["result"]["ws"]
    if spec.get("abort"):
        if snap is None:
            txn = materialize.WorkspaceTxn(spec["job_id"], staging, git, store, ws_journal, **kw)
        else:
            txn = materialize.WorkspaceTxn.restore(snap, staging, git, store, ws_journal, **kw)
        txn.discard()
        _append(journal, {"kind": "finished", "outcome": "aborted"})
        return 0
    if snap is None:
        txn = materialize.WorkspaceTxn(spec["job_id"], staging, git, store, ws_journal, **kw)
        for entry in spec["items"]:
            row = from_wire(MapRow, entry["out_row"]) if entry.get("out_row") else None
            txn.prepare_item(
                from_wire(PlanItem, entry["item"]),
                from_wire(TargetDecision, entry["decision"]),
                entry["pack"],
                out_row=row,
            )
        _append(journal, {"kind": "prepared", "snapshot": txn.snapshot()})
    else:
        txn = materialize.WorkspaceTxn.restore(snap, staging, git, store, ws_journal, **kw)

    def snap_after(fn):
        def run(*_a):
            fn()
            return {"ws": txn.snapshot()}

        return run

    steps = [(name, snap_after(fwd), snap_after(inv)) for name, fwd, inv in txn.commit_steps()]
    f3, i3 = txn.place_hermes_files([(item, Path(target)) for item, target in spec["hermes"]])
    steps.append(("c3:hermes_files", snap_after(f3), snap_after(i3)))
    if spec.get("fail_last"):

        def boom() -> dict:
            raise RuntimeError("injected failure after the workspace steps")

        steps.append(("c9:fail", boom, lambda *_a: None))

    direction, done_fwd, done_inv, attention = "forward", [], set(), []
    for ev in _events(journal):
        if ev["kind"] == "direction":
            direction = ev["direction"]
        elif ev["kind"] == "needs_attention":
            attention.append(ev["step"])
        elif ev["kind"] == "done" and ev.get("direction") == "rollback":
            done_inv.add(ev["step"])
        elif ev["kind"] == "done" and ev["step"] not in done_fwd:
            done_fwd.append(ev["step"])
    if direction == "forward":
        name = None
        try:
            for name, fwd, _inv in steps:
                if name in done_fwd:
                    continue
                _append(journal, {"kind": "intent", "step": name, "direction": "forward"})
                if crash and crash["at"] == "intent" and crash["step"] == name:
                    _crash()
                result = fwd()
                _append(journal, {"kind": "done", "step": name, "direction": "forward", "result": result})
                done_fwd.append(name)
        except Exception as exc:  # the commit op's rollback switch
            if (getattr(exc, "details", None) or {}).get("needs_attention"):
                _attention(journal, name, exc)
                attention.append(name)
            _append(journal, {"kind": "direction", "direction": "rollback", "cause": str(exc)[:200]})
            direction = "rollback"
        else:
            txn.discard()
            _append(journal, {"kind": "finished", "outcome": "committed"})
            return 0
    inverse = {name: inv for name, _f, inv in steps}
    for name in reversed(done_fwd):
        if name in done_inv or name not in inverse:
            continue
        _append(journal, {"kind": "intent", "step": name, "direction": "rollback"})
        result = None
        try:
            result = inverse[name]()
        except Exception as exc:  # a failing inverse: needs_attention, the rollback goes on
            _attention(journal, name, exc)
            attention.append(name)
        _append(journal, {"kind": "done", "step": name, "direction": "rollback", "result": result})
    if attention:
        _append(journal, {"kind": "finished", "outcome": "needs_attention", "steps": attention})
        return 0
    txn.discard()
    _append(journal, {"kind": "finished", "outcome": "rolled_back"})
    return 0


def _attention(journal: Path, step: str | None, exc: BaseException) -> None:
    details = getattr(exc, "details", None) or {}
    message = getattr(exc, "message", None) or str(exc)
    _append(
        journal,
        {
            "kind": "needs_attention",
            "step": step,
            "error": {"type": type(exc).__name__, "message": str(message)[:2000], "details": details},
        },
    )


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
