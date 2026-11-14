"""Messages the interface sends on its own: the audit channel and direct messages to the operators.

* **Audit channel** (``audit_channel``, operator-only setting): one line per change made in ``/policy``
  — who, which settings, never the values (guidance: length and version id).
* **Operators** (``settings.notify_operators``, default on, OPEN L17): direct messages for activation
  requests (in :mod:`.commands`), every change of a server's guidance (with a diff attached) and a worse
  protection status. Guidance messages are coalesced per server: at most one per
  :data:`GUIDANCE_WINDOW`; a change inside the window is not dropped but sent, as one diff since the last
  message, when the window ends. Status messages: at most one per intervention per day.

Everything here runs in the background after the interaction was answered and never raises into it.
"""

from __future__ import annotations

import asyncio
import difflib
import io
import json
import logging
import time
from pathlib import Path
from typing import Any, Optional

from .. import config, hermes
from ..adapt.status import write_json_atomic
from ..store.file import _file_lock  # the store's thread+process lock, reused
from ..store.ops import guidance_digest
from . import perms

logger = logging.getLogger("ctx_discord_policy")

GUIDANCE_WINDOW = 300.0
STATUS_WINDOW = 24 * 3600.0
DIFF_LIMIT = 200_000


class NotifyState:
    """Small JSON state next to the store (``notify-state.json``), changed only under its lock."""

    def __init__(self, path: Path) -> None:
        self.path = Path(path)
        self.lock_path = self.path.with_name(self.path.name + ".lock")

    def _read(self) -> dict:
        try:
            data = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return {}
        return data if isinstance(data, dict) else {}

    def change(self, fn: Any) -> Any:
        with _file_lock(self.lock_path):
            data = self._read()
            result = fn(data)
            write_json_atomic(self.path, data)
            return result

    def get(self, key: str) -> Any:
        return self._read().get(key)


def forget_guidance(home: Any, bot_id: Any, guild_id: Any) -> None:
    """A server purged on the host (CLI, settings page) leaves no held copy of its guidance in the operator
    message state of its owner home (``/policy`` does the same through :meth:`Notifier.forget_guild`)."""
    path = hermes.plugin_data_dir(Path(home)) / "notify-state.json"
    if path.exists():
        key = f"guidance:{bot_id}:{guild_id}"
        NotifyState(path).change(lambda data: data.pop(key, None))


def diff_text(old: str, new: str, *, name: str = "guidance") -> str:
    lines = difflib.unified_diff((old or "").splitlines(), (new or "").splitlines(),
                                 fromfile=f"{name} (before)", tofile=f"{name} (now)", lineterm="")
    text = "\n".join(lines)
    text = text + "\n" if text else "(no textual difference)\n"
    return text[:DIFF_LIMIT]


