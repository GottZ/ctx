"""The only module that imports Hermes (layer L2; INTERFACES §3.6, DESIGN §15.1, catalog rule 9).

Every Hermes import is lazy (inside a function) and names only modules of the published plugin API
(``hermes_cli/data/plugin_api.json`` of main): ``hermes_state``, ``hermes_constants``, ``hermes_cli.config``,
``agent.secret_scope``, ``agent.plugin_llm``, ``agent.redact`` (``redact_sensitive_text``),
``plugins.plugin_storage``, ``gateway.platforms.base``, ``gateway.status``, ``providers``, and on a host with the
storage interface ``persistence``, ``persistence.conversation.messages``, ``persistence.conversation.sessions``,
``persistence_assembly`` and ``storage_limits``. A missing module or name degrades to ``CapabilityMissing`` (code
``capability_missing``) instead of ``ImportError``.

Two session stores (``session_db``): Hermes up to v0.23.2 has the ``hermes_state.SessionDB`` class; from v0.23.3 on
sessions sit behind the storage interface (``persistence_assembly.open_persistence``) and ``hermes_state`` is a
compatibility module that carries only part of the old class. On such a host ``session_db`` returns a
``StoreSessionDB``: the ``SessionDB`` methods the engine calls, with their old arguments, return shapes and the
exceptions the engine catches, over one store handle — the engine above this module is the same on both hosts.

Only composition roots (``jobs``, ``commands``, ``dashboard/plugin_api.py``) call this module; they reach the
functions as ``hostcompat.<name>`` (module attribute) so tests can swap them with
``tests/fakes/hostcompat_fake.py``. Callers that start worker threads wrap the work in ``run_in_context`` so
profile-scoped lookups (``plugin_data_dir``, ``get_secret``, ``PluginLlm``) resolve the right profile
(DESIGN §2.1 consequence 6).

Nothing here runs at import time.
"""

from __future__ import annotations

import contextvars
import importlib.metadata
import json
import logging
import os
import queue
import re
import sys
import threading
import weakref
from collections.abc import Callable
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path

from .errors import BadRequest, CapabilityMissing, ContinueRefused, EnvNotWritable, HandoverError
from .model import HermesPaths, HostFacts

log = logging.getLogger("ctx_handover.hostcompat")

PLUGIN_ID = "ctx_handover"

HOST_PROCESS_ENV = "HERMES_PLUGIN_HOST_PROCESS"
"""Set to ``"1"`` by Hermes inside the plugin host of ``plugins.isolation: host``
(``hermes_cli/plugin_isolation.py:97``); read as an environment contract, not imported."""

REQUIRED_SESSIONDB_METHODS: tuple[str, ...] = (
    "create_session",
    "get_session",
    "end_session",
    "set_session_archived",
    "set_session_hidden",
    "acquire_session_turn_lease",
    "refresh_session_turn_lease",
    "release_session_turn_lease",
    "try_acquire_compression_lock",
    "refresh_compression_lock",
    "release_compression_lock",
    "get_compression_lock_holder",
    "import_sessions",
    "export_session_lineage",
    "get_compression_lineage",
    "conversation_revision",
    "get_next_title_in_lineage",
    "set_session_title",
    "set_session_title_source",
    "append_messages_batch",
    "archive_and_compact",
    "rewind_to_message",
    "assert_resume_safe",
    "get_resume_conversations",
    "get_meta",
    "set_meta",
    "list_meta_prefix",
    "delete_session",
)
"""SessionDB methods the plugin needs (DESIGN §15.1 table row "SessionDB"); all present at the floor v0.22.6."""

SESSION_STORE_SESSIONDB = "sessiondb"
SESSION_STORE_PERSISTENCE = "persistence"
"""``HostFeatures.session_store``: the ``hermes_state.SessionDB`` class (Hermes ≤ v0.23.2) or the storage interface
(``persistence_assembly``, Hermes ≥ v0.23.3)."""

REQUIRED_STORE_VERBS: dict[str, tuple[str, ...]] = {
    "sessions": (
        "create",
        "end",
        "reopen",
        "get",
        "set_archived",
        "set_hidden",
        "get_title",
        "get_title_source",
        "set_title",
        "set_title_source",
        "get_by_title",
        "next_title_in_lineage",
        "delete",
        "update_cwd",
        "list_sessions",
    ),
    "lineage": ("lineage", "resolve_resume_target"),
    "messages": (
        "read_transcript",
        "append_batch",
        "compact",
        "conversation_revision",
        "read_resume_conversations",
        "assert_resume_safe",
        "plan_user_turn_rewind",
        "rewind_user_turn",
    ),
    "coordination": (
        "try_acquire_turn_lease",
        "refresh_turn_lease",
        "release_turn_lease",
        "try_acquire_compression_lock",
        "refresh_compression_lock",
        "release_compression_lock",
        "get_compression_lock_holder",
        "list_inflight_delegations",
    ),
    "admissions": ("list",),
    "maintenance": ("import_sessions", "export_session_lineage"),
    "state_meta": ("get", "set", "list_prefix"),
}
"""Repository verbs of the storage interface ``StoreSessionDB`` stands on (``persistence_assembly.PersistenceStore``
names each repository with its protocol); all present from v0.23.3 on."""

_DIST_NAMES = ("hermes-agent", "hermes_agent")


def _missing(what: str) -> CapabilityMissing:
    return CapabilityMissing(
        f"this Hermes installation does not offer {what}, which ctx_handover needs; update Hermes", capability=what
    )


@dataclass(frozen=True)
class HostFeatures:
    """Feature detection by attribute, never by version string (DESIGN §15.1)."""

    side_question: bool
    ask_choice: bool
    post_message: bool
    host_isolated: bool
    session_db_ok: bool
    plugin_llm: bool
    missing: tuple[str, ...]
    session_store: str = SESSION_STORE_SESSIONDB


def _store_protocols() -> dict[str, object] | None:
    """``{repository: protocol class}`` of the host's storage interface, or None on a host without one (no
    ``persistence_assembly``). A repository the store does not declare maps to None."""
    try:
        import inspect

        from persistence_assembly import PersistenceStore
    except ImportError:
        return None
    try:
        hints = inspect.get_annotations(PersistenceStore, eval_str=True)
    except Exception:  # an unresolvable annotation: such a store offers nothing this plugin can rely on
        hints = {}
    return {repo: hints.get(repo) for repo in REQUIRED_STORE_VERBS}


def _session_store_static() -> tuple[str, bool, list[str]]:
    """``(session_store, session_db_ok, missing)``: the storage interface when the host has one, else the
    ``SessionDB`` class."""
    protocols = _store_protocols()
    if protocols is not None:
        absent = [
            f"persistence.{repo}.{verb}"
            for repo, verbs in REQUIRED_STORE_VERBS.items()
            for verb in verbs
            if not callable(getattr(protocols.get(repo), verb, None))
        ]
        return SESSION_STORE_PERSISTENCE, not absent, absent
    try:
        from hermes_state import SessionDB as cls
    except ImportError:
        return SESSION_STORE_SESSIONDB, False, ["hermes_state.SessionDB"]
    absent = [m for m in REQUIRED_SESSIONDB_METHODS if not callable(getattr(cls, m, None))]
    return SESSION_STORE_SESSIONDB, not absent, [f"SessionDB.{m}" for m in absent]


