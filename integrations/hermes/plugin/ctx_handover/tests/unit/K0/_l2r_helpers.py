"""Programs for ``test_l2r_basetemp.py`` (V32). Never collected by a directory run (the file name does not match
``test_*.py``); the test starts them in separate pytest processes by node id.

``test_hold_basetemp`` writes a marker into its ``tmp_path``, announces it (``L2R_READY``) and waits for the release
file (``L2R_RELEASE``, at most 120 s); it passes only when its marker survived — i.e. no second run wiped the base
temp directory meanwhile. ``test_touch_basetemp`` only uses ``tmp_path``. Without the variables both just pass."""

from __future__ import annotations

import os
import time
from pathlib import Path


def test_hold_basetemp(tmp_path):
    marker = tmp_path / "marker.txt"
    marker.write_text("first run", encoding="utf-8")
    ready, release = os.environ.get("L2R_READY"), os.environ.get("L2R_RELEASE")
    if not ready or not release:
        return
    Path(ready).write_text(str(tmp_path), encoding="utf-8")
    deadline = time.monotonic() + 120
    while not Path(release).exists() and time.monotonic() < deadline:
        time.sleep(0.05)
    assert marker.exists(), "the base temp directory of this run was removed by another run"


def test_touch_basetemp(tmp_path):
    (tmp_path / "second.txt").write_text("second run", encoding="utf-8")
