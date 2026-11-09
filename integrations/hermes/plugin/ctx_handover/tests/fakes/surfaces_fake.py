"""Test doubles of cluster F (surfaces and integration; INTERFACES §4.6, §6).

* ``FakeOutboxLedger`` — the three ``Ledger`` outbox methods of INTERFACES §4.5 on a real SQLite file with the
  ``ctx_outbox`` table of INTERFACES §7; two instances on one file behave like two processes.
* ``FakeCtxServer`` — a ctx MCP server behind ``ctx.call_mcp(server, tool, arguments, timeout)``.
* ``FakeBackend`` — the ``commands.Backend`` surface in memory (peers, jobs, pairing, hooks runtime …).
* ``FakeHookRuntime`` — the veto/arrival index the hooks consult (``lock.VetoIndex`` + ledger note calls).
* ``RecordingSpawner`` — a ``continuation.Spawner`` stand-in for cluster A's ``LocalTarget`` tests.
"""

from __future__ import annotations

import json
import sqlite3
import threading
from collections.abc import Callable
from pathlib import Path

_OUTBOX_DDL = (
    "CREATE TABLE IF NOT EXISTS ctx_outbox(id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, "
    "payload TEXT NOT NULL, created_at REAL NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, "
    "next_try_at REAL NOT NULL, done_at REAL)"
)


class FakeOutboxLedger:
    """``outbox_put`` / ``outbox_due`` / ``outbox_done`` (INTERFACES §4.5) over a real SQLite file.

    Backoff after a failure: ``min(60 * 2**attempts, 3600)`` seconds. ``now`` for ``outbox_put`` is the
    ``clock`` (default 0.0, so a fresh row is due at once).
    """

    def __init__(self, path: Path, clock: Callable[[], float] = lambda: 0.0) -> None:
        self.path = Path(path)
        self.clock = clock
        self._lock = threading.Lock()
        with self._connect() as conn:
            conn.execute(_OUTBOX_DDL)

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.path, timeout=5.0, isolation_level=None)
        conn.row_factory = sqlite3.Row
        return conn

    def outbox_put(self, kind: str, payload: dict) -> None:
        now = self.clock()
        with self._lock, self._connect() as conn:
            conn.execute(
                "INSERT INTO ctx_outbox(kind, payload, created_at, next_try_at) VALUES (?, ?, ?, ?)",
                (kind, json.dumps(payload, sort_keys=True, ensure_ascii=False), now, now),
            )

    def outbox_due(self, now: float, limit: int = 20) -> list[dict]:
        with self._lock, self._connect() as conn:
            rows = conn.execute(
                "SELECT id, kind, payload, created_at, attempts, next_try_at FROM ctx_outbox "
                "WHERE done_at IS NULL AND next_try_at <= ? ORDER BY id LIMIT ?",
                (now, int(limit)),
            ).fetchall()
        return [{**dict(r), "payload": json.loads(r["payload"])} for r in rows]

    def outbox_done(self, row_id: int, ok: bool, now: float) -> None:
        with self._lock, self._connect() as conn:
            if ok:
                conn.execute("UPDATE ctx_outbox SET done_at = ? WHERE id = ?", (now, row_id))
            else:
                row = conn.execute("SELECT attempts FROM ctx_outbox WHERE id = ?", (row_id,)).fetchone()
                attempts = int(row["attempts"]) if row else 0
                delay = min(60 * 2**attempts, 3600)
                conn.execute(
                    "UPDATE ctx_outbox SET attempts = attempts + 1, next_try_at = ? WHERE id = ?",
                    (now + delay, row_id),
                )

    def rows(self) -> list[dict]:
        with self._connect() as conn:
            return [dict(r) for r in conn.execute("SELECT * FROM ctx_outbox ORDER BY id").fetchall()]