@lru_cache(maxsize=1)
def _static_features() -> tuple[bool, bool, tuple[str, ...], str]:
    session_store, session_db_ok, missing = _session_store_static()
    try:
        from agent.plugin_llm import PluginLlm

        plugin_llm = callable(getattr(PluginLlm, "complete_structured", None))
    except ImportError:
        plugin_llm = False
    if not plugin_llm:
        missing.append("agent.plugin_llm.PluginLlm")
    return session_db_ok, plugin_llm, tuple(missing), session_store


def features(ctx=None) -> HostFeatures:
    """Detect what the host offers. The ctx-independent part (the session store's methods, ``PluginLlm``) is
    cached per process; the ctx part is plain attribute detection. Under host isolation ``ask_choice`` stays
    available and ``post_message`` does not (``hermes_cli/plugin_isolation.py`` HOST_UNSUPPORTED_CTX_METHODS).

    ``session_store`` names the store the host offers: the storage interface wherever ``persistence_assembly``
    exists (``session_db_ok`` then means every verb of ``REQUIRED_STORE_VERBS``), else the ``SessionDB`` class
    (every method of ``REQUIRED_SESSIONDB_METHODS``)."""
    session_db_ok, plugin_llm_ok, missing, session_store = _static_features()
    isolated = os.environ.get(HOST_PROCESS_ENV) == "1"
    side_question = ctx is not None and callable(getattr(ctx, "side_question", None))
    ask_choice = ctx is not None and callable(getattr(ctx, "ask_choice", None))
    post_message = ctx is not None and not isolated and callable(getattr(ctx, "post_message", None))
    return HostFeatures(
        side_question=side_question,
        ask_choice=ask_choice,
        post_message=post_message,
        host_isolated=isolated,
        session_db_ok=session_db_ok,
        plugin_llm=plugin_llm_ok,
        missing=missing,
        session_store=session_store,
    )


def session_db(path: Path | None = None):
    """A ``hermes_state.SessionDB()`` of the active profile (its default path, resolved at call time), or of the
    ``state.db`` at ``path`` (additive: composition roots pin the profile's database once, so worker threads do
    not depend on the environment of the moment). On a host with the storage interface the same, as a
    ``StoreSessionDB`` over ``open_persistence`` of that profile (``path`` must then name a ``state.db``)."""
    try:
        import persistence_assembly  # noqa: F401 - the storage interface decides which store this host has
    except ImportError:
        pass
    else:
        return StoreSessionDB(path)
    try:
        from hermes_state import SessionDB
    except ImportError as exc:
        raise _missing("hermes_state.SessionDB") from exc
    return SessionDB() if path is None else SessionDB(Path(path))


_ADDRESSING_KEYS = frozenset({"_row_id", "_db_row_snapshot", "_restored_from_store", "_canonical_row"})
"""Keys that make ``NewMessage.from_live`` name a stored row, which ``append_batch`` and ``compact`` would repair or
adopt instead of inserting (``NewMessage``): a row the engine writes is always a new row."""


def _new_messages(messages: list[dict]) -> list:
    """The engine's message dicts as the interface's ``NewMessage`` rows, each a fresh insert."""
    from persistence.conversation.messages import NewMessage

    return [NewMessage.from_live({k: v for k, v in m.items() if k not in _ADDRESSING_KEYS}) for m in messages]


def _media_root() -> str:
    """The profile's retained media root a session delete names (``gateway.session_ingress_media.native_inputs_root``
    is not public: the same folder from the public ``gateway.platforms.base.get_document_cache_dir``)."""
    try:
        from gateway.platforms.base import get_document_cache_dir
    except ImportError as exc:
        raise _missing("gateway.platforms.base.get_document_cache_dir") from exc
    return str((Path(get_document_cache_dir()) / "native-inputs").resolve())


_collected_handles: queue.SimpleQueue = queue.SimpleQueue()
_reaper_lock = threading.Lock()
_reaper: threading.Thread | None = None


def _reap_handles() -> None:
    """Release the handles of ``StoreSessionDB`` objects collected unclosed, one by one, holding no lock."""
    while True:
        handle = _collected_handles.get()
        try:
            handle.close()
        except Exception:  # a collected handle has no caller to report to
            log.debug("releasing a collected session store handle failed", exc_info=True)
        del handle


def _ensure_reaper() -> None:
    global _reaper
    with _reaper_lock:
        if _reaper is None or not _reaper.is_alive():
            _reaper = threading.Thread(target=_reap_handles, name="ctx-handover-store-reaper", daemon=True)
            _reaper.start()


