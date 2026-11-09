"""Layer R (red) for V32 of ``reports/L2-contracts.md``: the root ``tests/conftest.py`` locks a ``--basetemp``
against two concurrent runs and stops the second one with a clear message (flake cause found by wp-c: a second run
on the same base temp directory removes the first run's files while it is still running).

Two real pytest processes on the same ``--basetemp``: the first holds its ``tmp_path`` (``_l2r_helpers.py``), the
second starts meanwhile. Expected: the second run fails and names the base temp directory; the first run's files
survive and it passes."""

from __future__ import annotations

import os
import subprocess
import sys
import time
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parents[3]
HELPERS = Path(__file__).with_name("_l2r_helpers.py")


def _pytest(node: str, basetemp: Path, env: dict, **kw) -> subprocess.Popen:
    argv = [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider", f"--basetemp={basetemp}", node]
    return subprocess.Popen(  # noqa: S603 - fixed argv: this interpreter running pytest on a file of this tree
        argv, cwd=str(PLUGIN_ROOT), env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, **kw
    )


def test_second_run_on_the_same_basetemp_is_refused_while_the_first_runs(tmp_path):
    basetemp = tmp_path / "shared-basetemp"
    ready, release = tmp_path / "ready", tmp_path / "release"
    env = {**os.environ, "PYTHONDONTWRITEBYTECODE": "1", "L2R_READY": str(ready), "L2R_RELEASE": str(release)}
    first = _pytest(f"{HELPERS}::test_hold_basetemp", basetemp, env)
    try:
        deadline = time.monotonic() + 120
        while not ready.exists() and first.poll() is None and time.monotonic() < deadline:
            time.sleep(0.05)
        assert ready.exists(), f"the first run did not start: {first.communicate(timeout=60)[0].decode()[-2000:]}"
        second = _pytest(f"{HELPERS}::test_touch_basetemp", basetemp, env)
        out2 = second.communicate(timeout=180)[0].decode(errors="replace")
    finally:
        release.write_text("go", encoding="utf-8")
        out1 = first.communicate(timeout=180)[0].decode(errors="replace")
    assert second.returncode != 0, f"a second run on a base temp directory in use must be refused:\n{out2[-2000:]}"
    assert "basetemp" in out2.lower(), f"the refusal must name the base temp directory:\n{out2[-2000:]}"
    assert first.returncode == 0, f"the first run lost its files to the second run:\n{out1[-2000:]}"


def test_runs_one_after_the_other_on_the_same_basetemp_both_pass(tmp_path):
    """The lock is released with the run: a later run on the same base temp directory is not refused."""
    basetemp = tmp_path / "shared-basetemp"
    env = {**os.environ, "PYTHONDONTWRITEBYTECODE": "1"}
    for node in ("test_touch_basetemp", "test_hold_basetemp"):
        proc = _pytest(f"{HELPERS}::{node}", basetemp, env)
        out = proc.communicate(timeout=180)[0].decode(errors="replace")
        assert proc.returncode == 0, out[-2000:]
