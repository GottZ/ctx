"""Scale evidence: many files and one large file stream through pack and apply without being held in memory.

Sizes default to 1 500 small files plus one 48 MiB file (unit gate; the Python peak must stay below 24 MiB).
``CTXH_SCALE_FILES`` / ``CTXH_SCALE_BIG_MB`` raise them; ``CTXH_SCALE_REPORT`` names a JSON file that receives
the measurements (runtime, Python peak memory via ``tracemalloc``, process and child max RSS);
``CTXH_SCALE_NO_TRACE=1`` measures runtime without ``tracemalloc`` (which slows Python several times) and skips
the memory assertion. The assertion is the design rule: the Python peak stays far below the size of the large
file (blocks are streamed in batches of at most 16 × 512 KiB)."""

from __future__ import annotations

import json
import os
import resource
import time
import tracemalloc
from pathlib import Path

from ctx_handover.engine import filetree, gitapply, gitpack
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import AT, LINUX, make_cfg

FILES = int(os.environ.get("CTXH_SCALE_FILES", "1500"))
BIG_MB = int(os.environ.get("CTXH_SCALE_BIG_MB", "48"))
TRACE = os.environ.get("CTXH_SCALE_NO_TRACE") != "1"
PEAK_LIMIT = 24 * 1024**2


def _populate(root: Path) -> None:
    for i in range(FILES):
        d = root / f"d{i % 200:03d}"
        if i < 200:
            d.mkdir(parents=True, exist_ok=True)
        (d / f"f{i:06d}.txt").write_bytes(b"%06d " % i + b"x" * 1000)
    chunk = os.urandom(1024 * 1024)
    with open(root / "big.bin", "wb") as fh:
        for i in range(BIG_MB):
            fh.write(chunk[:-8] + i.to_bytes(8, "big"))


def _measure(fn):
    if not TRACE:
        t0 = time.monotonic()
        result = fn()
        return result, round(time.monotonic() - t0, 2), 0
    tracemalloc.start()
    t0 = time.monotonic()
    try:
        result = fn()
    finally:
        _, peak = tracemalloc.get_traced_memory()
        tracemalloc.stop()
    return result, round(time.monotonic() - t0, 2), peak


def _rss() -> dict:
    return {
        "self_max_rss_mib": round(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024, 1),
        "children_max_rss_mib": round(resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss / 1024, 1),
    }


def test_scale_tree_and_repo_stream(git, user_home, hermes_home, tmp_path, store, handover_root):
    cfg = make_cfg(user_home, hermes_home, max_file_bytes=(BIG_MB + 1) * 1024**2, max_total_bytes=64 * 1024**3)
    report: dict = {"files": FILES, "big_mib": BIG_MB, "tracemalloc": TRACE}

    tree = user_home / "tree"
    _populate(tree)
    (entries, _), t_scan, p_scan = _measure(lambda: filetree.scan(tree, IgnoreMatcher.for_tree(tree, ()), cfg))
    mid, t_pack, p_pack = _measure(lambda: filetree.pack_tree(entries, tree, store))
    state, t_apply, p_apply = _measure(
        lambda: filetree.prepare_fresh(
            store, mid, handover_root / ".ctxh-s", LINUX, dest=handover_root / "tree", max_files=cfg.max_files
        )
    )
    filetree.commit(state)
    assert (handover_root / "tree" / "big.bin").stat().st_size == BIG_MB * 1024**2
    report["tree"] = {
        "entries": len(entries),
        "scan_s": t_scan,
        "scan_peak_mib": round(p_scan / 2**20, 1),
        "pack_s": t_pack,
        "pack_peak_mib": round(p_pack / 2**20, 1),
        "apply_s": t_apply,
        "apply_peak_mib": round(p_apply / 2**20, 1),
    }

    repo = RepoBuilder.init(user_home / "repo")
    _populate(repo.root)
    repo.write("seed.txt", "seed\n")
    repo.add("seed.txt")
    repo.commit("seed")
    res, t_gpack, p_gpack = _measure(
        lambda: gitpack.pack_repo(
            git,
            str(repo.root),
            tmp_path / "staging" / "w0",
            basis_candidates=(),
            cfg=cfg,
            store=store,
            created_at=AT,
            force=False,
        )
    )
    item = gitpack.pack_item("w0", res)
    dest = handover_root / "repo"
    gstate, t_gapply, p_gapply = _measure(
        lambda: gitapply.prepare_fresh(git, store, item, handover_root / ".ctxh-g", rules=LINUX, cfg=cfg, dest=dest)
    )
    gitapply.commit(git, gstate, item)
    assert (dest / "big.bin").stat().st_size == BIG_MB * 1024**2
    pack_size = store.manifests([res.pack_object])[0].size
    report["git"] = {
        "pack_mib": round(pack_size / 2**20, 1),
        "pack_s": t_gpack,
        "pack_peak_mib": round(p_gpack / 2**20, 1),
        "apply_s": t_gapply,
        "apply_peak_mib": round(p_gapply / 2**20, 1),
    }
    report.update(_rss())
    out = os.environ.get("CTXH_SCALE_REPORT")
    if out:
        Path(out).write_text(json.dumps(report, indent=2), encoding="utf-8")
    for peak in (p_pack, p_apply, p_gpack, p_gapply):
        assert peak < PEAK_LIMIT, report