class StoreSessionDB:
    """The ``SessionDB`` methods the engine calls, over one handle of the storage interface (Hermes ≥ v0.23.3).

    One handle per instance (``open_persistence(profile_scope(home=…), eager=True)``: an unopenable store raises its
    typed error here); handles on one profile share Hermes' process-wide backend, so a handle per thread costs no
    connection of its own. ``close`` releases the handle; an instance collected unclosed (a worker thread's
    per-thread instance, ``jobs._db_factory``) has it released on a reaper thread, as Hermes' own ``SessionDB``
    stand-in does — a finalizer may run on a thread that holds a lock the release takes, so it only queues the
    handle. Each method takes the old arguments and returns the old
    shape (the session row dict, wire rows, the revision tuple), and raises what the engine catches where the
    interface raises something else: a busy store in a lease call reads as not acquired / not refreshed
    (``sqlite3.OperationalError`` and ``sqlite3.Error`` in ``lock``), a title an import cannot take as
    ``sqlite3.IntegrityError`` naming the title (``session_import._import_a``), a refused rewind as ``ValueError``.
    Reads and writes go through repository verbs only; nothing here opens the database file itself.
    """

    read_only = False

    def __init__(self, path: Path | None = None) -> None:
        try:
            from persistence.conversation.sessions import CANONICAL_BOT_CHAT_TITLE, MAX_TITLE_LENGTH
            from persistence_assembly import open_persistence, profile_scope, store_location
        except ImportError as exc:
            raise _missing("persistence_assembly.open_persistence") from exc
        if path is not None and Path(path).name != "state.db":
            raise ValueError(f"a session store is a profile's state.db, not {str(path)!r}")
        scope = profile_scope() if path is None else profile_scope(home=Path(path).parent)
        self.MAX_TITLE_LENGTH = int(MAX_TITLE_LENGTH)
        self.CANONICAL_BOT_CHAT_TITLE = str(CANONICAL_BOT_CHAT_TITLE)
        self.db_path = store_location(scope)
        _ensure_reaper()
        self._store = open_persistence(scope, eager=True)
        self._finalizer = weakref.finalize(self, _collected_handles.put, self._store)
        self._finalizer.atexit = False

    def close(self) -> None:
        self._finalizer.detach()
        self._store.close()

    def __enter__(self) -> StoreSessionDB:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    # -- sessions ----------------------------------------------------------------------------------------------
    def get_session(self, session_id: str) -> dict | None:
        record = self._store.sessions.get(session_id)
        return None if record is None else record.as_session_row()

    def create_session(self, session_id: str, source: str, **kwargs) -> str:
        from persistence.conversation.sessions import NewSession

        origin_json = kwargs.pop("origin_json", None)
        if origin_json is not None:
            kwargs["origin"] = json.loads(origin_json)
        return self._store.sessions.create(NewSession(session_id=session_id, source=source, **kwargs))

    def end_session(self, session_id: str, end_reason: str) -> None:
        self._store.sessions.end(session_id, end_reason)

    def reopen_session(self, session_id: str) -> None:
        self._store.sessions.reopen(session_id)

    def set_session_archived(self, session_id: str, archived: bool, notify: bool = True) -> bool:
        return bool(self._store.sessions.set_archived(session_id, bool(archived), notify=notify).changed)

    def set_session_hidden(self, session_id: str, hidden: bool) -> bool:
        return bool(self._store.sessions.set_hidden(session_id, bool(hidden)))

    def get_session_title(self, session_id: str) -> str | None:
        return self._store.sessions.get_title(session_id)

    def get_session_title_source(self, session_id: str) -> str | None:
        source = self._store.sessions.get_title_source(session_id)
        return None if source is None else str(source.value)

    def set_session_title(self, session_id: str, title: str) -> bool:
        return bool(self._store.sessions.set_title(session_id, title))

    def set_session_title_source(self, session_id: str, source: str) -> bool:
        from persistence.conversation.sessions import TitleSource

        return bool(self._store.sessions.set_title_source(session_id, TitleSource(source)))

    def get_session_by_title(self, title: str) -> dict | None:
        record = self._store.sessions.get_by_title(title)
        return None if record is None else record.as_session_row()

    def get_next_title_in_lineage(self, base_title: str) -> str:
        return self._store.sessions.next_title_in_lineage(base_title)

    def delete_session(self, session_id: str, include_compression_chain: bool = False) -> bool:
        """The store's delete (lineage, delegate children, ids retired) — without Hermes' ``on_session_delete``
        hook and transcript-file clean-up, which run only in the core's private ``session_retention``; the engine
        deletes only sessions it imported itself and never showed (README, Limits)."""
        removed = self._store.sessions.delete(
            session_id, include_compression_chain=bool(include_compression_chain), media_root=_media_root()
        )
        return bool(removed)

    def update_session_cwd(
        self, session_id: str, cwd: str, git_branch: str | None = None, git_repo_root: str | None = None
    ) -> int | None:
        return self._store.sessions.update_cwd(session_id, cwd, git_branch, git_repo_root)

    def list_sessions_rich(
        self,
        limit: int = 20,
        offset: int = 0,
        order_by_last_active: bool = False,
        include_archived: bool = False,
        search_query: str | None = None,
        min_message_count: int = 0,
    ) -> list[dict]:
        from persistence.conversation.sessions import SessionOrder, SessionQuery, SessionWindow

        query = SessionQuery(
            min_message_count=min_message_count,
            order=SessionOrder.LAST_ACTIVE if order_by_last_active else SessionOrder.STARTED_AT,
            include_archived=include_archived,
            search_text=search_query,
        )
        # as the old class: a negative limit is every row, a negative offset none
        window = SessionWindow(limit=None if limit is not None and limit < 0 else limit, offset=max(offset, 0))
        return [entry.as_list_row() for entry in self._store.sessions.list_sessions(query, window)]

    def resolve_resume_session_id(self, session_id: str) -> str | None:
        return self._store.lineage.resolve_resume_target(session_id)

    def get_compression_lineage(self, session_id: str) -> list[str]:
        return list(self._store.lineage.lineage(session_id))

    # -- messages ----------------------------------------------------------------------------------------------
    def get_messages(
        self,
        session_id: str,
        include_inactive: bool = False,
        include_compacted: bool = False,
        limit: int | None = None,
        offset: int = 0,
        latest: bool = False,
        after_id: int | None = None,
    ) -> list[dict]:
        from persistence.conversation.messages import TranscriptRead, TranscriptView

        if include_inactive:
            view = TranscriptView.ALL
        else:
            view = TranscriptView.DISPLAY if include_compacted else TranscriptView.ACTIVE
        request = TranscriptRead(view=view, limit=limit, offset=offset, latest=latest, after_id=after_id)
        return [row.as_wire_row() for row in self._store.messages.read_transcript(session_id, request)]

    def append_messages_batch(self, session_id: str, messages: list[dict], chunk_rows: int | None = None) -> int:
        """``messages.append_batch`` of the message dicts (``_new_messages``); the number of rows inserted."""
        rows = _new_messages(messages)
        return int(self._store.messages.append_batch(session_id, rows, chunk_rows=chunk_rows).inserted)

    def archive_and_compact(self, session_id: str, messages: list[dict]) -> int:
        """``messages.compact`` unfenced and without a watermark: the active rows archived, ``messages`` live after
        them; the active row count afterwards. Never with a watermark: the store would then let a live row claim an
        archived row with the same ``message_uid`` and mark that one rewound, out of every hash."""
        return int(self._store.messages.compact(session_id, _new_messages(messages)).active_count)

    def conversation_revision(self, session_id: str, include_ancestors: bool = False) -> tuple:
        return self._store.messages.conversation_revision(session_id, include_lineage=include_ancestors).as_tuple()

    def get_resume_conversations(self, session_id: str) -> tuple[list[dict], list[dict]]:
        both = self._store.messages.read_resume_conversations(session_id)
        return [m.as_openai_message() for m in both.model], [m.as_openai_message() for m in both.display]

    def assert_resume_safe(self, session_id: str) -> int:
        from storage_limits import resolved_max_resume_messages

        return self._store.messages.assert_resume_safe(session_id, max_messages=resolved_max_resume_messages())

    def rewind_to_message(self, session_id: str, message_id: int) -> dict:
        """Rewind the active transcript to just before the user row ``message_id`` (it and every later active row
        deactivated); ``{"rewound_count": n}``.

        The interface rewinds by user turn (``rewind_user_turn``): the turn whose durable row is ``message_id`` is
        looked up newest first with ``plan_user_turn_rewind`` (the core's own turn rule), at most once per user row
        from ``message_id`` on, and rewound by its ordinal from the oldest turn. Both calls get the replay projection
        read here as ``warm_history``, so a transcript whose turns changed meanwhile is refused (``RuntimeError``)
        instead of rewinding another turn. ``ValueError`` when no turn starts at ``message_id`` — a user row the
        replay merges into the unanswered user message before it, whose rewind would take that message too; a busy
        store and a refused write raise the store's error, which the caller retries."""
        from persistence import RewindRefused, TransientStoreError
        from persistence.conversation.messages import ConversationRequest

        messages = self._store.messages
        durable = messages.read_conversation(
            session_id, ConversationRequest(repair_alternation=True, include_row_ids=True)
        )
        warm = [m.as_openai_message() for m in durable]
        later_users = sum(
            1 for m in durable if m.role == "user" and m.row_id is not None and int(m.row_id) >= int(message_id)
        )
        plan = None
        for back in range(1, later_users + 1):
            try:
                candidate = messages.plan_user_turn_rewind(session_id, -back, warm_history=warm)
            except (TransientStoreError, RewindRefused):
                raise
            except ValueError:
                break
            if int(candidate.target_row_id) == int(message_id):
                plan = candidate
                break
            if int(candidate.target_row_id) < int(message_id) or candidate.user_ordinal == 0:
                break
        if plan is None:
            raise ValueError(
                f"Hermes counts message {message_id} as part of the unanswered message before it, and removing it "
                "there would remove that message too"
            )
        outcome = messages.rewind_user_turn(session_id, plan.user_ordinal, warm_history=warm)
        return {"rewound_count": int(outcome.rewound_count)}

    # -- leases --------------------------------------------------------------------------------------------------
    def try_acquire_session_turn_lease(
        self, session_id: str, holder: str, ttl_seconds: float = 300.0, patience_s: float | None = None
    ) -> bool:
        import sqlite3

        from persistence import TransientStoreError

        try:
            return bool(
                self._store.coordination.try_acquire_turn_lease(
                    session_id=session_id, holder=holder, ttl_seconds=ttl_seconds, patience_s=patience_s
                )
            )
        except TransientStoreError as exc:
            raise sqlite3.OperationalError(str(exc)) from exc

    def refresh_session_turn_lease(
        self, session_id: str, holder: str, ttl_seconds: float = 300.0, patience_s: float | None = None
    ) -> bool:
        import sqlite3

        from persistence import TransientStoreError

        try:
            return bool(
                self._store.coordination.refresh_turn_lease(
                    session_id=session_id, holder=holder, ttl_seconds=ttl_seconds, patience_s=patience_s
                )
            )
        except TransientStoreError as exc:
            raise sqlite3.OperationalError(str(exc)) from exc

    def release_session_turn_lease(self, session_id: str, holder: str) -> None:
        self._store.coordination.release_turn_lease(session_id=session_id, holder=holder)

    def try_acquire_compression_lock(self, session_id: str, holder: str, ttl_seconds: float = 300.0) -> bool:
        import sqlite3

        from persistence import TransientStoreError

        try:
            return bool(
                self._store.coordination.try_acquire_compression_lock(
                    session_id=session_id, holder=holder, ttl_seconds=ttl_seconds
                )
            )
        except TransientStoreError as exc:
            raise sqlite3.OperationalError(str(exc)) from exc

    def refresh_compression_lock(self, session_id: str, holder: str, ttl_seconds: float = 300.0) -> bool:
        import sqlite3

        from persistence import TransientStoreError

        try:
            return bool(
                self._store.coordination.refresh_compression_lock(
                    session_id=session_id, holder=holder, ttl_seconds=ttl_seconds
                )
            )
        except TransientStoreError as exc:
            raise sqlite3.OperationalError(str(exc)) from exc

    def release_compression_lock(self, session_id: str, holder: str) -> None:
        self._store.coordination.release_compression_lock(session_id=session_id, holder=holder)

    def get_compression_lock_holder(self, session_id: str) -> str | None:
        return self._store.coordination.get_compression_lock_holder(session_id=session_id)

    # -- busy signals (``lock.probe_busy``) ------------------------------------------------------------------
    def busy_signals(self, lineage: list[str]) -> tuple[int, list[dict]]:
        """``(queued prompts for any session of lineage, running delegations)`` through the interface — the store's
        own reads of what ``lock.probe_busy`` reads over SQL on the old host: admissions ``queued`` targeting a
        lineage session, and every ``running`` delegation with ``delegation_id``, ``state``, ``dispatched_at``,
        ``parent_session_id``, ``origin_session_id`` and ``origin_session`` (the caller matches them)."""
        queued = 0
        for session_id in lineage:
            for record in self._store.admissions.list(session_id=session_id, pending_only=True):
                if str(getattr(record.status, "value", record.status)) == "queued":
                    queued += 1
        delegations = [
            {
                "delegation_id": str(d.delegation_id),
                "state": str(d.state),
                "dispatched_at": d.dispatched_at,
                "parent_session_id": d.parent_session_id,
                "origin_session_id": d.origin_session_id,
                "origin_session": d.origin_session,
            }
            for d in self._store.coordination.list_inflight_delegations()
        ]
        return queued, delegations

    # -- state_meta --------------------------------------------------------------------------------------------
    def get_meta(self, key: str) -> str | None:
        return self._store.state_meta.get(key)

    def set_meta(self, key: str, value: str) -> None:
        self._store.state_meta.set(key, value)

    def list_meta_prefix(self, prefix: str) -> list[tuple[str, str]]:
        if not prefix:
            return []
        return [(entry.key, entry.value) for entry in self._store.state_meta.list_prefix(prefix)]

    # -- import and export -------------------------------------------------------------------------------------
    def import_sessions(self, sessions: list[dict]) -> dict:
        """``maintenance.import_sessions`` as the old dict (lists, not tuples). A title another session holds is the
        store's ``PermanentStoreError`` over the ``IntegrityError``; it is raised as the old ``IntegrityError``."""
        import sqlite3

        from persistence import PermanentStoreError

        try:
            result = self._store.maintenance.import_sessions(sessions)
        except PermanentStoreError as exc:
            if isinstance(exc.__cause__, sqlite3.IntegrityError):
                raise sqlite3.IntegrityError(str(exc)) from exc
            raise
        return {
            "ok": bool(result.ok),
            "imported": result.imported,
            "skipped": result.skipped,
            "detached": result.detached,
            "imported_ids": list(result.imported_ids),
            "skipped_ids": list(result.skipped_ids),
            "errors": list(result.errors),
        }

    def export_session_lineage(
        self, session_id: str, include_compacted: bool = False, include_inactive: bool = False
    ) -> dict | None:
        """The old lineage export dict (the tip's row with ``segments``, ``lineage_session_ids``, ``message_count``
        and ``messages``), without ``timings``, which only the core's private export helper computes."""
        export = self._store.maintenance.export_session_lineage(session_id, include_compacted, include_inactive)
        if export is None:
            return None
        segments = [
            {**segment.session.as_dict(), "messages": [m.as_wire_row() for m in segment.messages]}
            for segment in export.segments
        ]
        return {
            **(segments[-1] if segments else {}),
            "segments": segments,
            "lineage_session_ids": list(export.lineage_session_ids),
            "message_count": export.message_count,
            "messages": [m.as_wire_row() for m in export.messages],
        }


