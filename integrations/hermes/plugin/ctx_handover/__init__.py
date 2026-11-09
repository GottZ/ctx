"""ctx_handover — Hermes plugin entry point.

Layer-K0 placeholder: registers nothing yet and has no side effects (R12 — ``register()`` runs in every
Hermes process, including ``hermes plugins validate``'s capability probe). Cluster F replaces this file
with the real registrations (INTERFACES §4.6); it must stay import-safe and I/O-free.
"""

from __future__ import annotations


def register(ctx) -> None:
    """Hermes plugin hook. Placeholder: no registrations, no I/O, no threads."""
    return None
