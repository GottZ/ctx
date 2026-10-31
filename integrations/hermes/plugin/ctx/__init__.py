"""CTX memory provider plugin for Hermes Agent.

Wraps GottZ/ctx (github.com/GottZ/ctx), a self-hosted knowledge store with an
HTTP API, as a MemoryProvider. Registered via the official memory plugin
discovery system — activate with ``memory.provider: ctx`` (or
``hermes memory setup``).
"""

from __future__ import annotations

from .provider import CtxMemoryProvider

__all__ = ["CtxMemoryProvider", "register"]


def register(ctx) -> None:
    """Called by the memory plugin discovery system."""
    ctx.register_memory_provider(CtxMemoryProvider())