def _hermes_home() -> Path:
    try:
        from hermes_constants import get_hermes_home
    except ImportError as exc:
        raise _missing("hermes_constants.get_hermes_home") from exc
    return Path(get_hermes_home())


def state_db_path() -> Path:
    """Path of the active profile's ``state.db`` for read-only best-effort SQL (``lock.probe_busy``, O8)."""
    return _hermes_home() / "state.db"


def hermes_paths() -> HermesPaths:
    """The active Hermes home and the document cache (``gateway.platforms.base.get_document_cache_dir``)."""
    home = _hermes_home()
    try:
        from gateway.platforms.base import get_document_cache_dir
    except ImportError as exc:
        raise _missing("gateway.platforms.base.get_document_cache_dir") from exc
    cache = get_document_cache_dir()
    return HermesPaths(home=str(home), document_cache=str(cache))


HOME_MODE_ENV = "TERMINAL_HOME_MODE"
"""Hermes' home mode of tool subprocesses (``terminal.home_mode``, bridged into this variable by the gateway and the
chat CLI, ``gateway/run.py:1988-2030``); read by ``hermes_constants.get_subprocess_home``."""


def _configured_home_mode() -> dict[str, str]:
    """``{HOME_MODE_ENV: mode}`` from the profile's ``terminal.home_mode`` when the process environment does not set
    it (a process that never bridged its config: a CLI subcommand, the dashboard), else ``{}``. Only a non-empty
    string counts; an unreadable config leaves Hermes' own default."""
    if os.environ.get(HOME_MODE_ENV, "").strip():
        return {}
    try:
        terminal = load_config().get("terminal")
    except Exception:  # no config here: Hermes' default mode applies, as in Hermes itself
        return {}
    mode = terminal.get("home_mode") if isinstance(terminal, dict) else None
    return {HOME_MODE_ENV: mode.strip()} if isinstance(mode, str) and mode.strip() else {}


