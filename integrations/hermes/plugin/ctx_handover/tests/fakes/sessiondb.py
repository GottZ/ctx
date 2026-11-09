"""Builder for real ``hermes_state.SessionDB`` lineages (layer K0 test fixture, TESTPLAN §1).

Every row, flag and lineage edge is written by a public ``SessionDB`` method on the call path the
Hermes core itself uses — no SQL, no import flags, no private ``SessionDB`` methods — so a test sees
exactly the state a real Hermes install would hold [SK §4.2]. Line references point into the upstream
tree NousResearch/hermes-agent ``main`` ``0f0e96fc8e`` (read-only copy at
``/compose/n8n/.gotmp/handover/upstream``).

Row kinds the builder produces (DESIGN §6.8, §6.9):

* live rows — ``active=1, compacted=0``;
* archived rows — ``active=0, compacted=1``: in-place compaction summarized them away
  (``_ARCHIVE_ACTIVE_SQL``, ``hermes_state_messages.py:74``);
* rewind rows — ``active=0, compacted=0``: a rewind took them back (``hermes_state_mutation_transcript.py:27-28``)
  or a compaction superseded them by a carried copy (``hermes_state_messages.py:874-883``);
* clones — rows sharing a ``message_uid`` with another row: carried compaction tails, concurrent-tail
  clones (``_clone_message_rows`` keeps every payload column, ``hermes_state_messages.py:838-852``) and
  ``/branch`` copies (``message_identity``, ``agent/message_metadata.py:264-275``).

Timestamps come from the injected clock (strictly increasing, never moving the clock itself); session
ids use the core's minting function with the clock's time. Message content is unique per builder, so the
core's replay/display dedupe never folds two builder rows by accident.

Session-wide templates (contract K-6): ``large_segment`` on a builder with ``templates`` (the ``make_session``
fixture passes the session's ``tests.conftest.SegmentTemplates``) builds each shape once per test session into a
template ``state.db`` — through exactly the core paths above, by a :meth:`SessionBuilder.twin` of the builder —
and the template store copies that database into the test's empty one. This module stays free of SQL; the copy
lives with the store. Ineligible calls build directly.
"""

from __future__ import annotations

import json
import os
import threading
import time
import uuid
from collections.abc import Callable, Iterable, Mapping, Sequence
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any

__all__ = [
    "BATCH_ROWS",
    "BRANCH_COPY_KEYS",
    "BuiltLineage",
    "RowCounts",
    "SessionBuilder",
    "large_segment",
]

#: Chunk size of every batch append — the core's own value for large copies
#: (``hermes_cli/cli_commands_mixin.py:963``, DESIGN §6.8 step 2).
BATCH_ROWS = 500

#: Message fields ``/branch`` copies verbatim (``_BRANCH_COPY_KEYS``, ``hermes_cli/cli_commands_mixin.py:121-123``);
#: kept in sync by ``test_branch_copy_keys_match_upstream``.
BRANCH_COPY_KEYS = (
    "content",
    "tool_calls",
    "tool_call_id",
    "reasoning",
    "reasoning_content",
    "reasoning_details",
    "codex_reasoning_items",
    "codex_message_items",
    "timestamp",
    "data_use",
)

# Storage coordinates a carried copy must not take along: the core inserts it as a fresh live row.
_CARRY_DROP = frozenset({"id", "session_id", "active", "compacted"})


@dataclass(frozen=True)
class BuiltLineage:
    """A compression lineage as the core reports it (``get_compression_lineage``).

    ``segment_ids`` runs root → tip; ``revision`` is ``conversation_revision(tip, include_ancestors=True)``
    at the time of the call (``hermes_state_messages.py:1541-1552``).
    """

    root_id: str
    tip_id: str
    segment_ids: tuple[str, ...]
    revision: tuple

    def __len__(self) -> int:
        return len(self.segment_ids)


@dataclass(frozen=True)
class RowCounts:
    """Row counts of one segment by flag kind (all rows in storage, read through ``get_messages``)."""

    rows: int
    active: int
    archived: int
    rewind: int