class FakeCtxServer:
    """``call_mcp`` of a ctx MCP server: ``search`` answers from ``heads`` (title → block id), ``store`` records
    the call and answers ``store_result``. ``fail_with`` makes every call raise."""

    def __init__(
        self,
        *,
        heads: dict[str, str] | None = None,
        fail_with: BaseException | None = None,
        store_result: dict | None = None,
    ) -> None:
        self.heads = dict(heads or {})
        self.fail_with = fail_with
        self.store_result = store_result
        self.stores: list[dict] = []
        self.searches: list[dict] = []

    def call_mcp(self, server: str, tool: str, arguments: dict | None = None, timeout: float = 30) -> dict:
        call = {"server": server, "tool": tool, "arguments": dict(arguments or {}), "timeout": timeout}
        if self.fail_with is not None:
            raise self.fail_with
        if tool == "search":
            self.searches.append(call)
            query = call["arguments"].get("query", "")
            results = [{"id": bid, "title": title} for title, bid in self.heads.items() if title == query]
            return {"ok": True, "result": json.dumps({"results": results})}
        if tool == "store":
            self.stores.append(call)
            if self.store_result is not None:
                return self.store_result
            return {"ok": True, "result": f"Stored block (id: 0199{len(self.stores):04d}-0000-7000-8000-000000000000)"}
        return {"ok": False, "error": f"unknown tool {tool}"}


# ---------------------------------------------------------------------------------------------------------
# commands.Backend / HookRuntime / continuation stand-ins
# ---------------------------------------------------------------------------------------------------------

from ctx_handover.engine import plan as _plan  # noqa: E402
from ctx_handover.engine import surfaces as _surfaces  # noqa: E402
from ctx_handover.engine.errors import HandoverError  # noqa: E402
from ctx_handover.engine.model import (  # noqa: E402
    ContinueResult,
    DestMode,
    FinalPlan,
    Include,
    ItemKind,
    ItemRole,
    NotTransferred,
    Origin,
    Peer,
    PlanItem,
    RefineReport,
    ResourcePlan,
    Right,
    TargetDecision,
)


def sample_final_plan(
    job_id: str = "01JC0000000000000000000ABC",
    *,
    proposed: int = 0,
    suspected: int = 0,
    secrets: int = 0,
    extras: int = 0,
) -> FinalPlan:
    """A small FinalPlan: one primary git repo (included) plus ``proposed`` proposed extras, ``suspected``
    proposed extras the policy flagged as suspected secrets (cluster D's exact marker ``plan.SUSPECTED_SECRET``),
    ``secrets`` secret files left behind and ``extras`` included files outside the workspace. ``model_change`` has
    the form ``LocalTarget`` writes (``from``/``to`` dicts) plus the offline flag."""
    items = [
        PlanItem(
            item_id="w0",
            kind=ItemKind.GIT_REPO,
            source_path="/srv/app",
            role=ItemRole.PRIMARY,
            origin=Origin.CWD,
            include=Include.YES,
            reasons=("cwd",),
            size_estimate=3 * 1024**2,
        )
    ]
    for i in range(proposed):
        items.append(
            PlanItem(
                item_id=f"x{i}",
                kind=ItemKind.EXTRA_FILE,
                source_path=f"/home/u/notes{i}.md",
                role=ItemRole.EXTRA,
                origin=Origin.MODEL,
                include=Include.PROPOSED,
                reasons=("model",),
                size_estimate=1024,
            )
        )
    for i in range(suspected):
        items.append(
            PlanItem(
                item_id=f"s{i}",
                kind=ItemKind.EXTRA_FILE,
                source_path=f"/home/u/token{i}.txt",
                role=ItemRole.EXTRA,
                origin=Origin.TRANSCRIPT,
                include=Include.PROPOSED,
                reasons=("transcript", _plan.SUSPECTED_SECRET),
                size_estimate=64,
            )
        )
    for i in range(extras):
        items.append(
            PlanItem(
                item_id=f"e{i}",
                kind=ItemKind.EXTRA_FILE,
                source_path=f"/home/u/extra{i}.txt",
                role=ItemRole.EXTRA,
                origin=Origin.USER,
                include=Include.YES,
                reasons=("user",),
                size_estimate=2048,
            )
        )
    plan = ResourcePlan(
        job_id=job_id,
        revision=(1,),
        items=tuple(items),
        not_transferred=tuple(
            NotTransferred(path=f"/srv/app/.env{i or ''}", why=_plan.WHY_SECRET) for i in range(secrets)
        ),
        processes=(),
        delegations=(),
        cron=(),
        refine=RefineReport(used="off"),
        tools_needed=(),
        env_vars_needed=(),
        notes_for_target="",
        approx_tokens=1000,
    )
    decisions = (
        TargetDecision(item_id="w0", dest="/home/u/hermes-handover/app", mode=DestMode.FRESH, reason="workspace-root"),
    )
    return FinalPlan(
        plan_id="p" * 64,
        plan=plan,
        decisions=decisions,
        need_bytes=10 * 1024**2,
        free_bytes={"/home/u": 10 * 1024**3},
        model_change={
            "from": {"model": "gpt-x", "provider": "cloud"},
            "to": {"model": "qwen3", "provider": "local"},
            "reason": "provider_not_configured_here",
            "offline_capable": True,
        },
    )