def effective_home() -> Path:
    """The home ``~`` means for the active profile — from the source Hermes' own file tools expand it with
    (``tools/file_tools_paths.py:_expand_tilde``): ``hermes_constants.get_subprocess_home()``, else the process home.

    Not ``Path.home()``: Hermes applies its subprocess HOME contract (a launch ``HOME`` that is the profile home
    ``<HERMES_HOME>/home`` is repaired to the real home on a host; ``terminal.home_mode: profile`` and containers use
    the profile home) to every child it starts, the plugin host of ``plugins.isolation: host`` included
    (``hermes_cli/plugin_host.py:127-136`` → ``served_profile_child_env``), but not to its own process — so the
    process home of an in-process plugin and of the plugin host differ (L3-F20). This answer is the same in both
    modes and in every process of the profile (R12): ``terminal.home_mode`` comes from the profile config when the
    process did not bridge it into ``TERMINAL_HOME_MODE``; a set variable wins (in the gateway and the chat CLI it
    holds the bridged config value)."""
    try:
        from hermes_constants import get_subprocess_home
    except ImportError as exc:
        raise _missing("hermes_constants.get_subprocess_home") from exc
    home = get_subprocess_home(_configured_home_mode())
    return Path(home) if isinstance(home, str) and home else Path.home()


def _model_section(config: dict) -> tuple[str | None, str | None, int | None]:
    model = config.get("model") if isinstance(config, dict) else None
    if isinstance(model, dict):
        name = model.get("default") or model.get("model") or model.get("name")
        provider = model.get("provider")
        ctx_len = model.get("context_length")
    else:
        name, provider, ctx_len = model, None, None
    name = str(name).strip() if isinstance(name, str) and name.strip() else None
    provider = str(provider).strip() if isinstance(provider, (str, int)) and str(provider).strip() else None
    ctx_len = ctx_len if isinstance(ctx_len, int) and not isinstance(ctx_len, bool) and ctx_len > 0 else None
    return name, provider, ctx_len


def hermes_version() -> str:
    """Installed hermes-agent distribution version (``""`` when unknown; source installs report ``0.0.0``).
    Informational only — features are detected by attribute (DESIGN §15.1)."""
    for dist in _DIST_NAMES:
        try:
            return importlib.metadata.version(dist)
        except importlib.metadata.PackageNotFoundError:
            continue
    return ""


def host_facts() -> HostFacts:
    """Hermes-side facts of this host for ``EnvFacts.host`` (DESIGN §11.1): WSL/container, profile, multiplexing,
    versions, configured model/provider/context length, terminal backend, Hermes home and document cache."""
    from . import PLUGIN_VERSION

    try:
        import hermes_constants
        from agent.secret_scope import is_multiplex_active
    except ImportError as exc:
        raise _missing("hermes_constants / agent.secret_scope") from exc
    home = _hermes_home()
    paths = hermes_paths()
    config = load_config()
    model, provider, ctx_len = _model_section(config)
    terminal = config.get("terminal") if isinstance(config.get("terminal"), dict) else {}
    profile = None
    profile_name_for_home = getattr(hermes_constants, "profile_name_for_home", None)
    if callable(profile_name_for_home):
        profile = profile_name_for_home(home)
    is_wsl = getattr(hermes_constants, "is_wsl", None)
    is_container = getattr(hermes_constants, "is_container", None)
    return HostFacts(
        is_wsl=bool(is_wsl()) if callable(is_wsl) else False,
        is_container=bool(is_container()) if callable(is_container) else False,
        profile=str(profile or "default"),
        multiplex=bool(is_multiplex_active()),
        hermes_version=hermes_version(),
        plugin_version=PLUGIN_VERSION,
        model=model,
        provider=provider,
        context_length=ctx_len,
        terminal_backend=str(terminal.get("backend") or "local"),
        hermes_home=str(home),
        document_cache=paths.document_cache,
    )


def plugin_dir() -> Path:
    """The plugin's data directory ``<HERMES_HOME>/plugin-data/ctx_handover`` (created by Hermes), chmod 0700."""
    try:
        from plugins.plugin_storage import plugin_data_dir
    except ImportError as exc:
        raise _missing("plugins.plugin_storage.plugin_data_dir") from exc
    path = Path(plugin_data_dir(PLUGIN_ID))
    os.chmod(path, 0o700)
    return path


def load_config() -> dict:
    """The active profile's merged configuration (``hermes_cli.config.load_config``: a private deep copy, so a
    caller mutating it cannot corrupt Hermes' cache)."""
    try:
        from hermes_cli.config import load_config as _load
    except ImportError as exc:
        raise _missing("hermes_cli.config.load_config") from exc
    config = _load()
    return config if isinstance(config, dict) else {}


