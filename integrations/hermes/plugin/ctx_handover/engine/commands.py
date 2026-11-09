"""``/handover …``, ``hermes handover …`` and the plugin hooks — the composition root of the surfaces (layer L11;
DESIGN §14.1, §14.2, §11.4, §7.6, §8.3, §13; INTERFACES §4.6).

``register()`` builds one ``Plugin`` per ``ctx`` without any I/O (R12). Everything that touches disk, the
network or Hermes runs only when a person types a command, a hook fires or the agent calls the tool, and goes
through a ``Backend``: ``EngineBackend`` composes the engine (jobs, ledger, pairing, lock …) and Hermes
(``hostcompat.<fn>``); unit tests swap in ``tests/fakes/surfaces_fake.FakeBackend``.

Rules this module enforces:

* **R24 / I12** — administrative and data-moving verbs (``LOCAL_ONLY_VERBS``) answer only on local surfaces
  (``LOCAL_PLATFORMS``) and in the CLI; any messaging platform gets a refusal. ``status``, ``doctor``, ``logs``
  and ``help`` stay available, limited to the caller's own session there.
* **R11** — the session and its cwd are always the session row's (``session_id`` keyword of the handler), never
  ``ctx.session_cwd()``.
* **BX L5** — an unanswered confirmation continues without proposed items when proposed items are the only
  reason, and cancels with nothing locked otherwise.
* **§7.6** — the veto hooks never raise and do no I/O after the first call of a process (the first call opens
  the ledger once through ``Backend.hook_runtime``).
"""

from __future__ import annotations

import argparse
import contextlib
import contextvars
import importlib.util
import json
import logging
import os
import shlex
import stat
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import weakref
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

from . import ctxrecords, hostcompat, surfaces
from .continuation import Spawner
from .errors import (
    BadRequest,
    ConflictError,
    Forbidden,
    HandoverError,
    InDoubt,
    NotFound,
    error_class,
)
from .model import (
    ContinueResult,
    FinalPlan,
    GateAuth,
    GateMode,
    Mode,
    Options,
    Peer,
    Right,
    SessionRef,
)
from .progress import PostMessageReporter, ReporterBroker, TtyReporter

logger = logging.getLogger(__name__)

LOCAL_ONLY_VERBS: frozenset[str] = frozenset(
    {"pair", "setup", "peers", "identity", "take", "to", "export", "import", "reclaim", "resume", "gc", "cancel"}
)
"""DESIGN §14.1 surface restriction (R24, SEC 6.4)."""

LOCAL_PLATFORMS = surfaces.LOCAL_PLATFORMS

REFUSAL = "This command only runs in the Hermes CLI, TUI or Desktop on this machine."
SIDE_QUESTION_TTL_S = 60.0
NOTE_MAX_OFFERS = 3
SIDE_QUESTION_CAPABILITY = "llm.side_question"

EXIT_OK, EXIT_FAILED, EXIT_USAGE, EXIT_BUSY, EXIT_AUTH, EXIT_DOUBT = 0, 1, 2, 3, 4, 5
_EXIT_BY_CODE = {
    "bad_request": EXIT_USAGE,
    "not_found": EXIT_USAGE,
    "session_busy": EXIT_BUSY,
    "already_handing_over": EXIT_BUSY,
    "wait_timeout": EXIT_BUSY,
    "job_conflict": EXIT_BUSY,
    "conflict": EXIT_BUSY,
    "invalid_state": EXIT_BUSY,
    "gate_rejected": EXIT_AUTH,
    "unauthorized": EXIT_AUTH,
    "forbidden": EXIT_AUTH,
    "pair_invalid": EXIT_AUTH,
    "https_required": EXIT_AUTH,
    "bad_response_signature": EXIT_AUTH,
    "identity_lost": EXIT_AUTH,
    "in_doubt": EXIT_DOUBT,
    "peer_unreachable": EXIT_DOUBT,
}


_BROKER = ReporterBroker()
"""One broker per process: the profile's single ``JobRunner`` (``jobs.runner_for``) is built with one
``reporter_factory``, so every ``Plugin`` of this process binds its reporters to the same broker."""


def allowed_on(platform: str | None, verb: str) -> bool:
    """True when ``verb`` may run for a caller on ``platform`` (R24). ``None`` (unknown surface) fails closed."""
    if verb not in LOCAL_ONLY_VERBS:
        return True
    return platform is not None and str(getattr(platform, "value", platform)).lower() in LOCAL_PLATFORMS


def exit_code_for(err: BaseException) -> int:
    """CLI exit code of DESIGN §14.2: 1 failed, 3 busy/conflict/wait timeout, 4 auth/gate, 5 in-doubt/suspended."""
    return _EXIT_BY_CODE.get(getattr(err, "code", ""), EXIT_FAILED)


# ---------------------------------------------------------------------------------------------------------
# Seams
# ---------------------------------------------------------------------------------------------------------


class HookRuntime(Protocol):
    """What the hooks consult (``lock.VetoIndex`` plus queued ledger note writes). No method does blocking I/O."""

    def lookup(self, session_id: str, parent_session_id: str | None = None) -> dict | None: ...
    def veto_text(self, entry: dict) -> str: ...
    def arrival(self, session_id: str) -> dict | None: ...
    def on_compaction(self, session_id: str, old_session_id: str) -> None: ...
    def note_offered(self, session_id: str, turn_id: str) -> None: ...
    def note_delivered(self, session_id: str) -> None: ...


class Backend(Protocol):
    """Everything the surfaces need from the engine and from Hermes (see ``EngineBackend``)."""

    def settings(self) -> dict: ...
    def config(self) -> dict: ...
    def local_name(self) -> str: ...
    def peers(self, *, include_revoked: bool = False) -> list[Peer]: ...
    def peer(self, name: str) -> Peer | None: ...
    def revoke_peer(self, name: str) -> list[str]: ...
    def remove_peer(self, name: str) -> bool: ...
    def rename_peer(self, old: str, new: str) -> bool: ...
    def create_pair_code(self, *, rights: frozenset[Right], name_hint: str, ttl_s: int, url: str) -> dict: ...
    def redeem_pair(self, pair_string: str, *, name: str | None, gate: GateAuth, allow_insecure: bool) -> Peer: ...
    def identity(self, *, create: bool) -> dict | None: ...
    def rotate_identity(self) -> dict: ...
    def start_job(
        self,
        mode: Mode,
        peer: Peer | None,
        options: Options,
        *,
        session_ref: SessionRef,
        calling_session: str | None,
        session_key: str | None,
        bundle_path: str | None,
        confirm: Callable[[dict], str] | None,
        side_question: Callable[[str], str | None] | None,
        reporter: Any,
    ) -> tuple[str, FinalPlan | None]: ...
    def continue_async(self, job_id: str) -> None: ...
    def run_foreground(self, job_id: str) -> int: ...
    def cancel(self, job_id: str) -> str: ...
    def resume(self, job_id: str) -> None: ...
    def job_status(self, job_id: str | None) -> dict: ...
    def create_offer(
        self,
        peer: Peer,
        session_ref: SessionRef,
        options: Options,
        *,
        side_question: Callable[[str], str | None] | None,
    ) -> dict: ...
    def remote_choices(self, peer: Peer, *, limit: int) -> dict: ...
    def reclaim(self, session_id: str, *, force: bool) -> dict: ...
    def gc(self, *, job_id: str | None, all_jobs: bool) -> dict: ...
    def logs(self, job_id: str) -> list[dict]: ...
    def session_state(self, session_id: str) -> dict: ...
    def session_source(self, session_id: str) -> str | None: ...
    def suggestion_put(self, session_id: str, data: dict) -> None: ...
    def presend_summary(self, final: FinalPlan) -> dict: ...
    def doctor_checks(self) -> list[dict]: ...
    def flush_outbox(self, call_mcp: Callable[..., dict] | None) -> dict: ...
    def grant_ctx(self, server: str) -> None: ...
    def set_setting(self, key: str, value: Any) -> None: ...
    def hook_runtime(self) -> HookRuntime | None: ...


# ---------------------------------------------------------------------------------------------------------
# Hooks (DESIGN §7.6 veto, §11.4 note delivery, §8.3 side-question tokens)
# ---------------------------------------------------------------------------------------------------------


class SideQuestionTokens:
    """``post_llm_call``'s ``side_question_token`` per session, usable ≤ 60 s after the turn (one redemption)."""

    def __init__(self, ttl_s: float = SIDE_QUESTION_TTL_S, clock: Callable[[], float] = time.monotonic) -> None:
        self._ttl = ttl_s
        self._clock = clock
        self._tokens: dict[str, tuple[str, float]] = {}
        self._lock = threading.Lock()

    def put(self, session_id: str, token: str) -> None:
        with self._lock:
            now = self._clock()
            self._tokens = {k: v for k, v in self._tokens.items() if now - v[1] <= self._ttl}
            self._tokens[session_id] = (token, now)

    def take(self, session_id: str) -> str | None:
        with self._lock:
            entry = self._tokens.pop(session_id, None)
        if entry is None or self._clock() - entry[1] > self._ttl:
            return None
        return entry[0]


