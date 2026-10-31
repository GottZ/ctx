"""Memory engine of the ``ctx_checkpoint`` Hermes plugin.

This package is the full ctx memory provider — recall before each turn, the agent tools
``ctx_query`` / ``ctx_store`` / ``ctx_search`` / ``ctx_get``, the ``MEMORY.md`` / ``USER.md``
mirror, pre-compress fact extraction with cross-compaction dedupe, per-segment session
digests, the transcript checkpoint chain and the ``hermes ctx_checkpoint …`` CLI.

It was written by derLonius (W4-NERF) as ``hermes-ctx-plugin`` (releases 1.0–1.7.1) and
imported into the ctx repository at 1.7.1; since ctx_checkpoint 0.6.0 it is the engine
behind the ``ctx_checkpoint`` provider (``ctx_checkpoint/__init__.py``), which owns the
plugin name, the setup surfaces, the single ``memory.ctx_checkpoint`` settings section and
the transport choice (Hermes' MCP dispatch by default, REST with an own key on request).

Nothing in here registers with Hermes directly — the umbrella does. The provider class is
exported lazily so that ``cli.py`` (imported by the host at argparse setup time, before any
provider is loaded) pulls only the stdlib-only ``client``/``config`` modules.
"""

from __future__ import annotations

from typing import Any

__all__ = ["CtxMemoryProvider"]


def __getattr__(name: str) -> Any:
    if name == "CtxMemoryProvider":
        from .provider import CtxMemoryProvider

        return CtxMemoryProvider
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
