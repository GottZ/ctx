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


def agent_store_kwargs(db) -> dict:
    """The store keywords a Hermes ``AIAgent`` takes on this host: ``persistence`` (the storage interface handle under
    a ``StoreSessionDB``: the agent writes through the interface) or ``session_db`` (the ``SessionDB`` itself)."""
    db = as_test_db(db)
    return {"persistence": db.store} if isinstance(db, StoreTestDB) else {"session_db": db}


def agent_store_attrs(db) -> dict:
    """The store attributes of an agent stand-in the core's turn helpers read (``_session_db`` and, on a storage
    interface host, ``persistence``)."""
    db = as_test_db(db)
    if isinstance(db, StoreTestDB):
        return {"_session_db": db.store, "persistence": db.store}
    return {"_session_db": db}


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

    def rewind_to_message(self, session_id: str, message_id: int, **kw: Any) -> dict:
        """The engine's rewind (``StoreSessionDB.rewind_to_message``); the old keywords have no store counterpart."""
        if kw:
            raise TypeError(f"the storage interface rewinds by turn; unsupported keywords {sorted(kw)}")
        return self.engine_db.rewind_to_message(session_id, message_id)

    # -- compaction (the compressor's own writes) ---------------------------------------------------------------
    def archive_and_compact(
        self,
        session_id: str,
        compacted_messages: list[dict],
        model_config_patch: dict | None = None,
        watermark: int | None = None,
        lock_holder: str | None = None,
        tail_count: int = 0,
        covered_ids: list[int] | None = None,
    ) -> int:
        """``messages.compact`` with the compressor's fences, as Hermes' in-place compaction writes it."""
        from persistence.conversation.messages import HeldCoverage, Watermark

        result = self.store.messages.compact(
            session_id,
            _rows(compacted_messages),
            watermark=None if watermark is None else Watermark(int(watermark)),
            carried_tail_count=int(tail_count),
            lock_holder=lock_holder,
            model_config_patch=model_config_patch,
            coverage=None if covered_ids is None else HeldCoverage(covered_ids=tuple(int(i) for i in covered_ids)),
        )
        return int(result.active_count)

    def publish_compression_child(
        self,
        *,
        parent_session_id: str,
        child_session_id: str,
        source: str,
        messages: list[dict],
        model: str | None = None,
        model_config: dict | None = None,
        system_prompt: str | None = None,
        cwd: str | None = None,
        profile_name: str | None = None,
        compression_lock_holder: str | None = None,
        require_compression_lease: bool = True,
        require_lease_refresh: bool = False,
        lease_ttl_seconds: float = 300.0,
        watermark: int | None = None,
        watermark_ceiling: int | None = None,
    ) -> None:
        """``lineage.publish_compression_child``, as Hermes' rotating compaction publishes the continuation."""
        from persistence.conversation.lineage import CompressionChild

        self.store.lineage.publish_compression_child(
            parent_session_id,
            CompressionChild(
                session_id=child_session_id,
                source=source,
                model=model,
                model_config=_json_object(model_config),
                system_prompt=system_prompt,
                cwd=cwd,
                profile_name=profile_name,
            ),
            _rows(messages),
            compression_lock_holder=compression_lock_holder,
            require_compression_lease=require_compression_lease,
            require_lease_refresh=require_lease_refresh,
            lease_ttl_seconds=lease_ttl_seconds,
            watermark=watermark,
            watermark_ceiling=watermark_ceiling,
        )

    # -- session columns ----------------------------------------------------------------------------------------
    def update_system_prompt(self, session_id: str, system_prompt: str | None) -> None:
        self.store.sessions.update_system_prompt(session_id, system_prompt)

    def set_session_pinned(self, session_id: str, pinned: bool) -> bool:
        return bool(self.store.sessions.set_pinned(session_id, bool(pinned)))

    def update_session_model(
        self,
        session_id: str,
        model: str,
        provider: str | None = None,
        *,
        base_url: str | None = None,
        api_mode: str | None = None,
    ) -> None:
        self.store.sessions.update_model(session_id, model, provider, base_url=base_url, api_mode=api_mode)

    def update_token_counts(
        self,
        session_id: str,
        input_tokens: int = 0,
        output_tokens: int = 0,
        model: str | None = None,
        cache_read_tokens: int = 0,
        cache_write_tokens: int = 0,
        reasoning_tokens: int = 0,
        estimated_cost_usd: float | None = None,
        actual_cost_usd: float | None = None,
        cost_status: str | None = None,
        cost_source: str | None = None,
        pricing_version: str | None = None,
        billing_provider: str | None = None,
        billing_base_url: str | None = None,
        billing_mode: str | None = None,
        api_call_count: int = 0,
    ) -> None:
        """One API call's accounting through the usage writer (``usage.queue_usage``), flushed before returning,
        as the turn loop records it; the increments of the old method (``absolute=False``)."""
        from persistence.conversation.usage import BillingRoute, TokenCounts, UsageDelta

        route = None
        if billing_provider or billing_base_url or billing_mode or model:
            route = BillingRoute(
                model=model,
                billing_provider=billing_provider,
                billing_base_url=billing_base_url,
                billing_mode=billing_mode,
            )
        delta = UsageDelta(
            tokens=TokenCounts(
                input_tokens=input_tokens,
                output_tokens=output_tokens,
                cache_read_tokens=cache_read_tokens,
                cache_write_tokens=cache_write_tokens,
                reasoning_tokens=reasoning_tokens,
            ),
            api_call_count=api_call_count,
            estimated_cost_usd=estimated_cost_usd,
            actual_cost_usd=actual_cost_usd,
            cost_status=cost_status,
            cost_source=cost_source,
            pricing_version=pricing_version,
            route=route,
        )
        self.store.usage.queue_usage(session_id, delta)
        if not self.store.usage.flush_usage():
            raise TimeoutError("the usage writer did not apply the delta")

    def _write_sql(self, sql: str, params: Sequence = ()) -> None:
        """Raw SQL on the store's file — only for ``test_legacy_rows``, which turns rows into rows of an older
        Hermes build (the old class's private ``_write_sql`` did the same in its own connection)."""
        import sqlite3

        conn = sqlite3.connect(str(self.engine_db.db_path), timeout=30, isolation_level=None)
        try:
            conn.execute(sql, tuple(params))
        finally:
            conn.close()

    def archive_stale_sessions(self, idle_days: float, *, exclude_pinned: bool = True) -> int:
        """The idle sweep: every candidate lineage archived as the sweep archives it (``auto_archive_lineage``)."""
        maintenance = self.store.maintenance
        candidates = maintenance.list_idle_archive_candidates(idle_days, exclude_pinned=exclude_pinned)
        return sum(1 for sid in candidates if maintenance.auto_archive_lineage(sid))


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
