"""The running Hermes' session store for tests, on both store interfaces (layer K0 test fixture).

``open_session_db(path)`` opens the store at ``path`` (a profile's ``state.db``) exactly as the engine does —
``engine.hostcompat.session_db``, captured at import so a ``FakeHost`` installed over the module never answers —
and returns it ready for the test builders: ``hermes_state.SessionDB`` on Hermes up to v0.23.2, unchanged; on a host
with the storage interface (v0.23.3 and later) ``hostcompat.StoreSessionDB`` wrapped in ``StoreTestDB``.

``as_test_db(db)`` gives any store the engine opened (``hostcompat.session_db``, the integration harness's
``Home.db()``) the same wrapper. ``StoreTestDB`` forwards every engine method to the ``StoreSessionDB`` it wraps and
adds the old ``SessionDB`` methods only tests call — the write paths the builders use to put real core state into a
store (``append_message``, ``publish_compression_child``, ``archive_and_compact`` with a watermark …) — each over the
repository verb the core itself writes that state with, never SQL. The engine never sees these methods.
"""

from __future__ import annotations

import json
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

from ctx_handover.engine import hostcompat

_REAL_SESSION_DB = hostcompat.session_db
"""The real ``hostcompat.session_db``, taken before any test can install a fake over the module."""


def is_store_host() -> bool:
    """Whether the running Hermes has the storage interface (``persistence_assembly``)."""
    return hostcompat._session_store_static()[0] == hostcompat.SESSION_STORE_PERSISTENCE


def open_session_db(path: str | Path | None = None):
    """The session store at ``path`` (default: the active profile's) as the engine opens it, test-ready."""
    return as_test_db(_REAL_SESSION_DB(None if path is None else Path(path)))


def as_test_db(db):
    """``db`` with the test-only write paths: a ``StoreSessionDB`` wrapped in ``StoreTestDB``, anything else as it
    is (a ``hermes_state.SessionDB`` has them itself)."""
    if isinstance(db, hostcompat.StoreSessionDB):
        return StoreTestDB(db)
    return db


class StoreTestDB:
    """A ``hostcompat.StoreSessionDB`` plus the ``SessionDB`` methods only tests call (module docstring)."""

    def __init__(self, db: hostcompat.StoreSessionDB) -> None:
        self.engine_db = db

    def __getattr__(self, name: str) -> Any:
        return getattr(self.engine_db, name)

    @property
    def store(self):
        """The storage-interface handle under the engine's adapter."""
        return self.engine_db._store

    def close(self) -> None:
        self.engine_db.close()

    def __enter__(self) -> StoreTestDB:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    # -- single rows and reads ----------------------------------------------------------------------------------
    def append_message(
        self,
        session_id: str,
        role: str,
        content: Any = None,
        *,
        turn_lease_holder: str | None = None,
        turn_lease_ttl_seconds: float = 300.0,
        compression_lock_holder: str | None = None,
        **fields: Any,
    ) -> int:
        """``messages.append`` of one message dict (``NewMessage.from_live``), as Hermes' turn loop appends;
        ``compression_lock_holder`` is accepted and not passed on (the store's append takes none, as the core's
        own ``SessionDB`` stand-in documents)."""
        from persistence.conversation.messages import NewMessage

        del compression_lock_holder
        message = {"role": role, "content": content, **fields}
        return int(
            self.store.messages.append(
                session_id,
                NewMessage.from_live(message),
                turn_lease_holder=turn_lease_holder,
                turn_lease_ttl_seconds=turn_lease_ttl_seconds,
            )
        )

    def get_messages_as_conversation(
        self,
        session_id: str,
        include_ancestors: bool = False,
        include_inactive: bool = False,
        repair_alternation: bool = False,
        include_row_ids: bool = False,
        include_compacted: bool = False,
    ) -> list[dict]:
        from persistence.conversation.messages import ConversationRequest, TranscriptView

        if include_inactive:
            view = TranscriptView.ALL
        else:
            view = TranscriptView.DISPLAY if include_compacted else TranscriptView.ACTIVE
        request = ConversationRequest(
            view=view,
            include_lineage=include_ancestors,
            repair_alternation=repair_alternation,
            include_row_ids=include_row_ids,
        )
        return [m.as_openai_message() for m in self.store.messages.read_conversation(session_id, request)]

    def get_active_message_watermark(self, session_id: str) -> int | None:
        last = self.store.messages.active_watermark(session_id).last_active
        return None if last is None else int(last)

    def session_turn_lease_expires_at(self, session_id: str, holder: str) -> float | None:
        return self.store.coordination.turn_lease_expires_at(session_id=session_id, holder=holder)

    def set_user_message_content(
        self, session_id: str, message_id: int, content: Any, display_metadata: Mapping | None = None
    ) -> bool:
        return bool(
            self.store.messages.set_user_message_content(
                session_id, message_id, content, dict(display_metadata) if display_metadata is not None else None
            )
        )

    # -- session columns ----------------------------------------------------------------------------------------
    def update_system_prompt(self, session_id: str, system_prompt: str | None) -> None:
        self.store.sessions.update_system_prompt(session_id, system_prompt)

    def set_session_pinned(self, session_id: str, pinned: bool) -> bool:
        return bool(self.store.sessions.set_pinned(session_id, bool(pinned)))


def _json_object(value: Any) -> dict | None:
    if isinstance(value, Mapping):
        return dict(value)
    if isinstance(value, str) and value:
        try:
            parsed = json.loads(value)
        except ValueError:
            return None
        return parsed if isinstance(parsed, dict) else None
    return None


def _rows(messages: Sequence[Mapping]) -> list:
    from persistence.conversation.messages import NewMessage

    return [NewMessage.from_live(dict(m)) for m in messages]