class SessionBuilder:
    """Builds sessions and lineages in a real ``SessionDB`` through the core's own write paths.

    ``db`` is a ``hermes_state.SessionDB``; ``clock`` any zero-argument callable returning epoch seconds
    (``tests.fakes.FakeClock``), ``None`` = wall clock. ``tick`` is the minimum spacing between two
    message timestamps when the clock stands still.
    """

    #: Agent tag inside the compression-lock holder string (shape of ``_compression_lock_holder``,
    #: ``agent/conversation_compression.py:1686-1689``; no pid namespace token ⇒ TTL-only reclaim).
    HOLDER_AGENT = "ctxh-test-builder"

    def __init__(
        self,
        db: Any,
        *,
        clock: Callable[[], float] | None = None,
        tick: float = 0.001,
        templates: Any = None,
    ) -> None:
        if tick <= 0:
            raise ValueError("tick must be positive")
        self.db = db
        self.clock = clock
        self.tick = float(tick)
        self.templates = templates
        self._last_ts: float | None = None
        self._seq = 0

    # ── builder state (templates, contract K-6) ───────────────────────────────────────────────────

    def counters(self) -> tuple[float | None, int]:
        """``(last stamp, sequence)`` — everything a build changes on the builder besides the database."""
        return self._last_ts, self._seq

    def adopt_counters(self, last_ts: float | None, seq: int) -> None:
        """Continue from ``counters()`` of another builder (after its rows were copied into this database)."""
        self._last_ts, self._seq = last_ts, int(seq)

    def twin(self, db: Any) -> SessionBuilder:
        """A builder over ``db`` that produces exactly what this one would now: a clock frozen at this clock's
        time (the builder never moves its clock), the same tick and counters, no templates."""
        from ctx_handover.tests.fakes import FakeClock

        twin = SessionBuilder(db, clock=FakeClock(self.now()), tick=self.tick)
        twin.adopt_counters(*self.counters())
        return twin

    # ── time, ids, content ────────────────────────────────────────────────────────────────────────

    def now(self) -> float:
        """Current time of the injected clock (wall clock without one)."""
        return float(self.clock()) if self.clock is not None else time.time()

    def stamp(self) -> float:
        """Next message timestamp: the clock's time, pushed forward by ``tick`` so stamps strictly increase."""
        t = self.now()
        if self._last_ts is not None and t <= self._last_ts:
            t = self._last_ts + self.tick
        self._last_ts = t
        return t

    def mint_id(self) -> str:
        """A session id in the core's ``YYYYMMDD_HHMMSS_<6 hex>`` shape, timestamped by the clock.

        Core path: ``hermes_state_ids.new_session_id`` (``hermes_state_ids.py:55-59``), the function the CLI,
        the compressor (``agent/conversation_compression.py:45,3257``) and ``/branch`` mint with.
        """
        from hermes_state_ids import new_session_id

        return new_session_id(datetime.fromtimestamp(self.now(), tz=timezone.utc))

    def next_seq(self) -> int:
        """Builder-wide sequence number (keeps generated content and tool-call ids unique)."""
        self._seq += 1
        return self._seq

    @staticmethod
    def text(label: str, size: int) -> str:
        """``label`` padded with ``x`` to ``size`` characters (never shorter than the label, no edge spaces)."""
        return label if len(label) >= size else label + " " + "x" * max(0, size - len(label) - 1)

    @staticmethod
    def tool_call(name: str, arguments: Mapping[str, Any] | None = None, *, call_id: str) -> dict[str, Any]:
        """One OpenAI-shaped tool call as the core stores it in ``messages.tool_calls``."""
        return {
            "id": call_id,
            "type": "function",
            "function": {"name": name, "arguments": json.dumps(dict(arguments or {}), sort_keys=True)},
        }

    def _holder(self) -> str:
        return f"pid={os.getpid()}:tid={threading.get_ident()}:agent={self.HOLDER_AGENT}:nonce={uuid.uuid4().hex[:8]}"

    # ── sessions ──────────────────────────────────────────────────────────────────────────────────

    def create(
        self,
        *,
        title: str | None = None,
        model: str | None = "test-model",
        provider: str | None = None,
        cwd: str | None = None,
        git_repo_root: str | None = None,
        source: str = "cli",
        system_prompt: str | None = None,
        session_id: str | None = None,
        git_branch: str | None = None,
        base_url: str | None = None,
        api_mode: str | None = None,
        model_config: Mapping[str, Any] | None = None,
    ) -> str:
        """Create a session row and return its id (minted by :meth:`mint_id` unless ``session_id`` is given).

        Core paths: ``create_session`` (``hermes_state_sessions.py:418-421``); a ``provider`` writes the whole
        route in both shapes resume reads, as ``/model`` does — ``update_session_model``
        (``hermes_state_sessions.py:719-748``; ``base_url``/``api_mode`` only with a provider); ``git_branch``
        via ``update_session_cwd`` (``hermes_state_sessions.py:569-600``, needs ``cwd``); ``title`` via
        ``set_session_title`` (user provenance, ``hermes_state_titles.py:181-184``; a taken title raises the
        core's ``ValueError``).
        """
        if (base_url or api_mode) and not provider:
            raise ValueError("base_url/api_mode are part of a provider route: pass provider too")
        if git_branch and not cwd:
            raise ValueError("git_branch is recorded together with cwd")
        sid = session_id or self.mint_id()
        self.db.create_session(
            sid,
            source,
            model=model,
            model_config=dict(model_config) if model_config else None,
            system_prompt=system_prompt,
            cwd=cwd,
            git_repo_root=git_repo_root,
        )
        if provider:
            self.db.update_session_model(sid, model, provider, base_url=base_url, api_mode=api_mode)
        if git_branch:
            self.db.update_session_cwd(sid, cwd, git_branch=git_branch, git_repo_root=git_repo_root)
        if title is not None:
            self.db.set_session_title(sid, title)
        return sid

    # ── single messages (the turn-loop path) ──────────────────────────────────────────────────────

    def _append_one(self, sid: str, role: str, content: Any, fields: dict[str, Any]) -> int:
        fields.setdefault("timestamp", self.stamp())
        return int(self.db.append_message(sid, role, content, **fields))

    def user(self, sid: str, text: Any, **fields: Any) -> int:
        """Append one user message; returns its row id.

        Core path: ``append_message`` (``hermes_state_messages.py:397-429``). ``fields`` are further
        ``append_message`` keywords (``api_content``, ``display_kind``, ``display_metadata``, …).
        """
        return self._append_one(sid, "user", text, fields)

    def assistant(
        self,
        sid: str,
        text: Any,
        *,
        tool_calls: Sequence[Mapping[str, Any]] | None = None,
        reasoning: str | None = None,
        **fields: Any,
    ) -> int:
        """Append one assistant message (optionally with tool calls and reasoning); returns its row id.

        Core path: ``append_message`` (``hermes_state_messages.py:397-429``); build calls with :meth:`tool_call`.
        """
        if tool_calls is not None:
            fields["tool_calls"] = [dict(call) for call in tool_calls]
        if reasoning is not None:
            fields["reasoning"] = reasoning
        return self._append_one(sid, "assistant", text, fields)

    def tool(self, sid: str, tool_call_id: str, content: Any, *, name: str, **fields: Any) -> int:
        """Append one tool result answering ``tool_call_id``; returns its row id.

        Core path: ``append_message`` (``hermes_state_messages.py:397-429``) with ``tool_name``/``tool_call_id``.
        """
        fields["tool_call_id"] = tool_call_id
        fields["tool_name"] = name
        return self._append_one(sid, "tool", content, fields)

    # ── batches (the flush/branch/import path) ────────────────────────────────────────────────────

    def message(self, role: str, content: Any, **fields: Any) -> dict[str, Any]:
        """A message dict for :meth:`append`, stamped with the next timestamp unless one is given."""
        fields.setdefault("timestamp", self.stamp())
        return {"role": role, "content": content, **fields}

    def plain_messages(self, n: int, *, size: int = 64, first_role: str = "user", label: str = "row") -> list[dict]:
        """``n`` alternating user/assistant message dicts with unique ``size``-character content."""
        if first_role not in ("user", "assistant"):
            raise ValueError("first_role must be 'user' or 'assistant'")
        roles = ("user", "assistant") if first_role == "user" else ("assistant", "user")
        out = []
        for i in range(n):
            k = self.next_seq()
            out.append(self.message(roles[i % 2], self.text(f"{label} {k}", size)))
        return out

    def turn_messages(self, n: int, *, size: int = 64, with_tools: bool = False) -> list[dict[str, Any]]:
        """Message dicts for ``n`` rounds: user → assistant, or user → assistant(tool call) → tool → assistant."""
        out: list[dict[str, Any]] = []
        for _ in range(n):
            k = self.next_seq()
            out.append(self.message("user", self.text(f"user {k}", size)))
            if with_tools:
                call_id = f"call_{k:06d}"
                call = self.tool_call("read_file", {"path": f"/work/file_{k}.txt"}, call_id=call_id)
                out.append(self.message("assistant", "", tool_calls=[call], reasoning=f"need file {k}"))
                out.append(
                    self.message("tool", self.text(f"result {k}", size), tool_call_id=call_id, tool_name="read_file")
                )
            out.append(self.message("assistant", self.text(f"assistant {k}", size)))
        return out

    def append(self, sid: str, messages: Iterable[Mapping[str, Any]]) -> list[int]:
        """Append message dicts in storage order; returns the new row ids as the core reports them.

        Core path: ``append_messages_batch(…, chunk_rows=500)`` (``hermes_state_messages.py:431-450``), the
        writer ``/branch`` and the B′ import use. Ids are read back with ``get_messages(include_inactive=True,
        after_id=…)`` (``hermes_state_messages.py:1385-1436``) above the newest row before the write
        (``get_messages(…, latest=True, limit=1)``; ``latest_message_row_id`` is role-filtered and does not
        fit). The core's transcript repair may merge or drop malformed rows; the returned ids are what it
        actually stored.
        """
        rows = [dict(m) for m in messages]
        if not rows:
            return []
        before = self._newest_row_id(sid)
        self.db.append_messages_batch(sid, rows, chunk_rows=BATCH_ROWS)
        return [int(m["id"]) for m in self.db.get_messages(sid, include_inactive=True, after_id=before)]

    def turns(self, sid: str, n: int, *, size: int = 64, with_tools: bool = False) -> list[int]:
        """Append ``n`` user/assistant rounds (with a tool call and result each when ``with_tools``) in one batch.

        Returns the new row ids. Core path: :meth:`append` (``append_messages_batch``).
        """
        return self.append(sid, self.turn_messages(n, size=size, with_tools=with_tools))

    # ── compaction, rotation, rewind, branch ─────────────────────────────────────────────────────

    def summary_message(self, text: str | None = None, *, role: str = "user") -> dict[str, Any]:
        """A compaction summary row: the compressor's reference prefix plus ``_compressed_summary``.

        Shape of ``agent/context_compressor.py:5553-5557`` (``SUMMARY_PREFIX`` from ``:259``). The real
        compressor picks the role by alternation (``:5326-5340``); the core stores whatever it is given.
        """
        from agent.context_compressor import SUMMARY_PREFIX

        body = text if text is not None else f"summary {self.next_seq()}"
        return self.message(role, f"{SUMMARY_PREFIX}\n{body}", _compressed_summary=True)

    def concurrent_messages(self, concurrent: int | Iterable[Mapping[str, Any]]) -> list[dict[str, Any]]:
        """Rows that land while a compaction summary is being generated: ``int`` ⇒ that many user/assistant
        rows (user first, as a message typed during the summary), else the given dicts."""
        if isinstance(concurrent, int):
            return self.plain_messages(concurrent, size=32, label="concurrent")
        return [dict(m) for m in concurrent]

    @staticmethod
    def carried_copies(held: Sequence[Mapping[str, Any]], keep_last: int) -> list[dict[str, Any]]:
        """Verbatim copies of the last ``keep_last`` held rows (same ``message_uid``, same timestamp)."""
        if keep_last < 0:
            raise ValueError("keep_last must be >= 0")
        tail = held[len(held) - keep_last :] if keep_last else []
        return [{k: v for k, v in m.items() if k not in _CARRY_DROP} for m in tail]

    def rotate(
        self,
        sid: str,
        *,
        keep_last: int = 2,
        summary: str | None = None,
        summary_role: str = "user",
        concurrent: int | Iterable[Mapping[str, Any]] = 0,
        child_id: str | None = None,
        model: str | None = None,
        system_prompt: str | None = None,
        cwd: str | None = None,
        carry_title: bool = True,
        ttl: float = 300.0,
    ) -> str:
        """Compression rotation as the agent commits it; returns the new tip id (``parent_session_id == sid``).

        Call order of the rotating compressor (``agent/conversation_compression.py``):
        1. durable lease: ``try_acquire_compression_lock`` + start watermark ``get_active_message_watermark``
           (``:2628-2660``; ``hermes_state_compression.py:651-674``, ``hermes_state_messages.py:825-831``);
        2. ``concurrent`` rows are appended to the parent now — they arrive during the slow summary;
        3. foreign-tail ceiling = ``get_active_message_watermark`` (``:3241-3246``);
        4. ``publish_compression_child`` with lease holder, refresh, ``watermark`` and ``watermark_ceiling``
           (``:3256-3270``; ``hermes_state_compression.py:373-400``): parent ended ``compression``, child row,
           handoff (summary + the last ``keep_last`` held rows as carried copies) and the concurrent tail cloned
           into the child — one transaction. The child keeps the parent's source (``:3212-3221``) and route;
           ``model``/``system_prompt``/``cwd`` default to the parent's;
        5. title carried to the child with its provenance (``:3184-3209``);
        6. lease released holder-qualified (``:2567-2583``, ``hermes_state_compression.py:676-683``).

        Not carried: ``/goal``/``/heartbeat``/``/loop`` state (``carry_compression_automation``, agent-side).
        """
        db = self.db
        parent = db.get_session(sid)
        if parent is None:
            raise KeyError(sid)
        holder = self._holder()
        if not db.try_acquire_compression_lock(sid, holder, ttl_seconds=ttl):
            raise RuntimeError(f"compression lock of {sid} is held by {db.get_compression_lock_holder(sid)!r}")
        try:
            watermark = db.get_active_message_watermark(sid)
            held = db.get_messages(sid)
            extra = self.concurrent_messages(concurrent)
            if extra:
                db.append_messages_batch(sid, extra, chunk_rows=BATCH_ROWS)
            ceiling = db.get_active_message_watermark(sid)
            handoff = [self.summary_message(summary, role=summary_role), *self.carried_copies(held, keep_last)]
            old_title = db.get_session_title(sid)
            title_source = db.get_session_title_source(sid)
            child = child_id or self.mint_id()
            db.publish_compression_child(
                parent_session_id=sid,
                child_session_id=child,
                source=parent.get("source") or "cli",
                messages=handoff,
                model=model if model is not None else parent.get("model"),
                model_config=_json_object(parent.get("model_config")),
                system_prompt=system_prompt if system_prompt is not None else parent.get("system_prompt"),
                cwd=cwd,
                profile_name=None,
                compression_lock_holder=holder,
                require_compression_lease=True,
                require_lease_refresh=True,
                lease_ttl_seconds=ttl,
                watermark=watermark,
                watermark_ceiling=ceiling,
            )
            if carry_title and old_title:
                db.set_session_title(child, old_title)
                if title_source:
                    db.set_session_title_source(child, title_source)
        finally:
            db.release_compression_lock(sid, holder)
        return child

    def compact_in_place(
        self,
        sid: str,
        *,
        keep_last: int,
        summary: str | None = None,
        summary_role: str = "user",
        concurrent: int | Iterable[Mapping[str, Any]] = 0,
        coverage: bool = True,
        system_prompt: str | None = None,
        ttl: float = 300.0,
        observe: bool = True,
    ) -> dict[str, Any]:
        """In-place compaction under the same session id, as the agent commits it (Hermes default config).

        Call order (``agent/conversation_compression.py:3586-3646``): lease + start watermark as in
        :meth:`rotate`; the held rows are the live rows at that point and their ids are the proved coverage
        (``coverage_for_commit`` names the held ``_row_id``s, ``agent/conversation_compression_archive.py:101-136``;
        ``coverage=False`` keeps the watermark-only path); ``concurrent`` rows land after the watermark; then
        ``archive_and_compact(sid, [summary, *carried tail], watermark=…, lock_holder=…, tail_count=…,
        covered_ids=…)`` (``:3627-3631``; ``hermes_state_messages.py:908-951``). Effect, observed and returned:

        * held rows except the carried tail → archived (``active=0, compacted=1``);
        * originals of the ``keep_last`` carried rows → rewind rows; their live copies share ``message_uid``;
        * concurrent rows → rewind rows, cloned after the new set with the same ``message_uid``
          (``_archive_named_rows``, ``hermes_state_messages.py:853-906``).

        ``system_prompt`` is stored afterwards with ``update_system_prompt`` (``:3646``;
        ``hermes_state_sessions.py:697-705``).

        Returns ``{"session_id", "active"`` (the core's return value = live count), ``"watermark"``,
        ``"covered_ids"``, ``"concurrent_ids"`` (rows appended after the watermark), and — from comparing the
        segment's rows before and after the commit, i.e. what the core did — ``"archived_ids"``,
        ``"rewound_ids"``, ``"inserted_ids"``, ``"summary_id"``, ``"archived"``, ``"rewound"``}``.
        ``observe=False`` skips the two full reads (the id lists are then empty, ``summary_id`` is ``None``).
        """
        db = self.db
        if db.get_session(sid) is None:
            raise KeyError(sid)
        holder = self._holder()
        if not db.try_acquire_compression_lock(sid, holder, ttl_seconds=ttl):
            raise RuntimeError(f"compression lock of {sid} is held by {db.get_compression_lock_holder(sid)!r}")
        try:
            watermark = db.get_active_message_watermark(sid)
            held = db.get_messages(sid)
            covered = tuple(int(m["id"]) for m in held) if coverage else None
            extra = self.concurrent_messages(concurrent)
            concurrent_ids: list[int] = self.append(sid, extra) if extra else []
            tail = self.carried_copies(held, keep_last)
            before = self._flags(sid) if observe else {}
            active = db.archive_and_compact(
                sid,
                [self.summary_message(summary, role=summary_role), *tail],
                watermark=watermark,
                lock_holder=holder,
                tail_count=len(tail),
                covered_ids=list(covered) if covered is not None else None,
            )
        finally:
            db.release_compression_lock(sid, holder)
        if system_prompt is not None:
            db.update_system_prompt(sid, system_prompt)
        after = self._flags(sid) if observe else {}
        inserted = [i for i in after if i not in before]
        summary_id = next((i for i in inserted if after[i][2]), None)
        archived_ids = [i for i, f in before.items() if f[0] and after[i][:2] == (0, 1)]
        rewound_ids = [i for i, f in before.items() if f[0] and after[i][:2] == (0, 0)]
        return {
            "session_id": sid,
            "active": int(active),
            "watermark": int(watermark),
            "covered_ids": list(covered) if covered is not None else None,
            "summary_id": summary_id,
            "archived": len(archived_ids),
            "rewound": len(rewound_ids),
            "archived_ids": archived_ids,
            "rewound_ids": rewound_ids,
            "inserted_ids": inserted,
            "concurrent_ids": concurrent_ids,
        }

    def rewind(self, sid: str, message_id: int, **kw: Any) -> dict[str, Any]:
        """Rewind to the user message ``message_id`` (``/rewind``, ``/undo``); returns the core's result dict.

        Core path: ``rewind_to_message`` (``hermes_state_messages.py:1855-1882``) → ``rewind_in_transaction``
        (``hermes_state_mutation_transcript.py:5-41``): every live row with ``id >= message_id`` becomes a rewind
        row (``active=0``, ``compacted`` stays ``0``). ``kw`` passes ``preserve_compaction_handoff``,
        ``expected_active_ids``, ``expected_target_content`` through. The target must be a live user row.
        """
        return self.db.rewind_to_message(sid, message_id, **kw)

    def clone(
        self,
        sid: str,
        *,
        title: str | None = None,
        session_id: str | None = None,
        end_parent: bool = True,
        model: str | None = None,
        source: str = "cli",
        model_config: Mapping[str, Any] | None = None,
    ) -> str:
        """``/branch`` of ``sid`` — an independent copy whose rows keep the original ``message_uid``s.

        Core path: the CLI's ``/branch`` (``hermes_cli/cli_commands_mixin.py:915-965``), which runs only on
        public ``SessionDB`` methods: title = ``get_next_title_in_lineage`` (``hermes_state_titles.py:287-289``)
        unless given; ``create_session`` with ``parent_session_id`` and the ``_branched_from`` marker; parent
        ``end_session(…, "branched")`` (``hermes_cli/cli_session_switch.py:11-19``; ``end_parent=False`` skips
        it); the model history a CLI resume holds (``get_resume_conversations``,
        ``hermes_cli/cli_agent_setup_mixin.py:851``) copied with ``BRANCH_COPY_KEYS`` + ``message_identity``
        through ``append_messages_batch(chunk_rows=500)``; ``set_session_title``. The copy is its own
        conversation: ``get_compression_lineage(copy) == [copy]``.

        The gateway's ``session.mutate branch`` (``hermes_state_mutation_branch.py:52-104``) clones with
        ``_clone_message_rows`` instead; it needs the gateway's local-route policy rows and is not modelled.
        """
        from agent.message_metadata import message_identity
        from agent.turn_context import extract_api_content_sidecar

        db = self.db
        parent = db.get_session(sid)
        if parent is None:
            raise KeyError(sid)
        child = session_id or self.mint_id()
        branch_title = title or db.get_next_title_in_lineage(db.get_session_title(sid) or "branch")
        history = db.get_resume_conversations(sid)[0]
        db.create_session(
            session_id=child,
            source=source,
            model=model if model is not None else parent.get("model"),
            parent_session_id=sid,
            system_prompt=parent.get("system_prompt") or None,
            model_config={**dict(model_config or {}), "_branched_from": sid},
        )
        if end_parent:
            db.end_session(sid, "branched")
        rows = [
            {
                "role": m.get("role", "user"),
                "tool_name": m.get("tool_name") or m.get("name"),
                "api_content": extract_api_content_sidecar(m),
                **{k: m.get(k) for k in BRANCH_COPY_KEYS},
                **message_identity(m, with_tool_uids=True),
            }
            for m in history
        ]
        if rows:
            db.append_messages_batch(child, rows, chunk_rows=BATCH_ROWS)
        db.set_session_title(child, branch_title)
        return child

    # ── reads ─────────────────────────────────────────────────────────────────────────────────────

    def lineage(self, tip_id: str) -> BuiltLineage:
        """The compression lineage containing ``tip_id`` (any segment id works for the root).

        Core path: ``get_compression_lineage`` (``hermes_state_compression.py:916-946``, root → tip; an
        explicit ``/branch`` copy is its own one-segment lineage) and ``conversation_revision``
        (``hermes_state_messages.py:1541-1552``).
        """
        segments = tuple(self.db.get_compression_lineage(tip_id))
        if not segments:
            raise KeyError(tip_id)
        tip = segments[-1]
        return BuiltLineage(
            root_id=segments[0],
            tip_id=tip,
            segment_ids=segments,
            revision=self.db.conversation_revision(tip, include_ancestors=True),
        )

    def counts(self, sid: str) -> RowCounts:
        """Row counts of one segment by kind, from ``get_messages(sid, include_inactive=True)``."""
        flags = self._flags(sid).values()
        return RowCounts(
            rows=len(flags),
            active=sum(1 for f in flags if f[0]),
            archived=sum(1 for f in flags if not f[0] and f[1]),
            rewind=sum(1 for f in flags if not f[0] and not f[1]),
        )

    def _flags(self, sid: str) -> dict[int, tuple[int, int, bool]]:
        return {
            int(m["id"]): (int(m.get("active") or 0), int(m.get("compacted") or 0), bool(m.get("_compressed_summary")))
            for m in self.db.get_messages(sid, include_inactive=True)
        }

    def _newest_row_id(self, sid: str) -> int:
        rows = self.db.get_messages(sid, include_inactive=True, latest=True, limit=1)
        return int(rows[-1]["id"]) if rows else 0


