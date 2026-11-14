"""Which Hermes profiles the operator may choose for a server (PROFILE-PER-GUILD §4.2/§4.3).

A profile is offered only when every condition holds: Hermes multiplexes profiles, it serves the profile,
ctx_discord_policy is armed there (registered and live in its hook registry), the profile runs no
Discord bot of its own, and it is not listed in ``settings.locked_profiles`` of the default root. The
bot's own profile is never a route target; it is the option "use the bot's own profile" and only the
lock list applies to it. Every refusal carries the next step.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any, Optional

from .. import config, hermes
from ..policy import hooks


@dataclass(frozen=True)
class Candidate:
    name: str
    label: str = ""
    model: Optional[str] = None
    provider: Optional[str] = None
    eligible: bool = False
    reason: str = ""             # why not (empty when eligible)
    next_step: str = ""


@dataclass(frozen=True)
class Host:
    """Facts about the running Hermes that every check uses (read once per decision)."""

    multiplex: bool
    served: Optional[dict]        # name -> home; None: unknown
    locked: Optional[frozenset]   # None: unreadable (everything counts as locked)
    owner_profile: str            # profile that owns the bot ("default" for the default profile's bot)


def owner_profile_name(owner_home: Any) -> str:
    return hermes.profile_name_for_home(owner_home) or "default"


def host(runner: Any, owner_home: Any) -> Host:
    cfg = getattr(runner, "config", None)
    return Host(multiplex=bool(getattr(cfg, "multiplex_profiles", False)), served=hermes.served_profiles(runner),
                locked=config.locked_profiles(hermes.default_root()), owner_profile=owner_profile_name(owner_home))


def unavailable(runner: Any, facts: Host) -> Optional[str]:
    """Why profile selection is unavailable on this Hermes at all (None: available)."""
    if runner is None:
        return ("the bot's gateway is not reachable from here. Next step: change it from /policy in Discord "
                "or on the host while Hermes runs")
    if not facts.multiplex:
        return ("this Hermes serves a single profile. Next step: `hermes config set multiplex_profiles true`, "
                "then restart Hermes")
    if facts.served is None:
        return "Hermes did not report which profiles it serves (Hermes changed). Next step: check `hermes ctx-discord-policy status`"
    return None


def is_locked(name: str, facts: Host) -> bool:
    return facts.locked is None or name in facts.locked


def home_key_of(name: str) -> Optional[str]:
    try:
        return hermes.home_key(hermes.profile_dir(name))
    except ValueError:
        return None


def evaluate(name: str, runner: Any, facts: Host, *, info: Optional[dict] = None) -> Candidate:
    label = name
    display = str((info or {}).get("display_name") or "").strip()
    if display and display != name:
        label = f"{name} — {display}"
    model, provider = (info or {}).get("model"), (info or {}).get("provider")

    def no(reason: str, step: str) -> Candidate:
        return Candidate(name, label, model, provider, False, reason, step)

    if not hermes.valid_profile_name(name):
        return no("not a valid profile name", "choose another profile")
    if is_locked(name, facts):
        if facts.locked is None:
            return no("the list of locked profiles (settings.locked_profiles) can't be read",
                      "fix the Hermes configuration of the default profile")
        return no("locked in the Hermes configuration (settings.locked_profiles)",
                  "remove it from settings.locked_profiles on the host if members may use it")
    if name == facts.owner_profile:
        return no("this is the bot's own profile", "use **Use the bot's own profile**")
    if facts.served is None or name not in facts.served:
        home = (info or {}).get("path")
        if home is not None and hermes.profile_parked(Path(home)):
            return no("not served right now (parked)", f"`hermes -p {name} gateway start`")
        return no("not served by the running Hermes (created after it started, or set to run standalone)",
                  "restart Hermes (or wait for its profile rescan)")
    if hermes.profile_has_own_bot(runner, name):
        return no("runs its own Discord bot", "invite that bot to this server and use its /policy")
    key = home_key_of(name)
    if key is None or not hooks.armed_live(key):
        return no("ctx_discord_policy is not enabled there",
                  f"`hermes -p {name} plugins enable ctx_discord_policy`, then restart Hermes")
    return Candidate(name, label, model, provider, True)


def candidates(runner: Any, owner_home: Any) -> tuple[list[Candidate], Optional[str]]:
    """Every installed profile with its verdict (slow: lists profile directories; call off the event
    loop). Returns ``(candidates, unavailable_reason)``."""
    facts = host(runner, owner_home)
    why = unavailable(runner, facts)
    if why is not None:
        return [], why
    out = [evaluate(info["name"], runner, facts, info=info) for info in hermes.list_profiles()]
    out = [c for c in out if c.name != facts.owner_profile]
    out.sort(key=lambda c: (not c.eligible, c.name))
    return out, None


def check(name: Optional[str], runner: Any, owner_home: Any) -> Candidate:
    """Fresh verdict for one choice (``None`` = the bot's own profile: only the lock list applies)."""
    facts = host(runner, owner_home)
    if name is None:
        own = facts.owner_profile
        if is_locked(own, facts):
            return Candidate(own, own, eligible=False, reason="the bot's own profile is locked in the Hermes "
                             "configuration (settings.locked_profiles)",
                             next_step="choose another profile; members' messages stay closed until you do")
        return Candidate(own, own, eligible=True)
    why = unavailable(runner, facts)
    if why is not None:
        return Candidate(name, name, eligible=False, reason="profile selection is not available", next_step=why)
    info = next((i for i in _safe_profiles() if i["name"] == name), None)
    return evaluate(name, runner, facts, info=info)


def _safe_profiles() -> list[dict]:
    try:
        return hermes.list_profiles()
    except Exception:
        return []