MCP_SERVER_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}")
"""Names ``grant_mcp_server`` accepts (an ``mcp_servers`` key; no wildcard, no path, no whitespace)."""


def _mcp_allowlist_of(config: object) -> list[str]:
    """``plugins.entries.ctx_handover.mcp_allowlist`` as the core reads it (``hermes_cli/plugins_context.py:365-373``:
    a list, anything else counts as empty)."""
    node: object = config
    for key in ("plugins", "entries", PLUGIN_ID, "mcp_allowlist"):
        if not isinstance(node, dict) or key not in node:
            return []
        node = node[key]
    return [str(item) for item in node] if isinstance(node, list) else []


def grant_mcp_server(server: str) -> None:
    """Add ``server`` to ``plugins.entries.ctx_handover.mcp_allowlist`` — the operator grant ``ctx.call_mcp`` checks
    (``hermes_cli/plugins_context.py:322-328``) — and prove the write by reading the merged config back (W108,
    DESIGN §13 "written with the public config writer").

    Call it only from an operator command after the operator agreed (``/handover setup`` on the local surface or
    the CLI, R24); the grant is the operator's, the plugin only writes it down. Writer:
    ``hermes_cli.config.save_config(partial, preserve_keys=…, merge_existing=True)`` — module ``hermes_cli.config``
    is public in ``plugin_api.json``, and the core's own ``save_plugin_setting`` ends in the same call
    (``hermes_cli/plugins_state.py:90``; same signature at the floor v0.22.6). Idempotent: a listed server is not
    written again; other entries of the list and the rest of ``config.yaml`` stay.

    ``BadRequest`` when ``server`` does not match ``MCP_SERVER_RE``. ``CapabilityMissing`` (``details.reason``
    ``unavailable`` | ``managed`` | ``write_failed`` | ``not_written``, ``details.command`` = the ``hermes config
    set`` line the operator can run instead) when the writer is missing, the install is package-managed, the
    write raises, or the read-back does not list the server (an administrator-managed key is stripped on save).
    """
    if not isinstance(server, str) or not MCP_SERVER_RE.fullmatch(server):
        raise BadRequest("invalid MCP server name; use the name of an entry under mcp_servers", field="server")
    key = f"plugins.entries.{PLUGIN_ID}.mcp_allowlist"

    def refused(reason: str, message: str, **details) -> CapabilityMissing:
        try:
            listed = _mcp_allowlist_of(load_config())
        except Exception:
            listed = []
        wanted = listed if server in listed else [*listed, server]
        command = f"hermes config set {key} '{json.dumps(wanted)}'"
        return CapabilityMissing(message, capability="mcp_allowlist", reason=reason, command=command, **details)

    try:
        from hermes_cli.config import is_managed, save_config
    except ImportError as exc:
        raise refused(
            "unavailable",
            "this Hermes version cannot save plugin settings, so the MCP allowlist was not written; set it yourself "
            "with hermes config set",
        ) from exc
    current = _mcp_allowlist_of(load_config())
    if server in current:
        return
    if is_managed():
        raise refused(
            "managed",
            "this Hermes installation is package-managed, so the MCP allowlist was not written; set it yourself with "
            "hermes config set",
        )
    partial = {"plugins": {"entries": {PLUGIN_ID: {"mcp_allowlist": [*current, server]}}}}
    try:
        save_config(partial, preserve_keys={("plugins", "entries", PLUGIN_ID, "mcp_allowlist")}, merge_existing=True)
    except Exception as exc:
        raise refused(
            "write_failed",
            f"the MCP allowlist could not be written ({type(exc).__name__}); set it yourself with hermes config set",
            error=str(exc)[:300],
        ) from exc
    if server not in _mcp_allowlist_of(load_config()):
        raise refused(
            "not_written",
            f"{key} does not list {server!r} after the write (the setting is managed elsewhere or the save was "
            "refused); set it yourself with hermes config set",
        )


def read_secret(name: str) -> str | None:
    """Credential by env-var name through ``agent.secret_scope.get_secret`` (catalog rule 11, profile-scoped).
    A missing secret scope under multiplexing raises ``HandoverError`` instead of the core's exception."""
    try:
        from agent.secret_scope import get_secret
    except ImportError as exc:
        raise _missing("agent.secret_scope.get_secret") from exc
    try:
        value = get_secret(name)
    except HandoverError:
        raise
    except Exception as exc:
        raise HandoverError(
            f"the secret {name} cannot be read in this process ({type(exc).__name__})", secret=name
        ) from exc
    return value if isinstance(value, str) and value != "" else None


def env_writable(name: str) -> None:
    """Raise ``EnvNotWritable`` when the profile ``.env`` write lock forbids setting ``name``
    (``hermes_cli.config.require_env_writable``)."""
    try:
        from hermes_cli.config import require_env_writable
    except ImportError as exc:
        raise _missing("hermes_cli.config.require_env_writable") from exc
    try:
        require_env_writable(name, "set")
    except ValueError as exc:
        raise EnvNotWritable(str(exc)[:500], variable=name) from exc


def save_env(name: str, value: str) -> None:
    """Write ``name=value`` into the profile ``.env`` (``hermes_cli.config.save_env_value``). The core may refuse
    silently; callers prove the write with ``read_env_fresh`` (R16)."""
    try:
        from hermes_cli.config import save_env_value
    except ImportError as exc:
        raise _missing("hermes_cli.config.save_env_value") from exc
    save_env_value(name, value)


def read_env_fresh(name: str) -> str | None:
    """Value of ``name`` preferring the profile ``.env`` over ``os.environ``
    (``hermes_cli.config.get_env_value_prefer_dotenv``) — the identity read-back of R16."""
    try:
        from hermes_cli.config import get_env_value_prefer_dotenv
    except ImportError as exc:
        raise _missing("hermes_cli.config.get_env_value_prefer_dotenv") from exc
    value = get_env_value_prefer_dotenv(name)
    return value if isinstance(value, str) and value != "" else None


def provider_usable(name: str, config: dict) -> bool:
    """Can THIS host serve ``name`` with its own configuration? (DESIGN §6.7, R11, R20)

    Usable when the provider is configured here as a custom ``providers.<name>`` entry, or when
    ``providers.get_provider_profile`` knows it and either one of its credential variables (its ``env_vars``
    without ``*_URL`` entries) is set here (``api_key`` auth) or it is this host's configured default provider
    (other auth types, or no credential variables). A peer's ``base_url`` never takes part in this decision.
    """
    name = str(name or "").strip()
    if not name:
        return False
    custom = config.get("providers") if isinstance(config, dict) else None
    if isinstance(custom, dict) and isinstance(custom.get(name), dict):
        return True
    try:
        from providers import get_provider_profile
    except ImportError as exc:
        raise _missing("providers.get_provider_profile") from exc
    profile = get_provider_profile(name)
    if profile is None:
        return False
    _model, default_provider, _ctx = _model_section(config)
    canonical = str(getattr(profile, "name", name) or name)
    is_default = default_provider in {name, canonical}
    if str(getattr(profile, "auth_type", "api_key")) != "api_key":
        return is_default
    env_vars = tuple(v for v in (getattr(profile, "env_vars", ()) or ()) if not str(v).endswith("_URL"))
    if not env_vars:
        return is_default
    return any(read_secret(var) or read_env_fresh(var) for var in env_vars)


