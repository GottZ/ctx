"""K0/L2: cost bounds of the source policy at scale — counted filesystem calls instead of wall-clock time.

E measured about 1 ms per file in ``check_source_file`` (reports/E.md §7): every call resolved the Hermes bases,
``$HOME`` and the item root again (13 ``realpath`` calls per file). These tests pin the fix by counting calls,
which stays deterministic under any host load: without a cache a check resolves at most five paths; with one
``SourceCheckCache`` per scan the ``realpath`` work is per directory, not per file, while every entry still gets
its own ``lstat`` and every regular file its own sniff (one ``open``). ``CTXH_POLICY_PERF_FILES`` raises the
file count (default 600)."""

from __future__ import annotations

import os
from collections import Counter

import pytest

from ctx_handover.engine import policy
from ctx_handover.engine.model import Options

FILES = int(os.environ.get("CTXH_POLICY_PERF_FILES", "600"))
DIRS = 6


@pytest.fixture
def tree(tmp_path):
    home = tmp_path / "home"
    hermes = tmp_path / "hermes-home"
    root = home / "proj"
    for i in range(FILES):
        d = root / f"d{i % DIRS}"
        d.mkdir(parents=True, exist_ok=True)
        (d / f"f{i:05d}.txt").write_bytes(b"%05d " % i + b"x" * 200)
    (hermes / "plugin-data" / "ctx_handover").mkdir(parents=True)
    cfg = policy.PolicyConfig.from_options(
        Options(),
        home=home,
        hermes_home=hermes,
        hermes_root=hermes,
        plugin_dir=hermes / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
    )
    entries = sorted(root.iterdir()) + sorted(p for d in root.iterdir() for p in d.iterdir())
    return root, cfg, entries


def _counted(monkeypatch, run):
    """Run ``run()`` while counting ``os.path.realpath``, ``os.lstat`` and ``os.open`` (realpath's own lstat
    calls included); the spies delegate to the real functions."""
    calls: Counter[str] = Counter()
    real = {"realpath": os.path.realpath, "lstat": os.lstat, "open": os.open}

    def spy(name):
        def wrapper(*a, **kw):
            calls[name] += 1
            return real[name](*a, **kw)

        return wrapper

    with monkeypatch.context() as m:
        m.setattr(os.path, "realpath", spy("realpath"))
        m.setattr(os, "lstat", spy("lstat"))
        m.setattr(os, "open", spy("open"))
        result = run()
    return result, calls


@pytest.mark.skipif(os.name != "posix", reason="parent-directory resolution is the POSIX path")
def test_scan_cache_resolves_per_directory_not_per_file(tree, monkeypatch):
    """With one cache for the scan: ``realpath`` runs once per base, item root and directory (not per file);
    ``lstat`` is one per entry plus the components of those few resolutions; every regular file is opened for
    the sniff exactly once."""
    root, cfg, entries = tree
    files = [p for p in entries if p.is_file()]
    cache = policy.SourceCheckCache()
    verdicts, calls = _counted(
        monkeypatch,
        lambda: [policy.check_source_file(p, cfg, explicit=False, item_root=root, cache=cache) for p in entries],
    )
    assert all(v.allowed for v in verdicts)
    resolutions = 3 + 1 + DIRS + 1
    assert calls["realpath"] <= resolutions, calls
    depth = max(len(p.parts) for p in entries)
    assert calls["lstat"] <= len(entries) + resolutions * depth, calls
    assert calls["open"] == len(files), calls


def test_uncached_check_resolves_at_most_five_paths(tree, monkeypatch):
    """Without a cache each call resolves itself (same verdicts, no state between calls), but only the three
    Hermes bases, the item root (or ``$HOME``) and the parent directory — not every base twice as before."""
    root, cfg, entries = tree
    sample = entries[: DIRS + 60]
    verdicts, calls = _counted(
        monkeypatch, lambda: [policy.check_source_file(p, cfg, explicit=False, item_root=root) for p in sample]
    )
    assert all(v.allowed for v in verdicts)
    assert calls["realpath"] <= 5 * len(sample), calls
    assert calls["open"] == sum(1 for p in sample if p.is_file()), calls
