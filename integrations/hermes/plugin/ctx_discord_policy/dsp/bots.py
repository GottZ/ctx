"""Bound Discord bots: which adapter instance runs which application, and which home owns its state.

A bot is bound when Hermes calls the plugin's platform-handler factory for a connected Discord adapter
(after ``ready``, in the scope of the profile that runs that bot). Until then every message of that
adapter keeps stock behaviour.
"""

from __future__ import annotations

import threading
import time
import weakref
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Optional

from . import hermes
from .store.file import PolicyStore


@dataclass
class BotBinding:
    bot_id: str
    owner_home: Path
    owner_key: str
    store: PolicyStore
    bound_at: float = field(default_factory=time.time)


class BotRegistry:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._by_adapter: "weakref.WeakKeyDictionary[Any, BotBinding]" = weakref.WeakKeyDictionary()
        self._by_bot: dict[str, BotBinding] = {}

    def bind(self, adapter: Any, bot_id: str, owner_home: Path) -> BotBinding:
        with self._lock:
            existing = self._by_bot.get(bot_id)
            if existing is not None and existing.owner_key == hermes.home_key(owner_home):
                binding = existing
            else:
                binding = BotBinding(bot_id=bot_id, owner_home=Path(owner_home),
                                     owner_key=hermes.home_key(owner_home),
                                     store=PolicyStore(hermes.plugin_data_dir(owner_home) / "policy.json"))
                self._by_bot[bot_id] = binding
            self._by_adapter[adapter] = binding
            return binding

    def for_adapter(self, adapter: Any) -> Optional[BotBinding]:
        with self._lock:
            try:
                return self._by_adapter.get(adapter)
            except TypeError:  # unhashable / not weak-referenceable test doubles
                return None

    def for_bot(self, bot_id: str) -> Optional[BotBinding]:
        with self._lock:
            return self._by_bot.get(str(bot_id))

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            return {b.bot_id: {"owner_home": str(b.owner_home), "store": str(b.store.path),
                               "bound_at": b.bound_at} for b in self._by_bot.values()}
