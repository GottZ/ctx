"""ctx_handover engine package (INTERFACES §1).

The engine is a library that runs identically in the gateway, ``hermes serve``, the CLI and the plugin host
(R12). It is imported in two ways: as ``<plugin package>.engine`` from ``register()``, and by path as a
standalone package from ``dashboard/plugin_api.py``. Therefore every engine module imports only its siblings,
relatively, and this file imports nothing from the engine and does no I/O at import time.

``PLUGIN_VERSION`` is read lazily from ``../VERSION`` (the single version source, BX L9) on first access.
"""

from __future__ import annotations

from pathlib import Path

WIRE_MAJOR = 1
"""Path major of the wire protocol (``/v1``, DESIGN §4.3)."""

WIRE_MINOR = 0
"""Minor of the v1 wire protocol: which optional fields this side understands."""

PROTOCOL = "ctxh"
"""``hello.protocol`` value."""

CAPABILITIES_V1 = frozenset(
    {
        "session.v1",
        "blocks.v1",
        "enc.zlib",
        "enc.zstd",
        "git.bundle.v1",
        "files.v1",
        "hermesfiles.v1",
        "offers.v1",
        "delta.v1",
        "continue.v1",
        "refine.llm",
        "refine.side_question",
        "operator.v1",
    }
)
"""Every capability name of wire v1 (DESIGN §4.3); a side announces the subset it supports."""

_VERSION_FILE = Path(__file__).resolve().parent.parent / "VERSION"
_cache: dict[str, str] = {}


def _read_version() -> str:
    try:
        text = _VERSION_FILE.read_text(encoding="utf-8").strip()
    except OSError:
        return "0.0.0"
    return text or "0.0.0"


def __getattr__(name: str) -> str:
    if name == "PLUGIN_VERSION":
        if name not in _cache:
            _cache[name] = _read_version()
        return _cache[name]
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
