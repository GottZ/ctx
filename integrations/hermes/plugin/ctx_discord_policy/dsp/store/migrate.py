"""Schema migrations, one step ``n -> n+1`` at a time, each after a backup copy of the file."""

from __future__ import annotations

from typing import Any, Callable

# MIGRATIONS[n] turns a schema-n document into a schema-(n+1) document. v1 is the first schema.
MIGRATIONS: dict[int, Callable[[dict[str, Any]], dict[str, Any]]] = {}


def upgrade(document: dict[str, Any], version: int, *, backup: Callable[[int], Any],
            chain: dict[int, Callable[[dict[str, Any]], dict[str, Any]]] | None = None,
            target: int | None = None) -> dict[str, Any]:
    from .schema import SCHEMA_VERSION

    steps = MIGRATIONS if chain is None else chain
    goal = SCHEMA_VERSION if target is None else target
    backup(version)
    current = dict(document)
    while version < goal:
        step = steps.get(version)
        if step is None:
            raise ValueError(f"no migration from schema {version}")
        current = step(current)
        version += 1
        current["schema"] = version
    return current