def make_peer(name: str, direction: str = "dial", rights=(Right.PUSH, Right.PULL), **kw) -> Peer:
    return Peer(
        name=name,
        peer_id="01P" + name.upper()[:20],
        direction=direction,
        url="https://" + name + ".example" if direction == "dial" else None,
        pubkey="A" * 43,
        rights=frozenset(rights),
        **kw,
    )


class FakeHookRuntime:
    """``commands.HookRuntime`` in memory: ``vetoed`` (session id → entry, with ``parents``), ``arrivals``."""

    def __init__(self) -> None:
        self.vetoed: dict[str, dict] = {}
        self.arrivals: dict[str, dict] = {}
        self.compactions: list[tuple[str, str]] = []
        self.offered: list[tuple[str, str]] = []
        self.delivered: list[str] = []
        self.raise_on_lookup: BaseException | None = None

    def lookup(self, session_id: str, parent_session_id: str | None = None) -> dict | None:
        if self.raise_on_lookup is not None:
            raise self.raise_on_lookup
        return self.vetoed.get(session_id) or (self.vetoed.get(parent_session_id) if parent_session_id else None)

    def veto_text(self, entry: dict) -> str:
        return f"This session was handed over to {entry.get('peer')}. Run /handover reclaim."

    def arrival(self, session_id: str) -> dict | None:
        return self.arrivals.get(session_id)

    def on_compaction(self, session_id: str, old_session_id: str) -> None:
        self.compactions.append((session_id, old_session_id))
        if old_session_id in self.vetoed:
            self.vetoed[session_id] = self.vetoed[old_session_id]

    def note_offered(self, session_id: str, turn_id: str) -> None:
        self.offered.append((session_id, turn_id))

    def note_delivered(self, session_id: str) -> None:
        # Only recorded: the real index learns the new note state from the ledger asynchronously (poll ≈ 2 s),
        # so the hooks must not rely on ``arrival()`` changing right after delivery.
        self.delivered.append(session_id)


