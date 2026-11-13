"""Child-process body for the store concurrency test (importable by multiprocessing spawn)."""

from __future__ import annotations

import sys
from pathlib import Path

BOT = "900000000000000001"


def bind_many(repo: str, path: str, n: int) -> None:
    sys.path.insert(0, repo)
    from dsp.store import file as f, ops

    store = f.PolicyStore(Path(path))
    for i in range(15):
        guild = f"8{n:02d}{i:02d}00000000000000"
        ops.bind(store, BOT, guild, by=f"p{n}")
        if i % 3 == 0:
            ops.set_state(store, BOT, guild, "paused", by=f"p{n}")
