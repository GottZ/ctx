"""The process-wide runtime anchor (DESIGN §3.1).

Hermes imports a plugin once per profile home (``hermes_plugins.<slug>`` and ``…__home_<digest>``), each
with its own module globals. The entry point therefore loads this package exactly once per process under
the fixed name ``ctx_discord_policy`` (see the plugin's ``__init__.py``); every profile instance
then shares the single ``RUNTIME`` object below: one route ContextVar, one route registry, one armed
register, one status register. A type defined in this package (``Route``) has one identity per process.
"""

from __future__ import annotations

import os
import threading
import time
from collections import OrderedDict
from contextvars import ContextVar
from dataclasses import dataclass, field
from typing import Any, Generic, Hashable, Optional, TypeVar

from . import RUNTIME_API, __version__
from .adapt.status import StatusRegister
from .bots import BotRegistry

ANCHOR_NAME = "ctx_discord_policy"
ROUTE_CAP = 4096

K = TypeVar("K", bound=Hashable)
V = TypeVar("V")


class BoundedMap(Generic[K, V]):
    """Thread-safe LRU map with a hard size cap (route bookkeeping must never grow without bound)."""

    def __init__(self, cap: int = ROUTE_CAP) -> None:
        self._cap = cap
        self._data: "OrderedDict[K, V]" = OrderedDict()
        self._lock = threading.Lock()

    def put(self, key: K, value: V) -> None:
        with self._lock:
            self._data[key] = value
            self._data.move_to_end(key)
            while len(self._data) > self._cap:
                self._data.popitem(last=False)

    def get(self, key: K) -> Optional[V]:
        with self._lock:
            return self._data.get(key)

    def pop(self, key: K) -> Optional[V]:
        with self._lock:
            return self._data.pop(key, None)

    def __contains__(self, key: object) -> bool:
        with self._lock:
            return key in self._data

    def __len__(self) -> int:
        with self._lock:
            return len(self._data)

    def drop_where(self, predicate) -> int:
        with self._lock:
            doomed = [k for k in self._data if predicate(k)]
            for key in doomed:
                del self._data[key]
            return len(doomed)

    def clear(self) -> None:
        with self._lock:
            self._data.clear()


@dataclass
class Instance:
    """One profile's plugin instance that registered against this anchor."""

    home_key: str
    profile: str
    plugin_id: str
    plugin_dir: str
    registered_at: float = field(default_factory=time.time)
    home: str = ""  # the home path itself (for live registry checks); additive


class ArmedRegister:
    """Profile homes whose plugin instance registered every policy hook (DESIGN §3.2).

    Public admission into a profile is only allowed while that profile's home is armed: there the
    ``pre_tool_call`` lane check provably runs for the turn.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._armed: dict[str, Instance] = {}

    def arm(self, instance: Instance) -> None:
        with self._lock:
            self._armed[instance.home_key] = instance

    def disarm(self, home_key: str) -> None:
        with self._lock:
            self._armed.pop(home_key, None)

    def is_armed(self, home_key: Optional[str]) -> bool:
        if not home_key:
            return False
        with self._lock:
            return home_key in self._armed

    def instance(self, home_key: Optional[str]) -> Optional[Instance]:
        with self._lock:
            return self._armed.get(home_key) if home_key else None

    def snapshot(self) -> dict[str, dict[str, Any]]:
        with self._lock:
            return {key: {"profile": inst.profile, "plugin_dir": inst.plugin_dir,
                          "registered_at": inst.registered_at} for key, inst in self._armed.items()}


class Runtime:
    def __init__(self) -> None:
        self.version = __version__
        self.runtime_api = RUNTIME_API
        self.pid = os.getpid()
        self.created_at = time.time()
        self.lock = threading.RLock()
        # The authority route of the turn running in this context (set per runner call, never inherited).
        self.route: ContextVar[Any] = ContextVar("ctx_discord_policy_route", default=None)
        self.armed = ArmedRegister()
        self.status = StatusRegister()
        self.instances: dict[str, Instance] = {}
        # Exact per-message routes: (channel_id, message_id) -> Route. Restored FIFO events find theirs here.
        self.message_routes: BoundedMap[tuple[str, str], Any] = BoundedMap()
        # (session_id, turn_id) -> Route, bound in pre_llm_call for tool calls that lost the ContextVar.
        self.turn_routes: BoundedMap[tuple[str, str], Any] = BoundedMap()
        # Sessions that ever carried a public route: a turn there without an exact route gets no tools.
        self.public_sessions: BoundedMap[str, float] = BoundedMap()
        # PluginContext used for config reads (``ctx.get_config`` under the bot owner's home).
        self.config_ctx: Any = None
        # Discord bots bound through the platform-handler factory (adapter -> application id -> owner home).
        self.bots = BotRegistry()
        # Message ids whose admission widened the allowlist: never handed on unrouted afterwards.
        self.widened: BoundedMap[str, float] = BoundedMap()
        # Hermes session key -> (lane, session user) of the routes that used it: a session never mixes the
        # operator lane with another author's public lane.
        self.session_lanes: BoundedMap[str, tuple[str, str]] = BoundedMap()

    def describe(self) -> dict[str, Any]:
        return {
            "plugin_version": self.version,
            "runtime_api": self.runtime_api,
            "pid": self.pid,
            "anchor_created_at": self.created_at,
            "armed": self.armed.snapshot(),
            "instances": {k: {"profile": v.profile, "plugin_dir": v.plugin_dir}
                          for k, v in self.instances.items()},
            "bots": self.bots.snapshot(),
            **self.status.snapshot(),
        }


RUNTIME = Runtime()


def note_refused(entry: dict[str, Any]) -> None:
    """Stable entry point for an incompatible plugin instance to report itself (any version calls this)."""
    RUNTIME.status.note_refused(entry)