def redact(text: str) -> str:
    """``agent.redact.redact_sensitive_text(text, force=True)``."""
    try:
        from agent.redact import redact_sensitive_text
    except ImportError as exc:
        raise _missing("agent.redact.redact_sensitive_text") from exc
    return redact_sensitive_text(text, force=True)


def plugin_llm() -> Callable[[str, list, dict, int], dict | None] | None:
    """Adapter over ``agent.plugin_llm.PluginLlm(plugin_id="ctx_handover").complete_structured`` for the listener
    and CLI process (DESIGN §8.3, R9), or None when unavailable.

    Call shape: ``llm(instructions, input_blocks, json_schema, timeout_s)`` → the parsed JSON object when the
    reply is one, else None. The result is NOT validated (no ``jsonschema`` in the managed runtime, LAB L7) —
    ``refine.validate`` does that. Trust and provider errors propagate so the caller can report them.
    """
    try:
        from agent.plugin_llm import PluginLlm
    except ImportError:
        return None

    def call(instructions: str, input_blocks: list, json_schema: dict, timeout_s: int) -> dict | None:
        result = PluginLlm(plugin_id=PLUGIN_ID).complete_structured(
            instructions=instructions,
            input=input_blocks,
            json_schema=json_schema,
            timeout=float(timeout_s),
            purpose="ctx_handover.refine",
        )
        parsed = getattr(result, "parsed", None)
        return parsed if isinstance(parsed, dict) else None

    return call


def run_in_context(fn: Callable, *a, **kw):
    """Run ``fn`` in a copy of the current ``contextvars`` context (profile scope travels with it)."""
    return contextvars.copy_context().run(fn, *a, **kw)


def hermes_root() -> Path:
    """The Hermes root of all profiles (``hermes_constants.get_default_hermes_root``)."""
    try:
        from hermes_constants import get_default_hermes_root
    except ImportError as exc:
        raise _missing("hermes_constants.get_default_hermes_root") from exc
    return Path(get_default_hermes_root())


def _approval_mode(value: object) -> str:
    if isinstance(value, bool):
        return "off" if value is False else "manual"
    if isinstance(value, str) and value.strip().lower() in {"manual", "smart", "off"}:
        return value.strip().lower()
    return "manual"


YOLO_ENV = "HERMES_YOLO_MODE"
"""Hermes' process-wide approval bypass: ``--yolo`` writes it into the environment before any import
(``hermes_cli/main.py:2762-2768``), ``tools/approval.py:48`` freezes it at import and skips every approval with it
(``:539-545``). The plugin host of ``plugins.isolation: host`` inherits it (``served_profile_child_env``)."""

_TRUTHY = frozenset({"1", "true", "yes", "on"})
"""``utils.TRUTHY_STRINGS`` (``utils.is_truthy_value`` is not in ``plugin_api.json``); a K0 test keeps it equal."""


def _yolo_process() -> bool:
    """``HERMES_YOLO_MODE`` of this process, read as Hermes reads it (``utils.is_truthy_value``: stripped,
    case-insensitive, one of ``_TRUTHY``)."""
    value = os.environ.get(YOLO_ENV)
    return isinstance(value, str) and value.strip().lower() in _TRUTHY


YOLO_FLAG = "--yolo"
"""The command-line form of ``HERMES_YOLO_MODE`` (``hermes_cli/_parser.py``: an inherited top-level option, so it can
stand before or after ``gateway run``). argparse also takes an unambiguous prefix (``--yo``, ``--yol``)."""

PLUGIN_HOST_ENV = "HERMES_PLUGIN_HOST_PROCESS"
"""Set to ``1`` in the plugin host Hermes starts under ``plugins.isolation: host`` (``hermes_cli/plugin_isolation.py``,
``HOST_PROCESS_ENV``)."""

APPROVAL_WHY = {
    "config_mode": True,
    "config_single_query": True,
    "process_env": True,
    "gateway_env": True,
    "gateway_flag": True,
    "gateway_process": False,
    "no_gateway": False,
    "gateway_elsewhere": None,
    "gateway_unverified": None,
    "gateway_unknown": None,
}
"""``ApprovalCheck.why`` → ``ApprovalCheck.off`` (review H2, R22, R33a)."""

UNIDENTIFIED_WHY = frozenset({"gateway_unverified", "gateway_unknown"})
"""``ApprovalCheck.why`` values where this process cannot identify the gateway that runs the turn (another container,
PID namespace or user, or Hermes' gateway status unreadable): no continuation starts from here (review R3-H2,
fail-closed) — ``approvals_off`` raises ``ContinueRefused`` with reason ``approvals_unchecked``."""


@dataclass(frozen=True)
class ApprovalCheck:
    """What this process can tell about approvals for an unattended continuation turn (review H2, R22, R33a).

    ``off``: True — approvals are off (the continuation must not start); False — they apply, checked; None — not
    checkable from this process (a gateway runs elsewhere and its environment is not visible here).
    ``why`` (``APPROVAL_WHY``): ``config_mode`` / ``config_single_query`` (``approvals.*`` of this profile),
    ``process_env`` (``HERMES_YOLO_MODE`` in this process, which is not the gateway), ``gateway_env`` (this process
    is the gateway or its plugin host and runs with ``HERMES_YOLO_MODE``), ``gateway_flag`` (the profile's running
    gateway, another process, was started with ``--yolo``), ``gateway_process`` (this process is the gateway or its
    plugin host, without the variable), ``no_gateway`` (no gateway of this profile runs: the continuation's
    ``hermes chat`` starts one from its own, cleaned environment), ``gateway_elsewhere`` (a gateway runs in another
    process without ``--yolo`` on its command line — whether its environment holds ``HERMES_YOLO_MODE`` is not
    visible here), ``gateway_unverified`` (a fresh runtime record names a running gateway this process cannot
    identify — another container or PID namespace, another user — and its recorded command line has no ``--yolo``),
    ``gateway_unknown`` (Hermes' gateway status could not be read); the last two are ``UNIDENTIFIED_WHY``.
    ``pid`` is the gateway's process id where one was identified; ``flag`` whether ``--yolo`` is on the gateway's
    recorded command line (other processes of the profile see that, not the environment) — for an unidentified
    gateway the command line of its fresh record counts (``gateway_flag`` with ``pid`` None, review R3-H2)."""

    off: bool | None
    why: str
    pid: int | None = None
    flag: bool = False


