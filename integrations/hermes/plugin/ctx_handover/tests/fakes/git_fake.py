"""Fakes of the workspace cluster (E) for other clusters' tests and for E's own unit tests.

``DirBlockStore``    a block store on disk implementing ``contracts.BlockSource`` and ``BlockSink`` (blocks
                     ``<root>/blocks/<2>/<62>``, manifests in memory and as JSON); counts reads and writes so
                     tests can assert reuse; ``corrupt``/``drop`` simulate damaged or missing blocks.
``FakeGitProbe``     scripted answers for ``contracts.GitProbe`` plus the extra ``gitops.GitProbe`` questions.
``RecordingJournal`` the ``journal(kind, step, data)`` callable ``materialize.WorkspaceTxn`` expects.
``FakeWorkspaceTxn`` the ``WorkspaceTxn`` surface without git or files, with fault injection per step, for the
                     orchestration cluster (A).
"""

from __future__ import annotations

import hashlib
import json
import os
from collections.abc import Callable, Iterable, Iterator, Sequence
from dataclasses import dataclass, field
from pathlib import Path

from ctx_handover.engine.errors import NotFound
from ctx_handover.engine.model import DestMode, GitInfo, ObjectManifest


class DirBlockStore:
    """Blocks on disk, manifests by object id (see module docstring)."""

    def __init__(self, root: Path):
        self.root = Path(root)
        (self.root / "blocks").mkdir(parents=True, exist_ok=True)
        self._manifests: dict[str, ObjectManifest] = {}
        self.writes = 0
        self.reads = 0
        self.manifest_puts = 0

    def _path(self, block_id: str) -> Path:
        return self.root / "blocks" / block_id[:2] / block_id[2:]

    # BlockSink
    def put_manifests(self, manifests: Sequence[ObjectManifest]) -> int:
        new = 0
        for m in manifests:
            self.manifest_puts += 1
            if m.object_id not in self._manifests:
                new += 1
            self._manifests[m.object_id] = m
        return new

    def missing(self, ids: Sequence[str]) -> list[str]:
        return [i for i in ids if not self._path(i).is_file()]

    def write_blocks(self, frames: Iterable[tuple[str, bytes]]) -> tuple[int, list[tuple[str, str]]]:
        accepted, errors = 0, []
        for bid, raw in frames:
            if hashlib.sha256(raw).hexdigest() != bid:
                errors.append((bid, "block_corrupt"))
                continue
            path = self._path(bid)
            path.parent.mkdir(parents=True, exist_ok=True)
            tmp = path.with_suffix(".tmp")
            tmp.write_bytes(raw)
            os.replace(tmp, path)
            self.writes += 1
            accepted += 1
        return accepted, errors

    # BlockSource
    def manifests(self, object_ids: Sequence[str]) -> list[ObjectManifest]:
        out = []
        for oid in object_ids:
            if oid not in self._manifests:
                raise NotFound("unknown object", object=oid)
            out.append(self._manifests[oid])
        return out

    def read_blocks(self, ids: Sequence[str]) -> Iterator[tuple[str, bytes]]:
        for bid in reversed(list(ids)):  # deliberately not in request order
            path = self._path(bid)
            if path.is_file():
                self.reads += 1
                yield bid, path.read_bytes()

    # test helpers
    def corrupt(self, block_id: str, data: bytes = b"garbage") -> None:
        self._path(block_id).write_bytes(data)

    def drop(self, block_id: str) -> None:
        self._path(block_id).unlink()

    def add_manifest(self, m: ObjectManifest) -> None:
        """Store a manifest as given (tests forging peer data)."""
        self._manifests[m.object_id] = m

    def dump_manifests(self, path: Path) -> None:
        path.write_text(json.dumps({k: v.__dict__ for k, v in self._manifests.items()}, default=list))

    def load_manifests(self, path: Path) -> None:
        """Read manifests written by ``dump_manifests`` (a second process opening the same store)."""
        for oid, d in json.loads(Path(path).read_text()).items():
            self._manifests[oid] = ObjectManifest(**{**d, "blocks": tuple(d["blocks"])})