class Notifier:
    def __init__(self, binding: Any) -> None:
        self.binding = binding
        self.state = NotifyState(binding.sync_path.with_name("notify-state.json"))
        self._pending: dict = {}

    # ── plumbing ──

    def spawn(self, coro: Any) -> None:
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            coro.close()
            return
        task = loop.create_task(coro)
        self.binding._tasks.add(task)
        task.add_done_callback(self.binding._tasks.discard)

    def enabled(self) -> bool:
        return bool(config.setting(self.binding.owner_home, "notify_operators"))

    async def dm_operators(self, text: str, *, file: Optional[tuple] = None) -> int:
        """Send ``text`` (and ``file`` = (name, bytes)) to every operator; returns how many got it."""
        found, readable = perms.operator_list(self.binding.owner_home)
        if not readable:
            return 0
        delivered = 0
        for operator in sorted(found):
            try:
                await self.binding.send_dm(operator, text, file=file)
                delivered += 1
            except Exception as exc:  # closed DMs, unknown user, outage
                logger.warning("ctx_discord_policy: message to an operator failed: %s", exc)
        return delivered

    # ── audit channel ──

    def audit(self, guild: Optional[dict], text: str) -> None:
        channel_id = (guild or {}).get("audit_channel")
        if channel_id:
            self.spawn(self._audit(str(channel_id), text))

    async def _audit(self, channel_id: str, text: str) -> None:
        try:
            await self.binding.send_channel(channel_id, text)
        except Exception as exc:
            logger.warning("ctx_discord_policy: audit message to channel %s failed: %s", channel_id, exc)

    # ── guidance changes ──

    def guidance_changed(self, ctx: Any, old_text: str, new_text: str) -> None:
        if not self.enabled():
            return
        key = f"guidance:{self.binding.bot_id}:{ctx.guild_id}"
        now = time.time()

        author = str(ctx.actor.user_id)

        def claim(data: dict) -> tuple:
            entry = data.get(key) if isinstance(data.get(key), dict) else {}
            last = entry.get("at")
            if isinstance(last, (int, float)) and now - last < GUIDANCE_WINDOW:
                authors = list(entry.get("by") or [])
                if author not in authors:
                    authors = (authors + [author])[-5:]
                entry.update(pending=True, by=authors)
                data[key] = entry
                return False, entry, float(last)
            pending = entry.get("pending") and isinstance(entry.get("text"), str)
            base = entry["text"] if pending else old_text
            authors = list(entry.get("by") or []) + [author] if pending else [author]
            data[key] = {"at": now, "text": new_text, "pending": False, "by": []}
            return True, {"base": base, "by": list(dict.fromkeys(authors))}, None

        due, info, last = self.state.change(claim)
        guild_name = ctx.guild_name
        if due:
            self.spawn(self._send_guidance(ctx.guild_id, guild_name, info["by"], info["base"], new_text))
        else:
            self._arm_timer(key, ctx.guild_id, guild_name, max(0.0, GUIDANCE_WINDOW - (now - last)))

    def _arm_timer(self, key: str, guild_id: str, guild_name: str, delay: float) -> None:
        """A timer, not a sleeping task: nothing waits on it, and it outlives the interaction."""
        if key in self._pending:
            return
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            return                                      # no loop: the next startup sends the pending diff
        self._pending[key] = loop.call_later(delay, lambda: self.spawn(self._flush_later(key, guild_id, guild_name)))

    def forget_guild(self, guild_id: str) -> None:
        key = f"guidance:{self.binding.bot_id}:{guild_id}"
        handle = self._pending.pop(key, None)
        if handle is not None and callable(getattr(handle, "cancel", None)):
            handle.cancel()
        self.state.change(lambda data: data.pop(key, None))

    async def _send_guidance(self, guild_id: str, guild_name: str, authors: list, old: str, new: str) -> None:
        from .panels import clean

        who = ", ".join(f"<@{a}>" for a in authors if str(a).isdigit()) or "Someone"
        text = (f"{who} changed the server guidance in **{clean(guild_name, 80)}** (id {guild_id}): "
                f"{len(old)} → {len(new)} characters, version {guidance_digest(new)[:12]}. The diff is attached. "
                "Server guidance never grants tools; it is shown to the model as data.")
        await self.dm_operators(text, file=(f"guidance-{guild_id}.diff", diff_text(old, new).encode("utf-8")))

    async def _flush_later(self, key: str, guild_id: str, guild_name: str) -> None:
        self._pending.pop(key, None)                    # a change arriving while this sends arms a new timer
        try:
            await self.flush_pending(key, guild_id, guild_name)
        except Exception:
            logger.warning("ctx_discord_policy: sending a delayed guidance diff failed", exc_info=True)

    def cancel_timers(self) -> None:
        for handle in list(self._pending.values()):
            cancel = getattr(handle, "cancel", None)
            if callable(cancel):
                cancel()
        self._pending.clear()

    async def flush_pending(self, key: str, guild_id: str, guild_name: str = "this server") -> bool:
        """Send the coalesced diff of a server whose window has ended (also called after a restart)."""
        snapshot = self.binding.store.read()
        guild = snapshot.guild(self.binding.bot_id, guild_id) or {}
        current = (guild.get("prompts") or {}).get("guild") or ""
        now = time.time()

        def take(data: dict) -> Optional[tuple]:
            entry = data.get(key) if isinstance(data.get(key), dict) else {}
            if not entry.get("pending"):
                return None
            base = entry.get("text") if isinstance(entry.get("text"), str) else ""
            data[key] = {"at": now, "text": current, "pending": False, "by": []}
            return base, list(entry.get("by") or [])

        taken = self.state.change(take)
        if taken is None or not self.enabled():
            return False
        await self._send_guidance(guild_id, guild_name, taken[1], taken[0], current)
        return True

    async def flush_all_pending(self) -> None:
        """After a (re)start: send held-back diffs whose window ended, arm timers for the others."""
        prefix = f"guidance:{self.binding.bot_id}:"
        for key, entry in list(self.state._read().items()):
            if key.startswith(prefix) and isinstance(entry, dict) and entry.get("pending"):
                last = entry.get("at")
                left = GUIDANCE_WINDOW - (time.time() - last) if isinstance(last, (int, float)) else 0.0
                if left <= 0:
                    await self.flush_pending(key, key[len(prefix):])
                else:
                    self._arm_timer(key, key[len(prefix):], "this server", left)

    # ── server models ──

    async def model_unavailable(self, guild_id: str, lane: str, model: str, reason: str) -> int:
        """A server's chosen model can't be used: tell the operators, at most once per server, lane and day."""
        if not self.enabled():
            return 0
        key = f"model:{self.binding.bot_id}:{guild_id}:{lane}"
        now = time.time()

        def claim(data: dict) -> bool:
            last = data.get(key)
            if isinstance(last, (int, float)) and now - last < STATUS_WINDOW:
                return False
            data[key] = now
            return True

        if not self.state.change(claim):
            return 0
        from .panels import clean

        guild = self.binding.store.read().guild(self.binding.bot_id, str(guild_id)) or {}
        get_guild = getattr(self.binding.client, "get_guild", None)
        server = getattr(get_guild(int(guild_id)), "name", None) if callable(get_guild) and str(guild_id).isdigit() \
            else None
        closes = lane == "public" and (guild.get("models") or {}).get("on_unavailable") == "close"
        whose = "members' turns" if lane == "public" else "operator turns"
        meanwhile = ("Members' messages there are not answered until it works again (the server is set to close then)."
                     if closes else "Those turns use the profile's model until it works again.")
        text = (f"The model chosen for {whose} in **{clean(server or 'a server', 80)}** (id {guild_id}), "
                f"{clean(model, 200)}, can't be used: {clean(reason, 300)}. {meanwhile} "
                "Next step: /policy → Models in that server, or `hermes ctx-discord-policy status` on the host.")
        return await self.dm_operators(text)

    # ── protection status ──

    async def check_status(self) -> int:
        """Tell the operators once a day per intervention when a safety intervention is degraded."""
        if not self.enabled():
            return 0
        rows = self.binding.runtime.status.snapshot().get("interventions") or {}
        down = {name: r for name, r in rows.items()
                if r.get("klass") == "S" and r.get("outcome") in ("fallback", "off")}
        now = time.time()

        def claim(data: dict) -> list:
            due = []
            for name in sorted(down):
                key = f"status:{name}"
                last = data.get(key)
                if isinstance(last, (int, float)) and now - last < STATUS_WINDOW:
                    continue
                data[key] = now
                due.append(name)
            return due

        due = self.state.change(claim) if down else []
        if not due:
            return 0
        lines = "\n".join(f"• {name}: {down[name].get('outcome')} — {str(down[name].get('reason', ''))[:160]}"
                          for name in due)
        text = ("Protection of ctx_discord_policy is reduced on this bot (members' messages stay "
                f"closed where a check failed):\n{lines}\nRun `hermes ctx-discord-policy status` on the host for "
                "details.")
        return await self.dm_operators(text)