def _json_object(value: Any) -> dict[str, Any] | None:
    """``sessions.model_config`` as a dict (the row stores JSON text)."""
    if value is None or value == "":
        return None
    if isinstance(value, Mapping):
        return dict(value)
    parsed = json.loads(value)
    return parsed if isinstance(parsed, dict) else None


def _split(total: int, parts: int) -> list[int]:
    base, rest = divmod(total, parts)
    return [base + (1 if i < rest else 0) for i in range(parts)]


def large_segment(
    builder: SessionBuilder,
    *,
    live: int,
    archived: int,
    size: int = 64,
    generations: int = 1,
    **create_kw: Any,
) -> str:
    """One segment with exactly ``archived`` archived and ``live`` live rows; returns its id.

    The shape long sessions take under Hermes' default in-place compaction
    (``hermes_cli/config_defaults.py:671``; [SK H1]: 15 613 rows = 5 205 live + 10 408 archived). Each of the
    ``generations`` appends rows (``append_messages_batch``) and compacts them with :meth:`compact_in_place`
    (``keep_last=0``, lock + watermark + coverage); the summary of one generation is archived by the next.
    After the last compaction ``live - 1`` rows follow the live summary, as turns after a compaction do.
    So ``archived = Σ appended + (generations - 1)`` summaries; all archived rows precede the live rows in
    storage order (the B′ precondition, DESIGN §6.8). ``archived == 0`` appends ``live`` plain rows without
    any compaction (e.g. the 12 000-small-rows case). ``create_kw`` goes to :meth:`SessionBuilder.create`.

    With ``builder.templates`` set, an eligible call copies a session-wide template of the same shape instead of
    building (``tests.conftest.SegmentTemplates``); the database and the builder end in the state a direct build
    leaves.
    """
    if live < 1 or archived < 0 or generations < 1:
        raise ValueError("need live >= 1, archived >= 0, generations >= 1")
    if archived and archived - (generations - 1) < generations:
        raise ValueError(f"archived={archived} is too small for {generations} generations")
    shape = {"live": live, "archived": archived, "size": size, "generations": generations}

    def build(target: SessionBuilder) -> str:
        return _build_large_segment(target, create_kw=create_kw, **shape)

    templates = builder.templates
    if templates is not None:
        sid = templates.copy_into(builder, ("large_segment", *shape.values()), create_kw, build)
        if sid is not None:
            return sid
    return build(builder)


def _build_large_segment(
    builder: SessionBuilder, *, live: int, archived: int, size: int, generations: int, create_kw: Mapping[str, Any]
) -> str:
    """The direct build of ``large_segment`` (every row through the core's write paths)."""
    sid = builder.create(**create_kw)
    if archived == 0:
        builder.append(sid, builder.plain_messages(live, size=size, label="live"))
        return sid
    appended = archived - (generations - 1)
    for n in _split(appended, generations):
        builder.append(sid, builder.plain_messages(n, size=size, label="archived"))
        builder.compact_in_place(sid, keep_last=0, observe=False)
    if live > 1:
        builder.append(sid, builder.plain_messages(live - 1, size=size, first_role="assistant", label="live"))
    return sid
