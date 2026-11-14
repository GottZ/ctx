"""Test doubles shared by the ctx_handover suites.

One module per cluster surface (INTERFACES §6): ``hostcompat_fake`` and ``sessiondb`` belong to K0, every
other ``<cluster>_fake.py`` to its cluster. Small helpers used everywhere live here.
"""

from __future__ import annotations

from datetime import datetime, timezone


class FakeClock:
    """A settable wall clock (seconds since the epoch). Call it or use ``now()``; ``advance()`` moves it."""

    def __init__(self, start: float = 1_762_646_400.0) -> None:  # 2025-11-09T00:00:00Z
        self.t = float(start)

    def __call__(self) -> float:
        return self.t

    def now(self) -> float:
        return self.t

    def advance(self, seconds: float) -> float:
        self.t += float(seconds)
        return self.t

    def datetime(self) -> datetime:
        return datetime.fromtimestamp(self.t, tz=timezone.utc)