class HookSet:
    """The four hook callbacks. They never raise; after the first call they only touch memory."""

    def __init__(self, runtime_factory: Callable[[], HookRuntime | None], tokens: SideQuestionTokens) -> None:
        self._factory = runtime_factory
        self._tokens = tokens
        self._runtime: HookRuntime | None = None
        self._loaded = False
        self._lock = threading.Lock()
        self._offers: dict[str, int] = {}
        self._offered_turn: dict[str, str] = {}
        self._delivered: set[str] = set()

    def callbacks(self) -> dict[str, Callable[..., Any]]:
        return {
            "pre_llm_call": self.pre_llm_call,
            "pre_tool_call": self.pre_tool_call,
            "post_llm_call": self.post_llm_call,
            "post_compaction": self.post_compaction,
        }

    def _rt(self) -> HookRuntime | None:
        if self._loaded:
            return self._runtime
        with self._lock:
            if not self._loaded:
                try:
                    self._runtime = self._factory()
                except Exception:
                    logger.warning("ctx_handover: hook runtime unavailable; the source lock still holds", exc_info=True)
                    self._runtime = None
                self._loaded = True
        return self._runtime

    def _veto(self, rt: HookRuntime, session_id: str, parent: str | None) -> dict | None:
        entry = rt.lookup(session_id, parent or None)
        if not entry:
            return None
        try:
            text = rt.veto_text(entry)
        except Exception:
            text = "This session was handed over to another gateway. Run /handover reclaim to continue here."
        return {"action": "block", "message": text}

    def pre_llm_call(self, session_id: str = "", parent_session_id: str = "", turn_id: str = "", **_kw: Any):
        try:
            rt = self._rt()
            if rt is None or not session_id:
                return None
            block = self._veto(rt, session_id, parent_session_id)
            if block is not None:
                return block
            arrival = rt.arrival(session_id)
            if not arrival or session_id in self._delivered:
                return None
            if str(arrival.get("note_state", "pending")) not in {"pending", "offered"}:
                return None
            note = arrival.get("note")
            if not isinstance(note, str) or not note:
                return None
            with self._lock:
                count = max(self._offers.get(session_id, 0), int(arrival.get("offers") or 0))
                if count >= NOTE_MAX_OFFERS:
                    return None
                self._offers[session_id] = count + 1
                self._offered_turn[session_id] = str(turn_id or "")
            with contextlib.suppress(Exception):
                rt.note_offered(session_id, str(turn_id or ""))
            return {"context": note}
        except Exception:
            logger.debug("ctx_handover pre_llm_call failed", exc_info=True)
            return None

    def pre_tool_call(self, session_id: str = "", **_kw: Any):
        try:
            rt = self._rt()
            if rt is None or not session_id:
                return None
            return self._veto(rt, session_id, None)
        except Exception:
            return None

    def post_llm_call(self, session_id: str = "", turn_id: str = "", side_question_token: str | None = None, **_kw):
        try:
            if session_id and side_question_token:
                self._tokens.put(session_id, str(side_question_token))
            if not session_id:
                return None
            with self._lock:
                offered = session_id in self._offered_turn
                if offered:
                    self._offered_turn.pop(session_id, None)
                    self._delivered.add(session_id)
            if offered:
                rt = self._rt()
                if rt is not None:
                    rt.note_delivered(session_id)
        except Exception:
            logger.debug("ctx_handover post_llm_call failed", exc_info=True)
        return None

    def post_compaction(self, session_id: str = "", old_session_id: str = "", **_kw: Any):
        try:
            rt = self._rt()
            if rt is not None and session_id and old_session_id:
                rt.on_compaction(session_id, old_session_id)
        except Exception:
            logger.debug("ctx_handover post_compaction failed", exc_info=True)
        return None


# ---------------------------------------------------------------------------------------------------------
# One call of a verb
# ---------------------------------------------------------------------------------------------------------


@dataclass
class Call:
    """Context of one command: who asked, from where, how to ask back and how to report."""

    platform: str | None
    session_id: str | None
    session_key: str | None
    asker: Callable[[str, list], str | None] | None
    cli: bool = False
    json_mode: bool = False
    out: list[str] = field(default_factory=list)
    data: dict = field(default_factory=dict)
    exit_code: int = EXIT_OK

    @property
    def local(self) -> bool:
        return self.cli or (self.platform is not None and str(self.platform).lower() in LOCAL_PLATFORMS)

    def say(self, text: str) -> None:
        if text:
            self.out.append(str(text))


@dataclass
class _ConfirmState:
    summary: dict | None = None
    answer: str | None = None
    timed_out: bool = False
    reasons: list[str] = field(default_factory=list)


