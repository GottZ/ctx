"""Seams between the layers: store lookups for delivery and progress, the per-turn context, and the steps
that run when a Discord bot binds (their order lives in ``bootstrap.BIND_STEPS``).

Everything here reads the bot owner's store through the runtime anchor (``RUNTIME.bots``), so it answers
for every bot and profile of the process; a guild without an active policy gets ``None``/``"default"``.
"""

from __future__ import annotations

import asyncio
import logging
import time
import weakref
from dataclasses import dataclass
from typing import Any, Optional

from . import RUNTIME_API, config, hermes, prompts
from .adapt.status import COMFORT, SECURITY, Outcome, Result
from .adapt.strategies import run_intervention, wrap_attr
from .delivery import delete, progress, split, worklog
from .ingress import hydrate, reactions
from .models import runtime as models_runtime
from .policy import lanes
from .prompts.assemble import SPILL_DEFAULT_MAX_CHARS, SPILL_MARGIN
from .routing import apply as routing_apply
from .runtime import RUNTIME
from .ui import commands as ui_commands
from .ui import items as ui_items

logger = logging.getLogger("ctx_discord_policy")

DELETE_CHAT = "K1b"          # delete through the chat a message really lives in (worklog redirection)
CONTEXT = "A9"               # the per-turn context and its budget (DESIGN §6.2 A9, §7.5)


# ── lookups (process-wide: every bot, every profile) ─────────────────────────────────────────────────


def _active_guild(bot_id: Any, guild_id: Any) -> Optional[dict]:
    binding = RUNTIME.bots.for_bot(str(bot_id or ""))
    if binding is None or not guild_id:
        return None
    snapshot = binding.store.read()
    if snapshot.guild_state(binding.bot_id, str(guild_id)) != "active":
        return None
    return snapshot.guild(binding.bot_id, str(guild_id)) or {}


def guild_config(bot_id: Any, guild_id: Any) -> Optional[dict]:
    """The guild's settings for the delivery layer (K3), only while its policy is active."""
    return _active_guild(bot_id, guild_id)


def progress_grouping(bot_id: Any, guild_id: Any) -> str:
    """``rotate``/``separate``/``default`` for K2/K2b; ``default`` for any guild without an active policy."""
    value = ((guild_config(bot_id, guild_id) or {}).get("progress") or {}).get("grouping", "default")
    return value if value in (progress.ROTATE, progress.SEPARATE) else progress.DEFAULT


# ── the per-turn context (pre_llm_call) ──────────────────────────────────────────────────────────────


def _settings(owner_home: Any) -> dict[str, Any]:
    return {key: config.setting(owner_home, key)
            for key in ("frame_prompt", "operator_label", "guild_prompt_max_chars")}


def _record_if_changed(result: Result) -> bool:
    """Record unless the register already holds this outcome and reason (one warning per change)."""
    current = RUNTIME.status.get(result.intervention)
    if current is not None and current.outcome is result.outcome and current.reason == result.reason:
        return False
    RUNTIME.status.record(result)
    return True


def context_key(route: Any) -> str:
    """Status key of one turn context: per bot, guild, lane and conversation mode. Lanes and modes have texts of
    their own, so one budget can fit a direct turn and shorten an ambient one; a shared key would flap."""
    return f"{CONTEXT}[{route.bot_id}/{route.guild_id}/{route.lane}/{route.mode}]"


def _budget(spill: Any) -> Optional[int]:
    budget = prompts.budget_from_spill(spill)
    if spill is None:
        source = Result(CONTEXT, SECURITY, Outcome.FALLBACK,
                        "Hermes' hook-output spill configuration is not readable: assuming its default limit of "
                        f"{SPILL_DEFAULT_MAX_CHARS} characters",
                        "tools.hook_output_spill.get_spill_config")
    elif budget is None:
        source = Result(CONTEXT, SECURITY, Outcome.OK, "Hermes does not spill hook context: no context budget",
                        "tools.hook_output_spill.get_spill_config")
    else:
        source = Result(CONTEXT, SECURITY, Outcome.OK,
                        f"context budget = Hermes' spill limit - {SPILL_MARGIN}",
                        "tools.hook_output_spill.get_spill_config")
    _record_if_changed(source)
    return budget


def _note_budget(route: Any, assembled: Any) -> None:
    key = context_key(route)
    budget = assembled.budget
    if assembled.over_budget:
        result = Result(key, SECURITY, Outcome.FALLBACK,
                        f"operator frame and lanes alone exceed the context budget of {budget} characters: Hermes "
                        "spills this context to a file and shows the model only a preview; shorten the operator "
                        "frame or the template overrides", "prompts.assemble")
    elif assembled.truncated_by:
        limits = {"max_chars": "the operator's guild_prompt_max_chars", "budget": f"the context budget of {budget}"}
        result = Result(key, SECURITY, Outcome.FALLBACK,
                        "server guidance shortened to fit " + " and ".join(limits[t] for t in assembled.truncated_by),
                        "prompts.assemble")
    else:
        if RUNTIME.status.get(key) is None:
            return  # only contexts that once had a problem get an entry (one per bot, guild, lane and mode)
        within = f"context within budget {budget}" if budget is not None else "context within budget (no limit)"
        result = Result(key, SECURITY, Outcome.OK, within, "prompts.assemble")
    _record_if_changed(result)


