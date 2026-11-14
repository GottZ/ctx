"""Seams between the layers: store lookups for delivery and progress, the per-turn context, and the steps
that run when a Discord bot binds (their order lives in ``bootstrap.BIND_STEPS``).

Everything here reads the bot owner's store through the runtime anchor (``RUNTIME.bots``), so it answers
for every bot and profile of the process; a guild without an active policy gets ``None``/``"default"``.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Any, Optional

from . import RUNTIME_API, config, hermes, prompts
from .adapt.status import COMFORT, SECURITY, Outcome, Result
from .adapt.strategies import run_intervention, wrap_attr
from .delivery import delete, progress, split, worklog
from .policy import lanes
from .prompts.assemble import SPILL_DEFAULT_MAX_CHARS, SPILL_MARGIN
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


def install_ui(bind: Bind) -> Any:
    """K6: ``/policy`` on the bound bot, on the bot binding's own store; a binding left on the same client
    (factory called again for it) is detached first, so listeners never double."""
    previous = ui_items.binding_for(bind.native)
    if previous is not None:
        previous.detach()
    return ui_commands.install(bind.native, bind.adapter, bot_id=bind.binding.bot_id,
                               owner_home=bind.binding.owner_home, store=bind.binding.store)


__all__ = ["Bind", "CONTEXT", "DELETE_CHAT", "context_key", "guild_config", "install_delete", "install_delete_chat",
           "install_delivery", "install_progress", "install_ui", "progress_grouping", "turn_context"]