class Plugin:
    """Per-``ctx`` holder of the surfaces (slash, CLI, hooks, tool). Construction does no I/O."""

    def __init__(self, ctx: Any, backend_factory: Callable[[Any, Plugin], Backend] | None = None) -> None:
        self.ctx = ctx
        self._backend_factory = backend_factory or (lambda c, p: EngineBackend(c, p))
        self._backend: Backend | None = None
        self._backend_lock = threading.Lock()
        self.tokens = SideQuestionTokens()
        self.broker = _BROKER
        self.hooks = HookSet(self._hook_runtime, self.tokens)

    # -- backend ----------------------------------------------------------------------------------------
    @property
    def backend(self) -> Backend:
        if self._backend is None:
            with self._backend_lock:
                if self._backend is None:
                    self._backend = self._backend_factory(self.ctx, self)
        return self._backend

    def _can_post(self) -> bool:
        try:
            return bool(hostcompat.features(self.ctx).post_message)
        except Exception:
            return False

    def _hook_runtime(self) -> HookRuntime | None:
        return self.backend.hook_runtime()

    def side_question_for(self, session_id: str | None) -> Callable[[str], str | None] | None:
        """``prompt -> answer`` over ``ctx.side_question`` with this session's fresh token, or None (§8.3)."""
        ask = getattr(self.ctx, "side_question", None)
        has = getattr(self.ctx, "has_capability", None)
        if not session_id or not callable(ask):
            return None
        try:
            if callable(has) and not has(SIDE_QUESTION_CAPABILITY):
                return None
        except Exception:
            return None
        tokens = self.tokens

        def side_question(prompt: str) -> str | None:
            token = tokens.take(session_id)
            if token is None:
                return None
            try:
                answer = ask(token, prompt, timeout=45.0)
            except Exception:
                logger.debug("side_question failed", exc_info=True)
                return None
            return answer if isinstance(answer, str) else None

        return side_question

    def agent_request(self, session_id: str, session_key: str | None, peer_name: str) -> None:
        """``allow_agent_start`` (DESIGN §14.3, SEC 5.6): hand this session to ``peer_name`` as soon as the running
        turn ends (``--wait``), without a person: proposed items are left out, any other confirmation stops the job
        with nothing locked. Runs on a worker thread in a copy of the caller's context; never raises."""
        peer = self._peer(peer_name)
        options = surfaces.options_from(surfaces.parse(f"to {shlex.quote(peer.name)} --wait"), self.backend.settings())
        reporter = PostMessageReporter(self.ctx, peer=peer.name, direction="out", enabled=self._can_post())

        def confirm(info: dict) -> str:
            return surfaces.choice_on_timeout((info or {}).get("reasons") or [])

        def work() -> None:
            try:
                with self.broker.bind(reporter):
                    job_id, final = self.backend.start_job(
                        Mode.PUSH,
                        peer,
                        options,
                        session_ref=SessionRef(id=session_id),
                        calling_session=session_id,
                        session_key=session_key,
                        bundle_path=None,
                        confirm=confirm,
                        side_question=None,
                        reporter=reporter,
                    )
                if final is not None:
                    reporter.start(
                        surfaces.render_presend(self.backend.presend_summary(final), peer.name, Mode.PUSH)
                    )
                    self.backend.continue_async(job_id)
            except HandoverError as err:
                reporter.finish(surfaces.render_error(err, peer.name))
            except Exception:
                logger.warning("ctx_handover: agent-requested handover failed", exc_info=True)

        context = contextvars.copy_context()
        threading.Thread(target=context.run, args=(work,), name="ctx_handover-agent-request", daemon=True).start()

    # -- entry points -----------------------------------------------------------------------------------
    def slash(
        self,
        raw_args: str = "",
        session_id: str | None = None,
        session_key: str | None = None,
        platform: Any = None,
        source: Any = None,
        **_kw: Any,
    ) -> str:
        """``/handover …`` (sync, so ``ask_choice`` works; long handlers are fine, LAB L2). Never raises."""
        plat = platform if platform is None else str(getattr(platform, "value", platform)).lower()
        asker = self._slash_asker()
        call = Call(platform=plat, session_id=session_id or None, session_key=session_key or None, asker=asker)
        self.run(raw_args or "", call)
        return "\n".join(call.out)

    def _slash_asker(self) -> Callable[[str, list], str | None] | None:
        ask = getattr(self.ctx, "ask_choice", None)
        return ask if callable(ask) else None

    def cli(self, ns: argparse.Namespace) -> int:
        """``hermes handover …``: prints the result, returns the exit code of DESIGN §14.2."""
        tokens = [str(getattr(ns, "verb", None) or "help"), *[str(t) for t in (getattr(ns, "rest", None) or [])]]
        call = Call(platform="cli", session_id=None, session_key=None, asker=_tty_asker(), cli=True)
        self.run(shlex.join(tokens), call)
        if call.json_mode:
            payload = {
                "ok": call.exit_code == EXIT_OK,
                "exit_code": call.exit_code,
                "text": "\n".join(call.out),
                **call.data,
            }
            print(json.dumps(payload, ensure_ascii=False, sort_keys=True, default=str))
        elif call.out:
            print("\n".join(call.out))
        return call.exit_code

    def run(self, raw_args: str, call: Call) -> None:
        parsed = surfaces.parse(raw_args)
        call.json_mode = bool(parsed.args.get("json"))
        if parsed.errors:
            call.say("\n".join(parsed.errors))
            call.exit_code = EXIT_USAGE
            return
        if not allowed_on("cli" if call.cli else call.platform, parsed.verb):
            call.say(REFUSAL)
            call.exit_code = EXIT_USAGE
            return
        handler = getattr(self, "_verb_" + parsed.verb)
        try:
            handler(parsed, call)
        except HandoverError as err:
            call.say(surfaces.render_error(err, (err.details or {}).get("peer") or parsed.args.get("peer")))
            call.exit_code = exit_code_for(err)
            call.data.setdefault("error", {"code": err.code, "message": err.message})
        except Exception as exc:  # the handler never raises into Hermes
            logger.exception("ctx_handover: /handover %s failed", parsed.verb)
            call.say(
                f"ctx_handover failed unexpectedly ({type(exc).__name__}). Details are in the Hermes log; "
                "/handover doctor checks the setup."
            )
            call.exit_code = EXIT_FAILED

    # -- helpers ----------------------------------------------------------------------------------------
    def _need_session(self, parsed: surfaces.Parsed, call: Call) -> str:
        sid = parsed.args.get("session") or call.session_id
        if not sid:
            raise BadRequest(
                "No session here to work on. In the CLI name it: --session <id> (hermes sessions list shows ids).",
                field="session",
            )
        return str(sid)

    def _peer(self, name: str | None, *, want: str | None = None) -> Peer:
        if not name:
            raise BadRequest("Name the device: /handover peers lists the paired ones.", field="peer")
        peer = self.backend.peer(name)
        if peer is None or peer.revoked_at:
            names = ", ".join(p.name for p in self.backend.peers()) or "none"
            raise NotFound(
                f"No paired device named {name!r} (paired: {names}). /handover pair connects a new one.", peer=name
            )
        return peer

    def _confirm_fn(self, call: Call, peer_name: str | None, options: Options, state: _ConfirmState):
        def confirm(info: dict) -> str:
            info = info or {}
            reasons = surfaces.reason_names(info.get("reasons") or [])
            summary = surfaces.plan_summary(
                info.get("final_plan"),
                info.get("summary") if isinstance(info.get("summary"), Mapping) else None,
                instruction_files=info.get("instruction_files") or (),
            )
            suspected = surfaces.suspected_secrets(info)
            summary["suspected_secrets"] = suspected
            state.summary = summary
            state.reasons = reasons
            if options.yes:  # R25: a blanket yes never takes suspected secrets along
                state.answer = "no_proposed" if suspected else "yes"
                return state.answer
            question = surfaces.confirm_question(summary, peer_name)
            why = [surfaces.REASON_TEXT.get(r, r) for r in reasons]
            if suspected:  # an item marker, not a reason code (X03): it explains reason (a)
                why.append(surfaces.SUSPECTED_TEXT)
            if why:
                question = f"{question}\n({'; '.join(why)})"
            if suspected:
                question += "\nSuspected secrets: " + ", ".join(suspected[:5])
            answer = None
            if call.asker is not None:
                try:
                    answer = call.asker(question, surfaces.CONFIRM_CHOICES)
                except Exception:
                    answer = None
            if answer not in {c["value"] for c in surfaces.CONFIRM_CHOICES}:
                state.timed_out = True
                answer = surfaces.choice_on_timeout(reasons)
            state.answer = answer
            return answer

        return confirm

    def _start(
        self,
        call: Call,
        mode: Mode,
        peer: Peer | None,
        parsed: surfaces.Parsed,
        *,
        session_ref: SessionRef,
        bundle_path: str | None,
    ) -> None:
        backend = self.backend
        options = surfaces.options_from(parsed, backend.settings())
        state = _ConfirmState()
        direction = "in" if mode in (Mode.PULL, Mode.IMPORT) else "out"
        peer_name = peer.name if peer is not None else None
        holder: dict[str, str] = {}
        if call.cli:
            reporter: Any = TtyReporter(json_mode=call.json_mode, peer=peer_name, direction=direction)
        else:
            reporter = PostMessageReporter(
                self.ctx,
                peer=peer_name,
                direction=direction,
                on_done=lambda: self._final_text(holder.get("job_id"), mode, peer_name, options),
                enabled=self._can_post(),
            )
        confirm = self._confirm_fn(call, peer_name, options, state)
        side_question = self.side_question_for(call.session_id) if mode in (Mode.PUSH, Mode.EXPORT) else None
        with self.broker.bind(reporter):
            job_id, final = backend.start_job(
                mode,
                peer,
                options,
                session_ref=session_ref,
                calling_session=call.session_id,
                session_key=call.session_key,
                bundle_path=bundle_path,
                confirm=confirm,
                side_question=side_question,
                reporter=reporter,
            )
        holder["job_id"] = job_id
        call.data["job_id"] = job_id
        if final is None or options.dry_run:
            summary = state.summary or (surfaces.plan_summary(final) if final is not None else None)
            if options.dry_run and summary is not None:
                call.say(surfaces.render_plan(summary))
                call.say("Dry run: nothing was locked or sent.")
            elif state.answer == "show_plan" and summary is not None:
                call.say(surfaces.render_plan(summary))
                call.say("Nothing was locked. Run the command again to hand over.")
            elif state.timed_out and state.answer == "cancel":
                if summary is not None:
                    call.say(surfaces.render_plan(summary))
                call.say("No answer to the confirmation, so the handover was cancelled. Nothing was locked.")
                call.exit_code = EXIT_FAILED
            else:
                call.say("Cancelled. Nothing was locked.")
                call.exit_code = EXIT_FAILED if not options.dry_run else EXIT_OK
            return
        summary = backend.presend_summary(final)
        presend = surfaces.render_presend(summary, peer_name, mode)
        if call.cli:
            call.say(presend) if call.json_mode else reporter.info(presend)
            try:
                code = backend.run_foreground(job_id)
            except KeyboardInterrupt:
                backend.cancel(job_id)
                try:
                    code = backend.run_foreground(job_id)
                except KeyboardInterrupt:
                    call.say(
                        f"Detached. The job is suspended; /handover resume {surfaces.short_job(job_id)} continues it."
                    )
                    call.exit_code = EXIT_DOUBT
                    return
            call.say(self._final_text(job_id, mode, peer_name, options))
            call.exit_code = int(code)
            return
        reporter.start(presend)
        backend.continue_async(job_id)
        call.say(
            f"Handover {surfaces.short_job(job_id)} is running; progress shows in the notice above. "
            f"/handover status {surfaces.short_job(job_id)} has the details, "
            "/handover cancel stops it before the commit."
        )

    def _final_text(self, job_id: str | None, mode: Mode, peer: str | None, options: Options) -> str:
        if not job_id:
            return "Handover finished. /handover status shows the result."
        try:
            status = self.backend.job_status(job_id)
        except HandoverError as err:
            return surfaces.render_error(err, peer)
        job = status.get("job") if isinstance(status.get("job"), Mapping) else status
        state = str(job.get("state") or "")
        err = job.get("last_error") if isinstance(job.get("last_error"), Mapping) else None
        short = surfaces.short_job(job_id)
        if state == "in_doubt":
            return surfaces.render_error(InDoubt("commit outcome unknown", job=job_id), peer)
        if state in {"aborted", "suspended"}:
            # a cancel by the user ends ``aborted`` with last_error.code == "cancelled" (rendered as such)
            code = (err or {}).get("code")
            if code:
                klass = error_class(str(code)) or HandoverError
                details = {"job": job_id, **dict((err or {}).get("details") or {})}
                text = surfaces.render_error(klass(str((err or {}).get("message") or ""), details=details), peer)
            else:
                text = f"Handover {short} ended: {state}. /handover status {short}."
            if state == "suspended" and "/handover resume" not in text:
                text += f" /handover resume {short} continues it."
            return text
        from .jsonutil import from_wire
        from .model import CommitAck

        ack = job.get("ack")
        if isinstance(ack, Mapping):
            try:
                ack = from_wire(CommitAck, dict(ack))
            except HandoverError:
                ack = None
        cont = job.get("continuation")
        cont_result = None
        if isinstance(cont, Mapping) and cont.get("started"):
            cont_result = ContinueResult(started=True, pid=cont.get("pid"), log=cont.get("log"))
        cont_error = None
        if isinstance(cont, Mapping) and cont.get("error"):
            from .errors import ContinueRefused

            cont_error = ContinueRefused(
                str(cont["error"].get("message", "")), details=dict(cont["error"].get("details") or {})
            )
        bundle = job.get("bundle") if isinstance(job.get("bundle"), Mapping) else {}
        text = surfaces.render_final(
            mode,
            ack if not isinstance(ack, Mapping) else None,
            state,
            peer,
            cont_result,
            title=job.get("title"),
            note=job.get("note"),
            origin=job.get("origin"),
            excluded_secrets=job.get("excluded_secrets") or (),
            withheld_stash=job.get("withheld_stash") or (),
            conflicts=job.get("conflicts") or (),
            instruction_files=job.get("instruction_files") or (),
            warnings=job.get("warnings") or (),
            continue_error=cont_error,
            bundle_path=bundle.get("path"),
            bundle_bytes=bundle.get("size"),
            keep=options.keep_source,
            remote_gateway=bool(bundle.get("remote")),
        )
        if state == "retiring" and job.get("next_action") and mode not in (Mode.PULL, Mode.IMPORT):
            text += f"\nNext: {job['next_action']}"
        return text

    # -- verbs ------------------------------------------------------------------------------------------
    def _verb_help(self, parsed: surfaces.Parsed, call: Call) -> None:
        call.say(surfaces.help_text(parsed.args.get("topic")))

    def _verb_to(self, parsed: surfaces.Parsed, call: Call) -> None:
        sid = self._need_session(parsed, call)
        peer = self._peer(parsed.args.get("peer"))
        if peer.direction == "inbound":
            if Right.PULL not in peer.rights:
                raise Forbidden(
                    f"{peer.name} may not take sessions from here.",
                    right="pull",
                    peer=peer.name,
                    details={"right": "pull"},
                )
            options = surfaces.options_from(parsed, self.backend.settings())
            offer = self.backend.create_offer(
                peer, SessionRef(id=sid), options, side_question=self.side_question_for(sid)
            )
            call.data["offer"] = offer
            call.say(surfaces.render_offer(offer, peer.name, self.backend.local_name(), now=time.time()))
            if parsed.args.get("continue"):
                call.say(
                    f"--continue applies on {peer.name}: use /handover take "
                    f"{self.backend.local_name()} --continue there."
                )
            return
        if Right.PUSH not in peer.rights:
            raise Forbidden("push not granted", right="push", peer=peer.name)
        self._start(call, Mode.PUSH, peer, parsed, session_ref=SessionRef(id=sid), bundle_path=None)

    def _resolve_take(self, parsed: surfaces.Parsed, call: Call) -> tuple[Peer, str | None]:
        pos = list(parsed.args.get("positionals") or [])
        dial = [p for p in self.backend.peers() if p.direction == "dial" and not p.revoked_at]
        if len(pos) == 2:
            return self._peer(pos[0]), pos[1]
        if len(pos) == 1:
            named = self.backend.peer(pos[0])
            if named is not None and not named.revoked_at:
                return named, None
            if len(dial) == 1:
                return dial[0], pos[0]
            raise NotFound(f"No paired device named {pos[0]!r}. /handover peers lists them.", peer=pos[0])
        if len(dial) == 1:
            return dial[0], None
        if not dial:
            raise NotFound("No paired device to take a session from. Run /handover pair first.")
        if call.asker is not None:
            picked = call.asker("Take a session from which device?", [p.name for p in dial])
            for p in dial:
                if picked == p.name:
                    return p, None
        raise BadRequest(
            f"Name the device: /handover take <peer> (paired: {', '.join(p.name for p in dial)}).", field="peer"
        )

    def _pick_remote(self, peer: Peer, call: Call) -> SessionRef | None:
        choices = self.backend.remote_choices(peer, limit=20)
        options: list[dict] = []
        if self.backend.settings().get("accept_offers", "ask") != "off":
            for o in choices.get("offers") or ():
                title = (o.get("session") or {}).get("title") if isinstance(o.get("session"), Mapping) else None
                options.append(
                    {
                        "value": "offer:" + str(o.get("offer_id")),
                        "label": f"Offer: {title or o.get('session') or o.get('offer_id')}",
                        "description": f"left for you, expires {surfaces.when(o.get('expires_at'))}",
                    }
                )
        now = time.time()
        for s in choices.get("sessions") or ():
            age = now - float(s.get("last_active_at") or now)
            mins = int(max(0, age) // 60)
            ago = f"{mins // 60} h" if mins >= 60 else f"{mins} min"
            options.append(
                {
                    "value": "session:" + str(s.get("id")),
                    "label": f"{s.get('title') or s.get('id')} · {ago} ago · {s.get('message_count', '?')} messages",
                    "description": str(s.get("cwd") or ""),
                }
            )
        if not options:
            raise NotFound(f"{peer.name} has no session to take.", peer=peer.name)
        if call.asker is None:
            lines = [f"Name the session: /handover take {peer.name} <session-id> (or --latest, --offer <id>)."]
            lines += [f"  {o['value'].split(':', 1)[1]} — {o['label']}" for o in options]
            raise BadRequest("\n".join(lines), field="session")
        picked = call.asker(f"Which session from {peer.name}?", options)
        if not picked:
            return None
        kind, _, ident = str(picked).partition(":")
        return SessionRef(offer_id=ident) if kind == "offer" else SessionRef(id=ident)

    def _verb_take(self, parsed: surfaces.Parsed, call: Call) -> None:
        peer, sid = self._resolve_take(parsed, call)
        if peer.direction != "dial":
            raise BadRequest(
                f"{peer.name} dials in to this gateway; take works from the device that dials. On {peer.name} run "
                f"/handover to {self.backend.local_name()}.",
                peer=peer.name,
            )
        if Right.PULL not in peer.rights:
            raise Forbidden("pull not granted", right="pull", peer=peer.name)
        if parsed.args.get("latest"):
            ref = SessionRef(latest=True)
        elif parsed.args.get("offer"):
            ref = SessionRef(offer_id=str(parsed.args["offer"]))
        elif sid:
            ref = SessionRef(id=sid)
        else:
            picked = self._pick_remote(peer, call)
            if picked is None:
                call.say("Nothing chosen. Nothing was locked.")
                return
            ref = picked
        self._start(call, Mode.PULL, peer, parsed, session_ref=ref, bundle_path=None)

    def _verb_export(self, parsed: surfaces.Parsed, call: Call) -> None:
        sid = self._need_session(parsed, call)
        peer = self._peer(parsed.args["for_peer"]) if parsed.args.get("for_peer") else None
        path = parsed.args.get("path")
        self._start(
            call,
            Mode.EXPORT,
            peer,
            parsed,
            session_ref=SessionRef(id=sid),
            bundle_path=os.path.expanduser(path) if path else None,
        )

    def _verb_import(self, parsed: surfaces.Parsed, call: Call) -> None:
        bundle = os.path.expanduser(str(parsed.args["bundle"]))
        if not os.path.exists(bundle):
            raise NotFound(f"No bundle at {bundle}. Give the path of the .ctxh.tar file (or the bundle folder).")
        self._start(call, Mode.IMPORT, None, parsed, session_ref=SessionRef(), bundle_path=bundle)

    def _own_jobs(self, status: dict, call: Call) -> dict:
        if call.local:
            return status
        jobs = [j for j in status.get("jobs") or () if call.session_id and j.get("calling_session") == call.session_id]
        return {**status, "jobs": jobs, "pulled_by": [], "pull_log": [], "outbox_pending": 0, "disk": {}}

    def _verb_status(self, parsed: surfaces.Parsed, call: Call) -> None:
        backend = self.backend
        flush = backend.flush_outbox(getattr(self.ctx, "call_mcp", None))
        job = parsed.args.get("job")
        status = dict(backend.job_status(job))
        if job and not call.local:
            owner = (status.get("job") or status).get("calling_session")
            if not call.session_id or owner != call.session_id:
                call.say(REFUSAL)
                call.exit_code = EXIT_USAGE
                return
        if call.session_id:
            status["session"] = backend.session_state(call.session_id)
        status["ctx"] = {**(status.get("ctx") or {}), **(flush or {})}
        status = self._own_jobs(status, call) if not job else status
        call.data["status"] = status
        if job:
            jobs = [status["job"]] if isinstance(status.get("job"), Mapping) else status.get("jobs") or []
            status = {**status, "jobs": jobs}
        call.say(surfaces.render_status(status))
        if job:
            entry = (status.get("jobs") or [{}])[0]
            call.say(surfaces.render_job_details(entry if isinstance(entry, Mapping) else {}))

    def _verb_peers(self, parsed: surfaces.Parsed, call: Call) -> None:
        backend = self.backend
        sub = parsed.args.get("sub", "list")
        names = parsed.args.get("names") or []
        if sub == "list":
            call.say(surfaces.render_peers(backend.peers(include_revoked=True)))
            return
        if sub == "show":
            call.say(surfaces.render_peers([self._peer(names[0])]))
            return
        if sub == "revoke":
            peer = self._peer(names[0])
            if peer.direction != "inbound":
                raise BadRequest(
                    f"{peer.name} is a gateway this device dials. Use /handover peers remove {peer.name} here; "
                    f"revoke this device on {peer.name} itself.",
                    peer=peer.name,
                )
            aborted = backend.revoke_peer(peer.name)
            call.say(
                f"Revoked {peer.name}: its key no longer works here; {len(aborted)} unfinished handovers with it were "
                "stopped. If the device is lost, also change this gateway's dashboard token "
                "(HERMES_DASHBOARD_SESSION_TOKEN) — the device may have stored it."
            )
            return
        if sub == "remove":
            peer = self._peer(names[0])
            backend.remove_peer(peer.name)
            call.say(f"Removed {peer.name} from this device. Pair again to use it.")
            return
        if sub == "rename":
            old, new = names
            if not backend.rename_peer(old, new):
                raise ConflictError(f"Could not rename {old!r} to {new!r} (unknown name or {new!r} exists).")
            call.say(f"Renamed {old} to {new}.")

    def _listen_url(self) -> str | None:
        settings = self.backend.settings()
        if settings.get("listen_url"):
            return str(settings["listen_url"]).rstrip("/")
        config = self.backend.config()
        for section in ("webapp", "dashboard"):
            sec = config.get(section) if isinstance(config, Mapping) else None
            url = sec.get("public_url") if isinstance(sec, Mapping) else None
            if isinstance(url, str) and url.strip():
                return url.strip().rstrip("/")
        return None

    def _verb_pair(self, parsed: surfaces.Parsed, call: Call) -> None:
        backend = self.backend
        args = parsed.args
        if args.get("pair_string"):
            gate = surfaces.gate_from_settings(
                backend.settings(), {"gate": args.get("gate")} if args.get("gate") else {}
            )
            missing = self._missing_gate_secret(gate)
            if missing:
                raise BadRequest(missing, field="peer_token")
            peer = backend.redeem_pair(
                str(args["pair_string"]),
                name=args.get("name"),
                gate=gate,
                allow_insecure=bool(args.get("allow_insecure")),
            )
            rights = ",".join(sorted(str(r) for r in peer.rights))
            call.say(
                f"Paired with {peer.name} (rights {rights}, key {surfaces.fingerprint_of(peer.pubkey)}). "
                f"/handover take {peer.name} brings a session here; /handover to {peer.name} sends this one there."
            )
            return
        url = self._listen_url()
        if not url:
            raise BadRequest(
                "Set 'Address other devices use to reach this gateway' (listen_url) in Settings → Plugins → "
                "ctx_handover — the URL the other device uses for this gateway — then run /handover pair again.",
                field="listen_url",
            )
        rights = args.get("rights") or frozenset({Right.PUSH, Right.PULL})
        ttl = int(args.get("ttl_s") or 600)
        code = backend.create_pair_code(rights=rights, name_hint=str(args.get("name") or ""), ttl_s=ttl, url=url)
        call.data["pair"] = {k: v for k, v in code.items() if k != "pair_string"}
        minutes = max(1, ttl // 60)
        call.say(
            "\n".join(
                [
                    f"Pairing code (valid {minutes} min, single use, rights "
                    f"{','.join(sorted(str(r) for r in rights))}).",
                    "On the other device run:",
                    f"/handover pair {code['pair_string']}",
                    "Its ctx_handover settings need this gateway's dashboard token (or Basic login) under "
                    "'Dashboard token of the other gateway' — the pairing string never contains it.",
                    f"This gateway's key: {code.get('fingerprint', '?')}",
                ]
            )
        )

    def _missing_gate_secret(self, gate: GateAuth) -> str | None:
        where = "Settings → Plugins → ctx_handover"
        if gate.mode in (GateMode.SESSION_TOKEN, GateMode.BEARER):
            if not hostcompat.read_secret(gate.token_env):
                return (
                    f"Fill in 'Dashboard token of the other gateway' in {where} (stored as {gate.token_env} in this "
                    "profile's .env), then paste the pairing string again."
                )
        elif gate.mode is GateMode.BASIC_LOGIN and (not gate.username or not hostcompat.read_secret(gate.password_env)):
            return (
                f"Fill in 'Dashboard user name' and 'Dashboard password' in {where} (basic_login), then paste "
                "the pairing string again."
            )
        return None

    def _verb_reclaim(self, parsed: surfaces.Parsed, call: Call) -> None:
        sid = self._need_session(parsed, call)
        result = self.backend.reclaim(sid, force=bool(parsed.args.get("force")))
        call.data["reclaim"] = result
        message = result.get("message") or (
            "Reclaimed: this session is active here again."
            if result.get("state") == "reclaimed"
            else "Nothing to reclaim."
        )
        call.say(str(message))
        if result.get("target_notified"):
            call.say("The other gateway was told that this session is active here again.")
        for warning in result.get("warnings") or ():
            call.say(f"warning: {warning}")

    def _job_for(self, parsed: surfaces.Parsed, call: Call) -> str:
        job = parsed.args.get("job")
        if job:
            return str(job)
        status = self.backend.job_status(None)
        active = [j for j in status.get("jobs") or () if j.get("active")]
        mine = [j for j in active if call.session_id and j.get("calling_session") == call.session_id]
        pick = mine or active
        if len(pick) == 1:
            return str(pick[0]["job_id"])
        if not pick:
            raise NotFound("No running handover.")
        raise BadRequest("Several handovers run; name one: " + ", ".join(surfaces.short_job(j["job_id"]) for j in pick))

    def _verb_cancel(self, parsed: surfaces.Parsed, call: Call) -> None:
        job = self._job_for(parsed, call)
        result = self.backend.cancel(job)
        call.say(str(result) or f"Cancel requested for {surfaces.short_job(job)}.")

    def _verb_resume(self, parsed: surfaces.Parsed, call: Call) -> None:
        job = parsed.args.get("job")
        if not job:
            status = self.backend.job_status(None)
            suspended = [j for j in status.get("jobs") or () if j.get("state") == "suspended"]
            if len(suspended) != 1:
                raise BadRequest("Name the suspended handover: /handover resume <job> (/handover status lists them).")
            job = suspended[0]["job_id"]
        self.backend.resume(str(job))
        call.say(f"Resumed {surfaces.short_job(job)}; /handover status {surfaces.short_job(job)} shows progress.")

    def _verb_identity(self, parsed: surfaces.Parsed, call: Call) -> None:
        if parsed.args.get("sub") == "rotate":
            ident = self.backend.rotate_identity()
            call.say(f"New key {ident.get('fingerprint')}. Every paired device must pair again with this one.")
            return
        ident = self.backend.identity(create=False)
        if not ident:
            call.say("No handover key yet. It is created when you pair (/handover pair).")
            return
        call.say(f"This device: instance {ident.get('instance_id')}, key {ident.get('fingerprint')}.")

    def _verb_gc(self, parsed: surfaces.Parsed, call: Call) -> None:
        result = self.backend.gc(job_id=parsed.args.get("job"), all_jobs=bool(parsed.args.get("all")))
        call.data["gc"] = result
        call.say(
            f"Freed {surfaces.human_bytes(result.get('freed_bytes', 0))}"
            + (
                f"; removed everything of {result.get('jobs', 0)} jobs"
                if parsed.args.get("job") or parsed.args.get("all")
                else ""
            )
            + "."
        )

    def _verb_logs(self, parsed: surfaces.Parsed, call: Call) -> None:
        job = str(parsed.args["job"])
        if not call.local:
            owner = (self.backend.job_status(job).get("job") or {}).get("calling_session")
            if not call.session_id or owner != call.session_id:
                call.say(REFUSAL)
                call.exit_code = EXIT_USAGE
                return
        events = self.backend.logs(job)
        if not events:
            call.say(f"No journal entries for {surfaces.short_job(job)}.")
            return
        for ev in events[-50:]:
            call.say(
                f"{surfaces.when(ev.get('ts'))} {ev.get('kind', '')} {ev.get('step') or ''} {ev.get('state') or ''} "
                f"{json.dumps(ev.get('data') or {}, ensure_ascii=False, sort_keys=True)[:300]}".strip()
            )

    def _verb_setup(self, parsed: surfaces.Parsed, call: Call) -> None:
        backend = self.backend
        settings = backend.settings()
        lines: list[str] = []
        if call.asker is not None:
            current = settings.get("inbound", "auto")
            choice = call.asker(
                f"Accept handovers from paired devices? (now: {current})",
                [
                    {"value": "auto", "label": "auto — only while a device is paired"},
                    {"value": "on", "label": "on — always answer"},
                    {"value": "off", "label": "off — never"},
                ],
            )
            if choice in {"auto", "on", "off"} and choice != current:
                backend.set_setting("inbound", choice)
                lines.append(f"inbound: {choice}")
        lines.append(
            f"Received workspaces go to {settings.get('workspace_root')} (setting workspace_root; "
            f"writes only under {', '.join(settings.get('allow_roots') or [])})."
        )
        server = ctxrecords.detect(backend.config())
        if server is None:
            lines.append("ctx: not in use (ctx_checkpoint is not the memory provider) — handovers work without it.")
        elif call.asker is not None:
            answer = call.asker(
                "ctx_checkpoint is your memory provider. Record one redacted note per handover in ctx "
                f"(server {server})?",
                [{"value": "yes", "label": "Yes, record handovers in ctx"}, {"value": "no", "label": "No"}],
            )
            if answer == "yes":
                backend.grant_ctx(server)
                backend.set_setting("ctx_persist", "auto")
                lines.append(f"ctx: handovers are recorded through MCP server {server}.")
            elif answer == "no":
                backend.set_setting("ctx_persist", "off")
                lines.append("ctx: off.")
        else:
            lines.append(
                "ctx: answer this question in an interactive session (/handover setup) to record handovers in "
                f"{server}."
            )
        url = self._listen_url()
        lines += _gate_advice(url)
        call.say("\n".join(lines))

    def _verb_doctor(self, parsed: surfaces.Parsed, call: Call) -> None:
        checks = local_checks(self.ctx, self.backend)
        try:
            checks += self.backend.doctor_checks()
        except HandoverError as err:
            checks.append({"name": "engine", "ok": False, "detail": surfaces.render_error(err, None)})
        call.data["checks"] = checks
        for c in checks:
            mark = "ok" if c.get("ok") is True else "FAIL" if c.get("ok") is False else "warn"
            text = f"[{mark}] {c.get('name')}: {c.get('detail', '')}"
            if c.get("remedy") and c.get("ok") is not True:
                text += f" → {c['remedy']}"
            call.say(text)
        if any(c.get("ok") is False for c in checks):
            call.exit_code = EXIT_FAILED


# ---------------------------------------------------------------------------------------------------------
# Doctor: checks that need no engine (DESIGN §14.1 doctor, R17, SEC 1.1)
# ---------------------------------------------------------------------------------------------------------


def local_checks(ctx: Any, backend: Backend) -> list[dict]:
    """Host capabilities, runtime imports, credential exposure and approvals, without touching the engine."""
    checks: list[dict] = []
    try:
        feats = hostcompat.features(ctx)
        checks.append(
            {
                "name": "Hermes session store",
                "ok": feats.session_db_ok,
                "detail": "all required SessionDB methods present"
                if feats.session_db_ok
                else "missing: " + ", ".join(feats.missing),
                "remedy": "update Hermes to 0.22.6 or newer",
            }
        )
        checks.append(
            {
                "name": "progress and questions",
                "ok": True if feats.post_message else None,
                "detail": f"post_message {'yes' if feats.post_message else 'no'}, "
                f"ask_choice {'yes' if feats.ask_choice else 'no'}",
            }
        )
        if feats.host_isolated:
            checks.append(
                {
                    "name": "plugin isolation",
                    "ok": None,
                    "detail": "plugins.isolation: host — no `hermes handover` CLI (use the slash form), no "
                    "progress notices, and the core buffers request bodies before this plugin's limit",
                    "remedy": "plugins.isolation: in_process, or a proxy body limit in front of the dashboard",
                }
            )
    except Exception as exc:
        checks.append({"name": "Hermes", "ok": False, "detail": f"capability probe failed ({type(exc).__name__})"})
    for module in ("cryptography", "httpx", "compression.zstd"):
        try:
            found = importlib.util.find_spec(module) is not None
        except (ImportError, ValueError):
            found = False
        checks.append(
            {
                "name": f"import {module}",
                "ok": found,
                "detail": "available" if found else "missing",
                "remedy": "this Hermes runtime lacks a core dependency; reinstall Hermes",
            }
        )
    settings = backend.settings()
    config = backend.config()
    peer_names = ", ".join(p.name for p in backend.peers() if p.direction == "dial") or "the other gateway"
    for key, env in surfaces.SECRET_ENVS.items():
        try:
            stored = bool(hostcompat.read_secret(env))
        except HandoverError:
            stored = False
        if stored:
            what = "dashboard token" if key == "peer_token" else "dashboard password"
            checks.append(
                {
                    "name": f"stored {what}",
                    "ok": None,
                    "detail": f"{env} opens the whole dashboard of {peer_names} (configuration, secrets, "
                    "terminal), not only this plugin; a stolen .env gives full access there",
                    "remedy": "put a reverse proxy in front of the other gateway that passes only "
                    "/api/plugins/ctx_handover/ and checks its own header (peer_extra_headers)",
                }
            )
    try:
        env_path = Path(hostcompat.hermes_paths().home) / ".env"
        mode = stat.S_IMODE(env_path.stat().st_mode)
        if mode & 0o077:
            checks.append(
                {
                    "name": "profile .env permissions",
                    "ok": None,
                    "detail": f"{env_path} is readable by other users (mode {mode:o}); it holds this device's "
                    "handover key and any stored dashboard token",
                    "remedy": f"chmod 600 {env_path}",
                }
            )
        else:
            checks.append({"name": "profile .env permissions", "ok": True, "detail": f"mode {mode:o}"})
    except FileNotFoundError:
        checks.append({"name": "profile .env permissions", "ok": True, "detail": "no .env yet"})
    except Exception as exc:
        checks.append({"name": "profile .env permissions", "ok": None, "detail": f"not checked ({type(exc).__name__})"})
    if settings.get("allow_remote_continue"):
        try:
            off = hostcompat.approvals_off(config)
        except Exception:
            off = False
        checks.append(
            {
                "name": "allow_remote_continue",
                "ok": None if off else True,
                "detail": "on, but approvals are off on this gateway, so no remote continuation ever starts"
                if off
                else "on: a paired device with --continue may start one turn here (approvals apply)",
            }
        )
    server = ctxrecords.detect(config)
    if server is not None and settings.get("ctx_persist") != "off":
        entry = (
            (((config.get("plugins") or {}).get("entries") or {}).get("ctx_handover") or {})
            if isinstance(config.get("plugins"), Mapping)
            else {}
        )
        allow = entry.get("mcp_allowlist") if isinstance(entry, Mapping) else None
        granted = isinstance(allow, list) and server in allow
        checks.append(
            {
                "name": "ctx records",
                "ok": True if granted else None,
                "detail": f"MCP server {server} {'allowed' if granted else 'not in mcp_allowlist'}",
                "remedy": "run /handover setup and allow ctx recording, or set ctx_persist: off",
            }
        )
    return checks


def _gate_advice(url: str | None) -> list[str]:
    """What the dialing device must fill in for this gateway's dashboard gate (setup, BX H1)."""
    if not url:
        return [
            "Address: not set. Fill 'Address other devices use to reach this gateway' (listen_url) in Settings → "
            "Plugins → ctx_handover before pairing."
        ]
    parsed = urllib.parse.urlparse(url)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        return [f"Address {url!r} is not an http(s) URL; fix listen_url."]
    lines = [f"Address for pairing: {url}"]
    try:
        with urllib.request.urlopen(url + "/api/auth/providers", timeout=5) as resp:  # noqa: S310 - http(s) checked above
            body = json.loads(resp.read(65536).decode("utf-8", "replace"))
        providers = [p for p in body.get("providers") or () if isinstance(p, Mapping)]
    except urllib.error.HTTPError as exc:
        providers = None
        if exc.code == 404:
            lines.append("The address does not look like a Hermes dashboard (no /api/auth/providers).")
    except (OSError, ValueError):
        providers = None
        lines.append("The address did not answer from here; check it from the other device.")
    if providers is not None:
        if any(p.get("supports_password") for p in providers):
            lines.append(
                "Its dashboard uses a Basic login: on the other device choose peer_gate basic_login and "
                "fill in the dashboard user name and password."
            )
        else:
            lines.append(
                "Its dashboard signs in with a fixed token: set HERMES_DASHBOARD_SESSION_TOKEN on this "
                "gateway (then restart `hermes serve` and the gateway) and enter the same token on the other "
                "device as 'Dashboard token of the other gateway'."
            )
    lines.append(
        "That credential opens this whole dashboard; a reverse proxy that passes only "
        "/api/plugins/ctx_handover/ limits it."
    )
    return lines


def _tty_asker() -> Callable[[str, list], str | None] | None:
    """An ``ask_choice``-like prompt on the terminal; None without a TTY (then nothing interactive happens)."""
    try:
        if not (sys.stdin.isatty() and sys.stdout.isatty()):
            return None
    except (AttributeError, ValueError):
        return None

    def ask(question: str, choices: list) -> str | None:
        values = []
        print(question)
        for i, c in enumerate(choices, 1):
            value = c["value"] if isinstance(c, Mapping) else str(c)
            label = c.get("label", value) if isinstance(c, Mapping) else str(c)
            values.append(value)
            print(f"  {i}) {label}")
        try:
            raw = input("Choice: ").strip()
        except (EOFError, KeyboardInterrupt):
            return None
        if raw.isdigit() and 1 <= int(raw) <= len(values):
            return values[int(raw) - 1]
        return raw if raw in values else None

    return ask


# ---------------------------------------------------------------------------------------------------------
# Entry points named by INTERFACES §4.6
# ---------------------------------------------------------------------------------------------------------

_PLUGINS: weakref.WeakKeyDictionary = weakref.WeakKeyDictionary()
_PLUGINS_LOCK = threading.Lock()


def plugin_for(ctx: Any) -> Plugin:
    """The one ``Plugin`` of this ``ctx`` (no I/O)."""
    with _PLUGINS_LOCK:
        try:
            plugin = _PLUGINS.get(ctx)
        except TypeError:
            return Plugin(ctx)
        if plugin is None:
            plugin = Plugin(ctx)
            try:
                _PLUGINS[ctx] = plugin
            except TypeError:
                pass
        return plugin


def slash_handler(ctx: Any) -> Callable[..., str]:
    """``(raw_args, session_id=None, session_key=None, platform=None, source=None) -> str``."""
    return plugin_for(ctx).slash


def cli_setup(subparser: argparse.ArgumentParser) -> None:
    """``hermes handover <verb> [args …]`` — one grammar with the slash form (``surfaces.parse``)."""
    subparser.description = (
        "Move a Hermes session and its working files to another gateway you paired (pull, push or bundle). "
        "Verbs: " + ", ".join(surfaces.VERBS) + ". Add --json for scripts. Exit codes: 0 ok, 1 failed, 2 usage, "
        "3 busy/conflict, 4 auth/gate, 5 in-doubt/suspended."
    )
    subparser.add_argument("verb", nargs="?", default="help", help="to, take, export, import, status, …")
    subparser.add_argument("rest", nargs=argparse.REMAINDER, help="arguments of the verb (see /handover help)")


def cli_handler(ctx: Any) -> Callable[[argparse.Namespace], int]:
    return plugin_for(ctx).cli


# ---------------------------------------------------------------------------------------------------------
# The engine behind the surfaces (layer-2 integration; written against INTERFACES §4, see reports/F.md)
# ---------------------------------------------------------------------------------------------------------


class _VetoAdapter:
    """``HookRuntime`` over ``lock.VetoIndex`` (in-memory lookups) and the ledger's note calls, which run on one
    lazily started writer thread so ``pre_llm_call`` never waits for SQLite (DESIGN §7.6, §11.4)."""

    def __init__(self, veto: Any, ledger: Any, veto_texts: Callable[[dict], str]) -> None:
        self._veto = veto
        self._ledger = ledger
        self._veto_texts = veto_texts
        self._queue: list[Callable[[], None]] = []
        self._cond = threading.Condition()
        self._thread: threading.Thread | None = None

    def lookup(self, session_id: str, parent_session_id: str | None = None) -> dict | None:
        return self._veto.lookup(session_id, parent_session_id)

    def veto_text(self, entry: dict) -> str:
        return self._veto_texts(entry)

    def arrival(self, session_id: str) -> dict | None:
        return self._veto.arrival(session_id)

    def on_compaction(self, session_id: str, old_session_id: str) -> None:
        self._veto.on_compaction(session_id, old_session_id)

    def _submit(self, fn: Callable[[], None]) -> None:
        with self._cond:
            self._queue.append(fn)
            if self._thread is None:
                self._thread = threading.Thread(target=self._drain, name="ctx_handover-notes", daemon=True)
                self._thread.start()
            self._cond.notify()

    def _drain(self) -> None:
        while True:
            with self._cond:
                while not self._queue:
                    self._cond.wait()
                fn = self._queue.pop(0)
            try:
                fn()
            except Exception:
                logger.debug("ctx_handover: note state write failed", exc_info=True)

    def note_offered(self, session_id: str, turn_id: str) -> None:
        self._submit(lambda: self._ledger.note_offer(session_id, turn_id))

    def note_delivered(self, session_id: str) -> None:
        self._submit(lambda: self._ledger.note_delivered(session_id))


class EngineBackend:
    """``Backend`` over the engine and Hermes. Every engine module is imported lazily inside a method, so
    ``register()`` and the unit tests never load them; ``hostcompat.<fn>`` is reached as a module attribute.

    The profile runtime is cluster A's: ``jobs.ensure_started(ctx)`` returns the profile (``ledger``, ``store``,
    ``veto``, ``identity``) and ``jobs.runner_for(ctx, reporter_factory)`` its one ``JobRunner``; this class builds
    no second ledger, block store or veto index. Revoke and reclaim go through ``jobs.revoke_peer`` /
    ``jobs.reclaim`` (W067, W047).
    """

    def __init__(self, ctx: Any, plugin: Plugin) -> None:
        self._ctx = ctx
        self._plugin = plugin
        self._lock = threading.RLock()
        self._parts: dict[str, Any] = {}

    # -- composition -----------------------------------------------------------------------------------
    def _data_dir(self) -> Path:
        return hostcompat.plugin_dir()

    def _part(self, name: str, build: Callable[[], Any]) -> Any:
        with self._lock:
            if name not in self._parts:
                self._parts[name] = build()
            return self._parts[name]

    def _started(self) -> Any:
        def build() -> Any:
            from . import jobs

            # The runner first: ensure_started's janitor would otherwise create the profile's runner without a
            # reporter factory, and progress of every later job would go nowhere (runner_for keeps the first).
            jobs.runner_for(self._ctx, self._plugin.broker.factory)
            return jobs.ensure_started(self._ctx)

        return self._part("runtime", build)

    def _ledger(self) -> Any:
        return self._started().ledger

    def _runner(self) -> Any:
        def build() -> Any:
            from . import jobs

            self._started()
            return jobs.runner_for(self._ctx, self._plugin.broker.factory)  # one runner + janitor per profile

        return self._part("runner", build)

    def _store(self) -> Any:
        return self._started().store

    def _spawner(self) -> Spawner:
        def build() -> Spawner:
            profile = hostcompat.host_facts().profile
            return Spawner(
                log_dir=self._data_dir() / "logs",
                profile=None if profile == "default" else profile,
                approvals_off=lambda: hostcompat.approvals_off(hostcompat.load_config()),
                allow_remote=lambda: bool(self.settings().get("allow_remote_continue")),
            )

        return self._part("spawner", build)

    def _identity(self, *, create: bool) -> Any:
        from .crypto import Identity

        ident = Identity.load(
            self._ledger(), read_secret=hostcompat.read_secret, read_env_fresh=hostcompat.read_env_fresh
        )
        if ident is None and create:
            ident = Identity.create(
                self._ledger(),
                env_writable=hostcompat.env_writable,
                save_env=hostcompat.save_env,
                read_env_fresh=hostcompat.read_env_fresh,
            )
        return ident

    def _client(self, peer: Peer) -> Any:
        from .wire import WireClient

        ident = self._identity(create=False)
        if ident is None:
            raise BadRequest("This device has no handover key. Pair with the other gateway first (/handover pair).")
        return WireClient(peer, ident, read_secret=hostcompat.read_secret)

    # -- Backend ---------------------------------------------------------------------------------------
    def settings(self) -> dict:
        return surfaces.settings_from_config(hostcompat.load_config())

    def config(self) -> dict:
        return hostcompat.load_config()

    def local_name(self) -> str:
        import socket

        return (socket.gethostname() or "this-gateway").split(".")[0][:64]

    def peers(self, *, include_revoked: bool = False) -> list[Peer]:
        return list(self._ledger().peers(include_revoked=include_revoked))

    def peer(self, name: str) -> Peer | None:
        return self._ledger().peer(name)

    def revoke_peer(self, name: str) -> list[str]:
        from . import jobs

        self._started()
        return list(jobs.revoke_peer(name))  # ledger cascade + abort of the peer's participant jobs (W067)

    def remove_peer(self, name: str) -> bool:
        return bool(self._ledger().remove_peer(name))

    def rename_peer(self, old: str, new: str) -> bool:
        return bool(self._ledger().rename_peer(old, new))

    def create_pair_code(self, *, rights: frozenset[Right], name_hint: str, ttl_s: int, url: str) -> dict:
        from . import pairing

        ident = self._identity(create=True)
        code = pairing.create_code(self._ledger(), rights=rights, name_hint=name_hint, ttl_s=ttl_s)
        profile = hostcompat.host_facts().profile
        text = pairing.pair_string(
            code,
            url=url,
            fingerprint=ident.fingerprint(),
            name=self.local_name(),
            profile=None if profile == "default" else profile,
        )
        return {"pair_string": text, "fingerprint": ident.fingerprint(), "expires_at": code.expires_at, "url": url}

    def redeem_pair(self, pair_string: str, *, name: str | None, gate: GateAuth, allow_insecure: bool) -> Peer:
        from . import pairing
        from .wire import WireClient

        pair = pairing.parse_pair_string(pair_string)
        ident = self._identity(create=True)
        return pairing.redeem_dialer(
            lambda peer: WireClient(peer, ident, read_secret=hostcompat.read_secret),
            ident,
            pair,
            name=name or str(pair.get("name") or "server"),
            gate=gate,
            allow_insecure=allow_insecure,
            ledger=self._ledger(),
        )

    def identity(self, *, create: bool) -> dict | None:
        ident = self._identity(create=create)
        if ident is None:
            return None
        return {"instance_id": ident.instance_id, "fingerprint": ident.fingerprint()}

    def rotate_identity(self) -> dict:
        from .crypto import Identity

        rt = self._started()
        ident = Identity.create(
            rt.ledger,
            env_writable=hostcompat.env_writable,
            save_env=hostcompat.save_env,
            read_env_fresh=hostcompat.read_env_fresh,
            rotate=True,
        )
        # the profile caches its identity for the runner and the pull client; the old key must not sign again
        with rt.lock:
            rt.identity = ident
            rt.identity_error = None
        return {"instance_id": ident.instance_id, "fingerprint": ident.fingerprint()}

    def start_job(
        self,
        mode: Mode,
        peer: Peer | None,
        options: Options,
        *,
        session_ref: SessionRef,
        calling_session: str | None,
        session_key: str | None,
        bundle_path: str | None,
        confirm: Callable[[dict], str] | None,
        side_question: Callable[[str], str | None] | None,
        reporter: Any,
    ) -> tuple[str, FinalPlan | None]:
        self._flush_quietly()
        job_id, final = self._runner().plan_sync(
            mode,
            peer,
            options,
            session_ref=session_ref,
            calling_session=calling_session,
            session_key=session_key,
            ctx=self._ctx,
            confirm=confirm,
            bundle_path=Path(bundle_path) if bundle_path is not None else None,
            # only flags typed here: this surface is local (R24); a peer's include_secret never gets here (X07)
            include_secret=tuple(options.hints.include_secret),
            side_question=side_question,
        )
        if final is None and options.dry_run:
            final = self._ended_plan(job_id)  # a dry run ends after NEGOTIATED with nothing locked (W113)
        return job_id, final

    def _ended_plan(self, job_id: str) -> FinalPlan | None:
        """The plan a job ended with before anything was locked (``result.plan`` of the conductor row)."""
        from .jsonutil import from_wire
        from .model import Role

        row = self._ledger().job(job_id, Role.CONDUCTOR) or {}
        raw = (row.get("result") or {}).get("plan")
        if not isinstance(raw, Mapping):
            return None
        try:
            return from_wire(FinalPlan, dict(raw))
        except (HandoverError, TypeError, ValueError):
            return None

    def continue_async(self, job_id: str) -> None:
        self._runner().continue_async(job_id)

    def run_foreground(self, job_id: str) -> int:
        return int(self._runner().run_foreground(job_id))

    def cancel(self, job_id: str) -> str:
        return str(self._runner().cancel(job_id))

    def resume(self, job_id: str) -> None:
        self._runner().resume(job_id)

    def job_status(self, job_id: str | None) -> dict:
        status = dict(self._runner().status(job_id) or {})
        if job_id is None and status.get("pull_log"):
            ledger = self._ledger()
            names: dict[str, str] = {}
            for row in status["pull_log"]:
                pid = str(row.get("peer_id") or "")
                if pid and pid not in names:
                    peer = ledger.peer(pid)
                    names[pid] = peer.name if peer is not None else pid
            status["pulled_by"] = [
                {"peer": names.get(str(r.get("peer_id") or ""), "?"), "at": r.get("at"), "items": r.get("items") or []}
                for r in status["pull_log"]
            ]
        if job_id and isinstance(status.get("job"), Mapping):
            cont = self._spawner().result(job_id)
            if cont.get("started"):
                status["job"] = {**status["job"], "continuation": {**(status["job"].get("continuation") or {}), **cont}}
        return status

    def create_offer(
        self,
        peer: Peer,
        session_ref: SessionRef,
        options: Options,
        *,
        side_question: Callable[[str], str | None] | None,
    ) -> dict:
        return dict(
            self._runner().create_offer(peer, session_ref, ctx=self._ctx, options=options, side_question=side_question)
        )

    def remote_choices(self, peer: Peer, *, limit: int) -> dict:
        client = self._client(peer)
        sessions = client.call("POST", "sessions/list", {"limit": int(limit)}).get("sessions") or []
        try:
            offers = client.call("POST", "offers/list", {}).get("offers") or []
        except HandoverError:
            offers = []
        return {"sessions": list(sessions), "offers": list(offers)}

    def reclaim(self, session_id: str, *, force: bool) -> dict:
        from . import jobs

        self._started()
        return dict(jobs.reclaim(session_id, force=force) or {})  # tells the target ``source_reclaimed`` (W047)

    def gc(self, *, job_id: str | None, all_jobs: bool) -> dict:
        import shutil

        from .model import Role

        ledger = self._ledger()
        store = self._store()
        data = self._data_dir()
        jobs: list[str] = []
        if job_id:
            jobs = [job_id]
        elif all_jobs:
            seen = {str(j.get("job_id")) for role in Role for j in ledger.jobs(role=role, active=False, limit=100000)}
            jobs = sorted(j for j in seen if j)
        for jid in jobs:
            if not jid.isalnum():
                continue
            ledger.purge_job(jid)
            store.release_job(jid, keep=False)
            for sub in ("staging", "quarantine"):
                target = data / sub / jid
                if target.is_dir() and not target.is_symlink():
                    shutil.rmtree(target, ignore_errors=True)
            for path in (
                data / "reports" / f"{jid}.md",
                data / "logs" / f"continue-{jid}.log",
                data / "logs" / f"continue-{jid}.json",
            ):
                with contextlib.suppress(FileNotFoundError):
                    path.unlink()
        s = self.settings()
        freed = store.gc(
            keep_days=0 if all_jobs else int(s["block_cache_days"]),
            max_bytes=0 if all_jobs else int(float(s["block_cache_gb"]) * surfaces.GiB),
        )
        return {"freed_bytes": int(freed or 0), "jobs": len(jobs)}

    def logs(self, job_id: str) -> list[dict]:
        from .model import Role

        ledger = self._ledger()
        events: list[dict] = []
        for role in Role:
            for ev in ledger.events(job_id, role) or ():
                data = ev.get("data")
                text = data if isinstance(data, str) else json.dumps(data or {}, ensure_ascii=False, default=str)
                events.append({**ev, "role": str(role), "data": hostcompat.redact(text)[:2000]})
        return sorted(events, key=lambda e: (e.get("ts") or 0, e.get("seq") or 0))

    def session_state(self, session_id: str) -> dict:
        ledger = self._ledger()
        return {"source": ledger.source_by_session(session_id), "arrival": ledger.arrival_for(session_id)}

    def session_source(self, session_id: str) -> str | None:
        db = hostcompat.session_db()
        try:
            row = db.get_session(session_id)
        finally:
            with contextlib.suppress(Exception):
                db.close()
        return str(row.get("source")) if isinstance(row, Mapping) and row.get("source") else None

    def suggestion_put(self, session_id: str, data: dict) -> None:
        self._ledger().suggestion_put(session_id, data)

    def presend_summary(self, final: FinalPlan) -> dict:
        return surfaces.plan_summary(final)  # cluster D's presend_summary/summarize, keyed for rendering (W125)

    def doctor_checks(self) -> list[dict]:
        checks: list[dict] = []
        try:
            from . import gitops

            ok, warning = gitops.check_version(gitops.Git())
            checks.append(
                {
                    "name": "git",
                    "ok": True if ok and not warning else (None if ok else False),
                    "detail": warning or "version is supported",
                    "remedy": "install git 2.38 or newer (and git-lfs for LFS repositories)",
                }
            )
        except Exception as exc:
            checks.append(
                {
                    "name": "git",
                    "ok": False,
                    "detail": f"not usable ({type(exc).__name__})",
                    "remedy": "install git 2.38 or newer",
                }
            )
        try:
            data = self._data_dir()
            mode = stat.S_IMODE(data.stat().st_mode)
            checks.append(
                {
                    "name": "data folder",
                    "ok": (mode & 0o077) == 0,
                    "detail": f"{data} mode {mode:o}",
                    "remedy": f"chmod 700 {data}",
                }
            )
        except Exception as exc:
            checks.append({"name": "data folder", "ok": False, "detail": f"not usable ({type(exc).__name__})"})
        try:
            ident = self._identity(create=False)
            checks.append(
                {
                    "name": "identity",
                    "ok": True if ident is not None else None,
                    "detail": f"key {ident.fingerprint()}" if ident is not None else "no key yet (created by pairing)",
                }
            )
        except HandoverError as err:
            checks.append({"name": "identity", "ok": False, "detail": surfaces.render_error(err, None)})
        settings = self.settings()
        try:
            ledger = self._ledger()
            inbound = [p for p in ledger.peers() if p.direction == "inbound"]
            pending = bool(ledger.pairing_pending(time.time()))
            mode = settings.get("inbound", "auto")
            answering = mode == "on" or (mode == "auto" and (inbound or pending))
            checks.append(
                {
                    "name": "inbound",
                    "ok": True,
                    "detail": f"{mode}: {'answering' if answering else 'hidden (answers like an uninstalled plugin)'}; "
                    f"{len(inbound)} paired device(s) may dial in",
                }
            )
            for peer in (p for p in ledger.peers() if p.direction == "dial"):
                try:
                    client = self._client(peer)
                    probe = client.probe_gate(measure_limit=True)
                    checks.append(peer_check(peer, probe, getattr(client, "clock_skew", None)))
                except HandoverError as err:
                    checks.append(
                        {"name": f"peer {peer.name}", "ok": False, "detail": surfaces.render_error(err, peer.name)}
                    )
        except HandoverError as err:
            checks.append({"name": "ledger", "ok": False, "detail": surfaces.render_error(err, None)})
        return checks

    def _flush_quietly(self) -> None:
        with contextlib.suppress(Exception):
            self.flush_outbox(getattr(self._ctx, "call_mcp", None))

    def flush_outbox(self, call_mcp: Callable[..., dict] | None) -> dict:
        settings = self.settings()
        config = self.config()
        server = ctxrecords.server_for(config, settings)
        if server is None:
            return {}
        ledger = self._ledger()
        stored = 0
        result: dict[str, Any] = {"server": server}
        if not ctxrecords.allowed(config, server):
            result["error"] = f"recording in {server} is not allowed yet; run /handover setup (or set ctx_persist: off)"
        elif callable(call_mcp):
            stored = ctxrecords.flush(
                ledger, call_mcp, server, str(settings.get("ctx_category") or "handover"), time.time()
            )
        return {**result, "stored": stored, "pending": ctxrecords.pending(ledger)}

    def grant_ctx(self, server: str) -> None:
        grant = getattr(hostcompat, "grant_mcp_server", None)
        if not callable(grant):
            raise CapabilityMissing(
                "This build cannot write the MCP allowlist itself. Run: hermes config set "
                f"plugins.entries.ctx_handover.mcp_allowlist '[\"{server}\"]'",
                capability="write mcp_allowlist",
                local=True,
            )
        grant(server)

    def set_setting(self, key: str, value: Any) -> None:
        setter = getattr(self._ctx, "set_config", None)
        if not callable(setter):
            raise CapabilityMissing("Settings cannot be written from here.", capability="set_config", local=True)
        setter(key, value)

    def hook_runtime(self) -> HookRuntime | None:
        from . import lock

        rt = self._started()  # the profile's one VetoIndex (started by ensure_started); never a second one
        return _VetoAdapter(rt.veto, rt.ledger, lock.veto_texts)