def _assemble(route: Any) -> str:
    binding = RUNTIME.bots.for_bot(route.bot_id)
    owner_home = binding.owner_home if binding is not None else hermes.current_home()
    guild = (binding.store.read().guild(route.bot_id, route.guild_id) if binding is not None else None) or {}
    assembled = prompts.assemble(_settings(owner_home), guild, lane=route.lane, mode=route.mode,
                                 capability=lanes.capability_lane_text(route), budget=_budget(hermes.spill_config()),
                                 server_name=route.server_name, channel_name=route.channel_name,
                                 channel_id=route.channel_id, bot_display_name=route.bot_display_name)
    _note_budget(route, assembled)
    return assembled.text


def turn_context(route: Any) -> str:
    """The assembled context of one turn: operator frame, capability lane (from the code catalog),
    conversation lane (the route's mode, passed through as it is), server guidance — within the spill budget.

    A failure raises for public routes (the hook then blocks: fail closed; the context's status entry says so).
    An operator turn falls back to its capability lane alone, so a broken store or template never locks the
    operator out; the failure is reported.
    """
    try:
        return _assemble(route)
    except Exception as exc:
        if not route.is_operator:
            _record_if_changed(Result(
                context_key(route), SECURITY, Outcome.OFF,
                f"turn context could not be assembled ({type(exc).__name__}): public turns are blocked here",
                "prompts.assemble"))
            raise
        changed = _record_if_changed(Result(
            context_key(route), SECURITY, Outcome.FALLBACK,
            f"turn context could not be assembled ({type(exc).__name__}): operator turns get the capability "
            "lane only", "prompts.assemble"))
        logger.log(logging.WARNING if changed else logging.DEBUG,
                   "ctx_discord_policy: turn context failed for an operator turn; capability lane only",
                   exc_info=True)
        return lanes.capability_lane_text(route)


# ── steps when a Discord bot binds (order: bootstrap.BIND_STEPS) ─────────────────────────────────────


@dataclass(frozen=True)
class Bind:
    """One call of the platform-handler factory for a connected bot, after ``RUNTIME.bots.bind``."""

    native: Any               # the discord.py client Hermes hands to the factory
    adapter: Any              # Hermes' Discord adapter instance
    binding: Any              # bots.BotBinding: bot id, owner home, store

    @property
    def adapter_cls(self) -> type:
        return type(self.adapter)


def install_delete(bind: Bind) -> Result:
    """K1: ``delete_message`` where the adapter class only has the core's no-op. Runs first: it must see the
    class as Hermes left it."""
    return delete.install(bind.adapter_cls, RUNTIME.status, adapter=bind.adapter)


def _can_delete(adapter_cls: type) -> bool:
    base = delete.base_adapter_class(adapter_cls)
    no_op = base.__dict__.get(delete.METHOD) if base is not None else None
    current = getattr(adapter_cls, delete.METHOD, None)
    return callable(current) and current is not no_op


def _make_delete_chat(original: Any) -> Any:
    async def policy_delete_message(self: Any, chat_id: Any, message_id: Any, *args: Any, **kwargs: Any) -> Any:
        # A message the delivery layer redirected (worklog) lives in another chat than the caller thinks.
        return await original(self, worklog.message_chat(chat_id, message_id), message_id, *args, **kwargs)

    return policy_delete_message


def _install_delete_chat(adapter_cls: type) -> Result:
    if not _can_delete(adapter_cls):
        k1 = RUNTIME.status.get(delete.INTERVENTION)
        return Result(DELETE_CHAT, COMFORT, Outcome.OFF,
                      f"the adapter cannot delete messages (K1 {k1.outcome.value if k1 else 'not run'})", "K1")
    state = wrap_attr(adapter_cls, delete.METHOD, _make_delete_chat, DELETE_CHAT, RUNTIME_API)
    return Result(DELETE_CHAT, COMFORT, Outcome.WRAPPED,
                  "deletes of redirected messages reach their real chat" + (" (already)" if state == "already" else ""),
                  "worklog.message_chat")


def install_delete_chat(bind: Bind) -> Result:
    """K1b: deletes go to the chat a redirected message really lives in. After K1, on top of whatever K1 found
    or injected (installed earlier, K1 would see a class entry of its own and inject nothing upstream)."""
    return run_intervention(RUNTIME.status, DELETE_CHAT, COMFORT, lambda: _install_delete_chat(bind.adapter_cls))


def install_delivery(bind: Bind) -> list[Result]:
    """K3/K4: worklog/split/media delivery and ambient reactions, with the active-guild lookup."""
    return split.install(bind.adapter_cls, guild_config, RUNTIME.status)