@dataclass
class FakeGitProbe:
    """Scripted ``GitProbe`` (paths are compared as strings; satisfies ``contracts.GitProbe``)."""

    toplevels: dict[str, str] = field(default_factory=dict)
    common_dirs: dict[str, str] = field(default_factory=dict)
    shallow: set[str] = field(default_factory=set)
    partial: set[str] = field(default_factory=set)
    ignored: set[str] = field(default_factory=set)
    infos: dict[str, GitInfo] = field(default_factory=dict)
    clean: dict[str, tuple[bool, str]] = field(default_factory=dict)
    roots: dict[str, tuple[str, ...]] = field(default_factory=dict)
    tips: dict[str, tuple[str, ...]] = field(default_factory=dict)
    calls: list[tuple[str, tuple]] = field(default_factory=list)

    def _rec(self, name: str, *args) -> None:
        self.calls.append((name, args))

    def toplevel(self, path: str) -> str | None:
        self._rec("toplevel", path)
        for root, top in self.toplevels.items():
            if path == root or path.startswith(root.rstrip("/") + "/"):
                return top
        return None

    def common_dir(self, path: str) -> str | None:
        self._rec("common_dir", path)
        return self.common_dirs.get(path)

    def is_shallow(self, path: str) -> bool:
        self._rec("is_shallow", path)
        return path in self.shallow

    def is_partial(self, path: str) -> bool:
        """Like ``gitops.GitProbe.is_partial`` (V30): True for a scripted partial-clone root and any path in it."""
        self._rec("is_partial", path)
        return any(path == root or path.startswith(root.rstrip("/") + "/") for root in self.partial)

    def is_ignored(self, path: str) -> bool:
        """Like ``gitops.GitProbe.is_ignored`` (R36): True for a scripted ignored directory and any path below it,
        never for a sibling with a common prefix; everything else is not ignored."""
        self._rec("is_ignored", path)
        return any(path == d or path.startswith(d.rstrip("/") + "/") for d in self.ignored)

    def info(self, root: str, cfg) -> GitInfo:
        self._rec("info", root)
        return self.infos[root]

    def status_clean(self, root: str) -> tuple[bool, str]:
        self._rec("status_clean", root)
        return self.clean.get(root, (True, ""))

    def root_commits(self, root: str) -> tuple[str, ...]:
        return self.roots.get(root, ())

    def ref_tips(self, root: str) -> tuple[str, ...]:
        return self.tips.get(root, ())


class RecordingJournal:
    """``journal(kind, step, data)`` that records every call (``WorkspaceTxn`` journals each sub-step)."""

    def __init__(self) -> None:
        self.events: list[tuple[str, str, dict]] = []

    def __call__(self, kind: str, step: str, data: dict) -> None:
        self.events.append((kind, step, dict(data)))

    def steps(self, kind: str | None = None) -> list[str]:
        return [s for k, s, _ in self.events if kind is None or k == kind]


class FakeWorkspaceTxn:
    """``materialize.WorkspaceTxn`` surface without git or files (for cluster A).

    ``fail`` maps a step name (``prepare:<item_id>``, ``c1:<item_id>``, ``c1-inverse:<item_id>``, ``c3``,
    ``c3-inverse``) to an exception instance raised when that step runs. ``log`` records every step in order.
    ``snapshot()``/``restore()`` carry the prepared items and the log (contract X11 shape, no files);
    ``map_states()`` returns ``{item_id: {"tree": {"v": 1, "files": {}}}}`` per item.
    """

    def __init__(
        self,
        job_id: str = "job",
        staging=None,
        git=None,
        store=None,
        journal=None,
        *,
        fail: dict[str, BaseException] | None = None,
        cfg=None,
        rules=None,
        now=None,
        cancelled=None,
    ):
        self.job_id = job_id
        self.fail = dict(fail or {})
        self.log: list[str] = []
        self._items: list[tuple[str, str, DestMode]] = []
        self.discarded = False

    def snapshot(self) -> dict:
        return {
            "v": 2,
            "job_id": self.job_id,
            "items": [[i, d, m.value] for i, d, m in self._items],
            "log": list(self.log),
        }

    @classmethod
    def restore(cls, snapshot: dict, staging=None, git=None, store=None, journal=None, **kw) -> FakeWorkspaceTxn:
        fake = cls(snapshot.get("job_id", "job"), fail=kw.get("fail"))
        fake._items = [(i, d, DestMode(m)) for i, d, m in snapshot.get("items") or ()]
        fake.log = list(snapshot.get("log") or ())
        return fake

    def map_states(self) -> dict[str, dict]:
        return {item_id: {"tree": {"v": 1, "files": {}}} for item_id, _, _ in self._items}

    def _step(self, name: str) -> None:
        self.log.append(name)
        if name in self.fail:
            raise self.fail[name]

    def prepare_item(self, item, decision, pack_item: dict, *, out_row=None) -> None:
        self._step(f"prepare:{item.item_id}")
        self._items.append((item.item_id, decision.dest, decision.mode))

    def commit_steps(self) -> list[tuple[str, Callable[[], None], Callable[[], None]]]:
        steps = []
        for item_id, _, _ in self._items:
            steps.append(
                (
                    f"c1:{item_id}",
                    (lambda i=item_id: self._step(f"c1:{i}")),
                    (lambda i=item_id: self._step(f"c1-inverse:{i}")),
                )
            )
        return steps

    def place_hermes_files(self, files: Sequence[tuple[dict, Path]]) -> tuple[Callable[[], None], Callable[[], None]]:
        return (lambda: self._step("c3")), (lambda: self._step("c3-inverse"))

    def discard(self) -> None:
        self.log.append("discard")
        self.discarded = True

    def dests(self) -> list[tuple[str, str, DestMode]]:
        return list(self._items)