def _yolo_flag(argv: object) -> bool:
    """``--yolo`` (or an unambiguous argparse prefix of it) among ``argv``."""
    if not isinstance(argv, list | tuple):
        return False
    return any(isinstance(a, str) and len(a) >= 4 and YOLO_FLAG.startswith(a) for a in argv)


def _gateway_view() -> tuple[str, int | None, bool]:
    """``(relation, pid, flag)`` of the gateway that runs this profile's turns, through Hermes' own identity check
    (``gateway.status.live_gateway_pid_for_home``: pid file and runtime record, start-time guard, live command line;
    it never writes) for this profile's home, then the Hermes root (one host gateway may serve every profile) — the
    ``gateway.pid`` and ``gateway_state.json`` of both are read (README, Reads).

    ``relation``: ``self`` (this process is that gateway, or the plugin host it started — its environment is this
    process's), ``elsewhere``, ``none`` (no record, or a stopped or stale one), ``unverified`` (a fresh record of a
    running gateway whose process cannot be identified here: another container or PID namespace, another user — the
    first home with one decides, so a profile's own gateway in another container is never hidden behind an
    identified root gateway), ``unknown`` (``gateway.status`` failed). ``flag``: ``--yolo`` on the recorded command
    line of the gateway — of an identified one when the record is its own, of an unverified one from its fresh record
    (review R3-H2: that gateway's own ``argv`` is the one sign of it this process has)."""
    try:
        from gateway.status import live_gateway_pid_for_home, read_runtime_status, runtime_status_is_stale
    except ImportError:
        return "unknown", None, False
    try:
        homes: list[Path] = []
        for home in (Path(hermes_paths().home), hermes_root()):
            if all(os.path.realpath(home) != os.path.realpath(h) for h in homes):
                homes.append(home)
        for home in homes:
            pid = live_gateway_pid_for_home(home)
            record = read_runtime_status(home / "gateway_state.json")
            if pid is not None:
                own = pid == os.getpid() or (pid == os.getppid() and os.environ.get(PLUGIN_HOST_ENV) == "1")
                argv = record.get("argv") if isinstance(record, dict) and record.get("pid") == pid else None
                return ("self" if own else "elsewhere"), pid, _yolo_flag(argv)
            if (
                isinstance(record, dict)
                and record.get("gateway_state") not in (None, "stopped", "startup_failed")
                and not runtime_status_is_stale(record)
            ):
                return "unverified", None, _yolo_flag(record.get("argv"))
        return "none", None, False
    except Exception:
        return "unknown", None, False


def approval_check(config: dict) -> ApprovalCheck:
    """``ApprovalCheck`` for a continuation started from this process (review H2, R22, R33a; see the class).

    Config first (``approvals.mode`` off — YAML ``off`` arrives as False — or an approving
    ``approvals.single_query_mode``: ``approve``/``off``/``allow``/``yes``, mirroring
    ``tools/approval_context.py:266-364``), then ``HERMES_YOLO_MODE`` of this process (Hermes' truth rule), then the
    gateway that runs the turn: ``hermes chat --resume … -q`` is a gateway client (``hermes_cli/gateway_agent.py:1-6``,
    ``hermes_cli/main.py`` ``_chat_runs_at_gateway``), so that gateway's ``--yolo`` bypasses approvals whatever the
    child's environment says. Another process — a separate ``hermes serve``/``hermes dashboard`` serving the routes,
    or ``hermes handover take|import --continue`` in a terminal — sees the recorded command line (its ``--yolo``) of a
    gateway it can identify, but not its environment; a gateway it cannot identify (another container or PID
    namespace: Hermes' ``docker-compose.yml`` runs gateway and dashboard apart) is ``gateway_unverified`` — unless
    its fresh record's command line carries ``--yolo`` (``gateway_flag``, ``pid`` None)."""
    approvals = config.get("approvals") if isinstance(config, dict) else None
    if isinstance(approvals, dict):
        if _approval_mode(approvals.get("mode", "manual")) == "off":
            return ApprovalCheck(True, "config_mode")
        single = str(approvals.get("single_query_mode", "deny")).lower().strip()
        if single in {"approve", "off", "allow", "yes"}:
            return ApprovalCheck(True, "config_single_query")
    relation, pid, flag = _gateway_view()
    if relation == "self":
        flag = flag or _yolo_flag(list(sys.argv))
    if _yolo_process():
        return ApprovalCheck(True, "gateway_env" if relation == "self" else "process_env", pid, flag)
    if relation == "self":
        return ApprovalCheck(False, "gateway_process", pid, flag)
    if relation == "elsewhere":
        return ApprovalCheck(True, "gateway_flag", pid, True) if flag else ApprovalCheck(None, "gateway_elsewhere", pid)
    if relation == "none":
        return ApprovalCheck(False, "no_gateway")
    if relation == "unverified":
        return ApprovalCheck(True, "gateway_flag", None, True) if flag else ApprovalCheck(None, "gateway_unverified")
    return ApprovalCheck(None, "gateway_unknown")


def unchecked_text(why: str) -> str:
    """Why a continuation from this process is refused although approvals were not found off
    (``UNIDENTIFIED_WHY``; the ``ContinueRefused`` message and the ``doctor`` reason, review R3-H2)."""
    if why == "gateway_unverified":
        return (
            "the Hermes gateway of this profile (the service that runs your chats) is running, but this process "
            "cannot identify it (it runs in another container or PID namespace, or as another user), so it cannot see "
            "whether approvals are switched off there"
        )
    return (
        "the status of the Hermes gateway (the service that runs your chats) could not be read here, so this process "
        "cannot tell whether approvals apply"
    )


def approvals_off(config: dict) -> bool:
    """The spawner's gate (``continuation.Spawner``; R22, R33a, review R3-H2, R3-L4), from ``approval_check``:

    * True — approvals are off as far as this process can tell (the continuation is refused, ``approvals_off``);
    * raises ``ContinueRefused`` (``details.reason`` ``approvals_unchecked``, ``details.why`` from
      ``UNIDENTIFIED_WHY``) — this process cannot identify the gateway that runs the turn, so nothing starts
      (fail-closed);
    * False — the turn may start. Where the gateway is another, identified process (``gateway_elsewhere``) its
      environment is not visible (R33a, named in the README and by ``doctor``): the start is logged as a warning
      that names that gateway, so a continuation never starts unchecked in silence."""
    check = approval_check(config)
    if check.off is True:
        return True
    if check.why in UNIDENTIFIED_WHY:
        raise ContinueRefused(
            f"No turn is started without a person present: {unchecked_text(check.why)}.",
            reason="approvals_unchecked",
            why=check.why,
        )
    if check.off is None:
        log.warning(
            "ctx_handover: starting a continuation from this process with approvals not checked: the gateway of this "
            "profile (pid %s) is another process, and whether it runs with HERMES_YOLO_MODE in its environment is "
            "not visible here; leave allow_remote_continue off on a gateway given HERMES_YOLO_MODE",
            check.pid,
        )
    return False