def install_progress(bind: Bind) -> list[Result]:
    """K2/K2b on Hermes' ``TurnRunner`` with the process-wide per-server grouping lookup."""
    return progress.install(hermes.turn_runner_class(), progress_grouping, RUNTIME.status)


def apply_routing(bind: Bind) -> Result:
    """R1 (security): chosen profiles go back into Hermes' running route table where the configuration is
    managed, and the agreement of configuration and policy is reported. After the delivery and progress layers,
    before anything that admits members (reaction wakeup, interface)."""
    try:
        routing_apply.on_bind(bind.adapter, bind.binding)
    except Exception as exc:  # on_bind reports its own outcome; this only catches a broken seam
        logger.exception("ctx_discord_policy: R1 at bind failed")
        return RUNTIME.status.record(Result(routing_apply.INTERVENTION, SECURITY, Outcome.OFF,
                                            f"off: {type(exc).__name__}: {exc}", "R1 bind"))
    return RUNTIME.status.get(routing_apply.INTERVENTION)


def install_models(bind: Bind) -> Result:
    """K8: the per-server model inside Hermes' model resolution (``gateway.run`` is loaded once a bot binds)."""
    return models_runtime.install()


def install_reaction_wakeup(bind: Bind) -> Result:
    """K5 on the bound bot's client (idempotent per client; a new client after a reconnect gets its own
    listeners): the woken message goes through the same gate as every other message."""
    return reactions.install_reaction_wakeup(bind.native, bind.adapter)


def probe_hydration(bind: Bind) -> Result:
    """K7: does the bot's client offer what hydration needs (A6 hydrates per message)?"""
    return hydrate.probe(bind.adapter)


# The interface binding per bot, for messages that start outside an interaction (model unavailable).
_UI: "weakref.WeakValueDictionary[str, Any]" = weakref.WeakValueDictionary()


def install_ui(bind: Bind) -> Any:
    """K6: ``/policy`` on the bound bot, on the bot binding's own store; a binding left on the same client
    (factory called again for it) is detached first, so listeners never double."""
    previous = ui_items.binding_for(bind.native)
    if previous is not None:
        previous.detach()
    ui = ui_commands.install(bind.native, bind.adapter, bot_id=bind.binding.bot_id,
                             owner_home=bind.binding.owner_home, store=bind.binding.store)
    if ui is not None:
        _UI[str(bind.binding.bot_id)] = ui
    return ui


# ── operator messages from outside the interface ─────────────────────────────────────────────────────


def _on_loop(ui: Any, coro: Any) -> bool:
    """Run ``coro`` on the bot client's own event loop (its HTTP session lives there) from wherever this is
    called: K8 runs on Hermes' threads too, some with a loop of their own."""
    try:
        loop = ui.client.loop
    except Exception:
        loop = None
    try:
        running = asyncio.get_running_loop()
    except RuntimeError:
        running = None
    if loop is not None and running is loop:
        ui.notify.spawn(coro)
        return True
    if loop is None or not isinstance(loop, asyncio.AbstractEventLoop) or not loop.is_running():
        coro.close()
        return False
    loop.call_soon_threadsafe(lambda: ui.notify.spawn(coro))
    return True


# (bot, guild, lane) -> time a message about an unavailable model was handed to the bot's loop (in-process
# throttle; ``Notifier.model_unavailable`` keeps the day across restarts).
_MODEL_NOTICES: dict = {}
MODEL_NOTICE_WINDOW = 24 * 3600.0


def model_unavailable(bot_id: str, guild_id: str, lane: str, model: str, reason: str) -> bool:
    """A server's chosen model can't be applied (K8 fell back, or ``on_unavailable: close`` keeps members
    closed): tell the bot's operators, at most once per server, lane and day. Called on every occurrence; one
    that finds no interface yet (K6 not bound, or off) is retried by the next."""
    key = (str(bot_id), str(guild_id), str(lane))
    last = _MODEL_NOTICES.get(key)
    if last is not None and time.monotonic() - last < MODEL_NOTICE_WINDOW:
        return False
    ui = _UI.get(str(bot_id))
    if ui is None:
        return False
    try:
        sent = _on_loop(ui, ui.notify.model_unavailable(guild_id, lane, model, reason))
    except Exception:
        logger.warning("ctx_discord_policy: message about an unavailable model failed", exc_info=True)
        return False
    if sent:
        _MODEL_NOTICES[key] = time.monotonic()
    return sent


models_runtime.UNAVAILABLE_LISTENERS.append(model_unavailable)


__all__ = ["Bind", "CONTEXT", "DELETE_CHAT", "apply_routing", "context_key", "guild_config", "install_delete",
           "install_delete_chat", "install_delivery", "install_models", "install_progress",
           "install_reaction_wakeup", "install_ui", "model_unavailable", "probe_hydration", "progress_grouping",
           "turn_context"]