class FakeBackend:
    """``commands.Backend`` in memory. ``confirm_reasons`` drive the confirmation ``start_job`` asks for;
    ``start_error`` makes ``start_job`` raise; every call is recorded in ``calls``."""

    def __init__(self, *, peers: list[Peer] | None = None, config: dict | None = None) -> None:
        self.peer_list: list[Peer] = list(peers or [])
        self.config_data: dict = dict(config or {})
        self.calls: list[tuple[str, dict]] = []
        self.confirm_reasons: list[str] = []
        self.confirm_answers: list[str] = []
        self.confirm_even_with_yes = False
        # info["yes_exempt"] as the conductor passes it (conductor._confirm, gp-a); None = D's table over the reasons
        self.yes_exempt: list[str] | None = None
        self.summary_extra: dict = {}  # keys merged into D's presend_summary (e.g. workspace_excluded, R30)
        self.instruction_files: list[dict] = []
        self.pull_log: list[dict] = []
        self.start_error: BaseException | None = None
        self.final = sample_final_plan()
        self.started: list[str] = []
        self.statuses: dict[str, dict] = {}
        self.jobs: list[dict] = []
        self.session_states: dict[str, dict] = {}
        self.sources: dict[str, str] = {}
        self.suggestions: list[tuple[str, dict]] = []
        self.hook = FakeHookRuntime()
        self.checks: list[dict] = []
        self.remote = {"sessions": [], "offers": []}
        self.settings_written: dict = {}
        self.granted: list[str] = []
        self.foreground_code = 0
        self.outbox: dict = {}
        self._n = 0

    def _rec(self, _call: str, **kw) -> None:
        self.calls.append((_call, kw))

    def names(self) -> list[str]:
        return [c[0] for c in self.calls]

    def last(self, name: str) -> dict:
        return [kw for n, kw in self.calls if n == name][-1]

    # settings / config
    def settings(self) -> dict:
        return _surfaces.settings_from_config(self.config_data)

    def config(self) -> dict:
        return self.config_data

    def local_name(self) -> str:
        return "server"

    # peers
    def peers(self, *, include_revoked: bool = False) -> list[Peer]:
        return [p for p in self.peer_list if include_revoked or not p.revoked_at]

    def peer(self, name: str) -> Peer | None:
        return next((p for p in self.peer_list if p.name == name), None)

    def revoke_peer(self, name: str) -> list[str]:
        self._rec("revoke_peer", name=name)
        return ["01JOBA"]

    def remove_peer(self, name: str) -> bool:
        self._rec("remove_peer", name=name)
        self.peer_list = [p for p in self.peer_list if p.name != name]
        return True

    def rename_peer(self, old: str, new: str) -> bool:
        self._rec("rename_peer", old=old, new=new)
        return any(p.name == old for p in self.peer_list)

    # pairing / identity
    def create_pair_code(self, *, rights, name_hint: str, ttl_s: int, url: str) -> dict:
        self._rec("create_pair_code", rights=rights, name_hint=name_hint, ttl_s=ttl_s, url=url)
        return {
            "pair_string": "ctxh-pair:eyJ2IjoxfQ",
            "fingerprint": "0123456789abcdef",
            "expires_at": 600.0,
            "url": url,
        }

    def redeem_pair(self, pair_string: str, *, name, gate, allow_insecure: bool) -> Peer:
        self._rec("redeem_pair", pair_string=pair_string, name=name, gate=gate, allow_insecure=allow_insecure)
        peer = make_peer(name or "server", gate=gate)
        self.peer_list.append(peer)
        return peer

    def identity(self, *, create: bool):
        self._rec("identity", create=create)
        return {"instance_id": "01INST", "fingerprint": "0123456789abcdef"}

    def rotate_identity(self) -> dict:
        self._rec("rotate_identity")
        return {"instance_id": "01INST", "fingerprint": "fedcba9876543210"}

    # jobs
    def start_job(
        self,
        mode,
        peer,
        options,
        *,
        session_ref,
        calling_session,
        session_key,
        bundle_path,
        confirm,
        side_question,
        reporter,
        include_secret=(),
    ):
        self._n += 1
        job_id = f"01JC00000000000000000JOB{self._n:02d}"
        self._rec(
            "start_job",
            mode=mode,
            peer=peer,
            options=options,
            session_ref=session_ref,
            calling_session=calling_session,
            session_key=session_key,
            bundle_path=bundle_path,
            confirm=confirm,
            side_question=side_question,
            reporter=reporter,
            include_secret=tuple(include_secret),
            job_id=job_id,
        )
        if self.start_error is not None:
            raise self.start_error
        if self.confirm_reasons and (not options.yes or self.confirm_even_with_yes):
            # the info the conductor passes (conductor._confirm): D's reason codes or letters, D's presend summary
            answer = confirm(
                {
                    "reasons": list(self.confirm_reasons),
                    "yes_exempt": (
                        list(self.yes_exempt) if self.yes_exempt is not None else _plan.yes_exempt(self.confirm_reasons)
                    ),
                    "summary": {**_plan.presend_summary(self.final), **self.summary_extra},
                    "final_plan": self.final,
                    "suspected_secrets": [
                        i.source_path
                        for i in self.final.plan.items
                        if i.include is Include.PROPOSED and _plan.SUSPECTED_SECRET in i.reasons
                    ],
                    "instruction_files": list(self.instruction_files),
                }
            )
            self.confirm_answers.append(answer)
            if answer in {"show_plan", "cancel"}:
                return job_id, None
        return job_id, self.final

    def continue_async(self, job_id: str) -> None:
        self._rec("continue_async", job_id=job_id)
        self.started.append(job_id)

    def run_foreground(self, job_id: str) -> int:
        self._rec("run_foreground", job_id=job_id)
        if isinstance(self.foreground_code, BaseException):
            raise self.foreground_code
        return int(self.foreground_code)

    def cancel(self, job_id: str) -> str:
        self._rec("cancel", job_id=job_id)
        return f"Cancel requested for {job_id[-6:]}."

    def resume(self, job_id: str) -> None:
        self._rec("resume", job_id=job_id)

    def job_status(self, job_id: str | None) -> dict:
        self._rec("job_status", job_id=job_id)
        if job_id:
            if job_id not in self.statuses:
                raise HandoverError(f"unknown job {job_id}")
            return {"job": self.statuses[job_id]}
        pulled = [{"peer": r.get("peer_id"), "at": r.get("at"), "items": r.get("items")} for r in self.pull_log]
        return {"jobs": list(self.jobs), "pull_log": list(self.pull_log), "outbox_pending": 0, "pulled_by": pulled}

    def create_offer(self, peer, session_ref, options, *, side_question, include_secret=()) -> dict:
        refinement = side_question("which resources?") if side_question else None
        self._rec(
            "create_offer",
            peer=peer,
            session_ref=session_ref,
            options=options,
            refinement=refinement,
            include_secret=tuple(include_secret),
        )
        return {"offer_id": "01OFFER", "expires_at": 1e18, "refinement": refinement}

    def remote_choices(self, peer, *, limit: int) -> dict:
        self._rec("remote_choices", peer=peer, limit=limit)
        return self.remote

    def reclaim(self, session_id: str, *, force: bool) -> dict:
        self._rec("reclaim", session_id=session_id, force=force)
        return {"state": "reclaimed", "warnings": ["the copy on laptop stays active"]}

    def gc(self, *, job_id, all_jobs: bool) -> dict:
        self._rec("gc", job_id=job_id, all_jobs=all_jobs)
        return {"freed_bytes": 5 * 1024**2, "jobs": 1 if job_id else 0}

    def logs(self, job_id: str) -> list[dict]:
        self._rec("logs", job_id=job_id)
        return [{"ts": 1.0, "kind": "intent", "step": "lock", "state": "locking", "data": {"x": 1}}]

    def session_state(self, session_id: str) -> dict:
        self._rec("session_state", session_id=session_id)
        return self.session_states.get(session_id, {})

    def session_source(self, session_id: str) -> str | None:
        return self.sources.get(session_id)

    def suggestion_put(self, session_id: str, data: dict) -> None:
        self.suggestions.append((session_id, data))

    def presend_summary(self, final) -> dict:
        if not self.summary_extra:
            return _surfaces.plan_summary(final)
        return _surfaces.plan_summary(final, {**_plan.presend_summary(final), **self.summary_extra})

    def doctor_checks(self, *, measure: bool = False) -> list[dict]:
        self._rec("doctor_checks", measure=measure)
        return list(self.checks)

    def flush_outbox(self, call_mcp) -> dict:
        self._rec("flush_outbox", call_mcp=call_mcp)
        return dict(self.outbox)

    def grant_ctx(self, server: str) -> None:
        self.granted.append(server)

    def set_setting(self, key: str, value) -> None:
        self.settings_written[key] = value

    def hook_runtime(self):
        self._rec("hook_runtime")
        return self.hook


class RecordingSpawner:
    """``continuation.Spawner`` stand-in: records ``spawn`` calls; ``refuse`` raises the given error."""

    def __init__(self, refuse: BaseException | None = None) -> None:
        self.refuse = refuse
        self.spawned: list[dict] = []

    def hermes_argv(self) -> list[str]:
        return ["hermes"]

    def spawn(self, session_id: str, prompt, job_id: str, *, requested_by=None, verified: bool = True, cwd=None):
        if self.refuse is not None:
            raise self.refuse
        self.spawned.append(
            {
                "session_id": session_id,
                "prompt": prompt,
                "job_id": job_id,
                "requested_by": requested_by,
                "verified": verified,
                "cwd": cwd,
            }
        )
        return ContinueResult(started=True, pid=4242, log=f"/logs/continue-{job_id}.log")

    def result(self, job_id: str) -> dict:
        hit = [s for s in self.spawned if s["job_id"] == job_id]
        return {"started": bool(hit), "pid": 4242 if hit else None}
