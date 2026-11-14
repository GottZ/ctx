"""Set, take over and re-apply a server's Hermes profile (PROFILE-PER-GUILD §3.2–§3.4, §4.7; R1).

One flow for every surface (Discord panel, CLI, Hermes settings page):

1. fresh eligibility check of the chosen profile (:mod:`.eligibility`);
2. a hand-written guild route of the same server and bot blocks a second entry (Hermes would raise
   ``admission_conflict``): the operator takes it over instead (rename only);
3. under the policy store's lock, after its revision check: the plugin entry is written into the
   default root's config.yaml through Hermes' configuration API (strategy 1 "saved + live"); with a
   managed configuration nothing is written (strategy 2 "live only", a YAML snippet is offered);
4. the running table is replaced as an attribute with a list from Hermes' own parser, then verified by
   resolving a synthetic message of the server; failures leave "saved, pending restart" (A4-P keeps the
   server closed for members until the running Hermes routes it where the operator chose);
5. the store records the choice (a changed profile resets the server's models).
"""

from __future__ import annotations

import logging
import threading
from dataclasses import dataclass, field
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Optional

from .. import hermes
from ..adapt.status import SECURITY, Outcome, Result
from ..runtime import RUNTIME
from ..store import ops
from ..store.file import PolicyStore, Snapshot
from . import check, eligibility, table

logger = logging.getLogger("ctx_discord_policy")

INTERVENTION = "R1"

# One running route table per gateway, shared by every bot (each bot has its own store and store lock):
# every read-transform-swap of it happens under this lock, or a concurrent change of another bot is lost.
_LIVE_LOCK = threading.RLock()


class Refused(Exception):
    """The choice is not possible; ``str(exc)`` is the user-facing reason with the next step."""

    def __init__(self, message: str, *, core_entry: Optional[dict] = None) -> None:
        super().__init__(message)
        self.core_entry = core_entry


@dataclass
class Applied:
    profile: Optional[str]
    saved: bool = False                # written to the Hermes configuration
    live: bool = False                 # the running table routes the server there (verified)
    strategy: str = ""                 # "saved+live" | "live only" | "saved" | "store only"
    reason: str = ""                   # why not saved or not live
    snippet: str = ""                  # YAML for a managed configuration
    managed: str = ""                  # who manages Hermes' configuration (nothing was written then)
    warning: str = ""
    snapshot: Optional[Snapshot] = None


@dataclass
class View:
    """What the panel shows about one server (cheap: running table, cached config reads)."""

    unavailable: Optional[str] = None
    desired: Optional[str] = None
    owner_profile: str = "default"
    live_profile: Optional[str] = None
    live_entry: Optional[str] = None
    contradicts: Optional[str] = None  # the running plugin entry routes to this profile, the policy says another
    state: str = "own"                 # own | live | pending | conflict | core
    core_entry: Optional[dict] = None
    exceptions: list = field(default_factory=list)
    managed: Optional[str] = None
    locked: bool = False
    armed: bool = False
    set_by: Optional[str] = None
    set_at: Optional[str] = None


def runner_of(adapter: Any) -> Any:
    return getattr(adapter, "gateway_runner", None)


# ── the running table ────────────────────────────────────────────────────────────────────────────────


def probe_live(runner: Any) -> tuple[bool, str]:
    """Sonde R1 (no I/O): the running table has the expected form and Hermes' parser/matcher work."""
    cfg = getattr(runner, "config", None)
    if runner is None or cfg is None:
        return False, "no gateway runner in this process"
    if not getattr(cfg, "multiplex_profiles", False):
        return False, "multiplexing disabled"
    routes = getattr(cfg, "profile_routes", None)
    if not isinstance(routes, list):
        return False, f"route table not recognised ({type(routes).__name__})"
    for route in routes:
        if not all(hasattr(route, a) for a in ("name", "platform", "profile", "guild_id", "chat_id", "thread_id",
                                               "user_id", "bot_profile", "enabled", "specificity", "matches")):
            return False, "route table not recognised (entries changed shape)"
    if not callable(getattr(runner, "_resolve_profile_home_for_source", None)):
        return False, "GatewayRunner._resolve_profile_home_for_source missing"
    try:
        sample = {"name": "dsp-probe", "platform": "discord", "guild_id": "1", "profile": "default"}
        parsed = hermes.parse_routes([sample])
        source = SimpleNamespace(platform="discord", guild_id="1", chat_id="2", thread_id=None, parent_chat_id=None,
                                 user_id="3")
        if len(parsed) != 1 or parsed[0].specificity != table.GUILD_SPECIFICITY \
                or hermes.match_route(parsed, source, None) is not parsed[0] or hermes.route_to_entry(parsed[0]) != sample:
            return False, "route table not recognised (parser or matcher behave differently)"
    except Exception as exc:
        return False, f"route table not recognised ({type(exc).__name__}: {exc})"
    return True, "parser, matcher and running table as expected"


def _live_entries(runner: Any) -> list[dict]:
    return [hermes.route_to_entry(r) for r in runner.config.profile_routes]


def replace_live(runner: Any, transform: Any) -> None:
    """Replace the running table with Hermes' parse of ``transform(entries)``: an attribute swap (readers
    iterating the old list finish on it), never an in-place change."""
    with _LIVE_LOCK:
        runner.config.profile_routes = hermes.parse_routes(transform(_live_entries(runner)))


def verify(runner: Any, adapter: Any, guild_id: str, profile: Optional[str], owner_home: Any) -> tuple[bool, str]:
    """A synthetic message of the server resolves to the chosen profile's home (the bot's own for None)."""
    try:
        source = hermes.synthetic_source(adapter, guild_id)
        if getattr(source, "profile_route_rejected", False):
            return False, "Hermes rejects the route (the profile is not served)"
        home = runner._resolve_profile_home_for_source(source)
        expected = hermes.profile_dir(profile) if profile else Path(owner_home)
    except Exception as exc:
        return False, f"verification failed ({type(exc).__name__})"
    if hermes.home_key(home) != hermes.home_key(expected):
        return False, "the running Hermes resolves this server to another profile"
    return True, ""


# ── views ────────────────────────────────────────────────────────────────────────────────────────────


def _config_entries(root: Path) -> list[dict]:
    try:
        return hermes.read_route_entries(root)
    except Exception:
        return []


def describe(store: PolicyStore, bot_id: str, guild_id: str, *, owner_home: Any, adapter: Any) -> View:
    runner = runner_of(adapter)
    owner = eligibility.owner_profile_name(owner_home)
    guild = store.read().guild(bot_id, guild_id) or {}
    routing = guild.get("routing") or {}
    view = View(desired=routing.get("profile"), owner_profile=owner, set_by=routing.get("set_by"),
                set_at=routing.get("set_at"))
    facts = eligibility.host(runner, owner_home)
    view.unavailable = eligibility.unavailable(runner, facts)
    try:
        view.managed = hermes.config_managed()
    except Exception:
        view.managed = None
    from ..policy import hooks  # late: hooks imports the runtime only

    live = list(getattr(getattr(runner, "config", None), "profile_routes", None) or [])
    stored = _config_entries(hermes.default_root())
    own = table.own_entries(live, bot_id, guild_id)
    core_live = table.core_guild_entries(live, guild_id, owner)
    core = core_live or table.core_guild_entries(stored, guild_id, owner)
    if core:
        first = core[0]
        view.core_entry = {"name": getattr(first, "name", None) if not isinstance(first, dict) else first.get("name"),
                           "profile": getattr(first, "profile", None) if not isinstance(first, dict)
                           else first.get("profile")}
    view.exceptions = [(getattr(e, "name", ""), getattr(e, "profile", ""),
                        bool(eligibility.home_key_of(str(getattr(e, "profile", "")))
                             and hooks.armed_live(eligibility.home_key_of(str(getattr(e, "profile", ""))))))
                       for e in table.core_exceptions(live, guild_id, owner)]
    if own:
        view.live_profile, view.live_entry = getattr(own[0], "profile", None), getattr(own[0], "name", None)
        if view.live_profile != view.desired:
            view.contradicts = view.live_profile
    elif core and not view.desired:
        view.live_profile, view.live_entry = view.core_entry["profile"], view.core_entry["name"]
    if own and core_live:
        view.state = "conflict"        # two guild routes of the same specificity: Hermes refuses every message
    elif view.desired is None:
        view.state = "core" if core else ("pending" if own else "own")
    elif core and not own:
        view.state = "conflict"
    elif own and view.live_profile == view.desired:
        view.state = "live"
    else:
        view.state = "pending"
    effective = view.desired or (view.core_entry or {}).get("profile") or owner
    view.locked = eligibility.is_locked(str(effective), facts)
    key = eligibility.home_key_of(str(effective))
    view.armed = bool(key) and hooks.armed_live(key)
    return view


def effective_profile(view: View) -> str:
    """The profile this server's turns run in (chosen, else a hand-written guild route, else the bot's own)."""
    return view.desired or (view.core_entry or {}).get("profile") or view.owner_profile


# ── operations ───────────────────────────────────────────────────────────────────────────────────────


def _core_conflict(entries: list, live: list, guild_id: str, owner: str) -> Optional[dict]:
    for source in (entries, [hermes.route_to_entry(r) for r in live]):
        found = table.core_guild_entries(source, guild_id, owner)
        if found:
            return dict(found[0])
    return None


def _effects(*, bot_id: str, guild_id: str, owner_home: Any, adapter: Any, outcome: Applied,
             transform_for: Any) -> Any:
    """Configuration write + live replacement, run by the store operation under its lock."""
    runner = runner_of(adapter)
    owner = eligibility.owner_profile_name(owner_home)

    def effects(guild_before: dict, profile: Optional[str]) -> None:
        transform = transform_for(profile, owner)
        switched = (guild_before.get("routing") or {}).get("profile") != profile
        root = hermes.default_root()
        managed = hermes.config_managed()
        if managed:
            outcome.strategy, outcome.saved, outcome.managed = "live only", False, str(managed)
            entry = table.make_entry(bot_id, guild_id, profile, owner) if profile else None
            outcome.snippet = table.snippet_yaml(entry) if entry else ""
            outcome.reason = f"Hermes' configuration is managed ({managed})"
        else:
            hermes.write_route_entries(root, transform)  # raises: nothing written, store unchanged
            outcome.saved = True
        ok, why = probe_live(runner)
        if ok:
            try:
                replace_live(runner, transform)
                ok, why = verify(runner, adapter, guild_id, profile, owner_home)
            except Exception as exc:
                ok, why = False, f"the running table could not be replaced ({type(exc).__name__})"
        outcome.live = ok
        if not ok:
            outcome.reason = "; ".join(r for r in (outcome.reason, why) if r)
            outcome.strategy = outcome.strategy or "saved"
            if not outcome.saved:
                outcome.strategy = "store only"
        elif not outcome.strategy:
            outcome.strategy = "saved+live"
        check.mark_touched(bot_id, guild_id)
        if switched:
            check.mark_switched(bot_id, guild_id)

    return effects


def set_profile(store: PolicyStore, bot_id: str, guild_id: str, profile: Optional[str], *, owner_home: Any, by: str,
                via: Optional[str] = None, expected_rev: Optional[int] = None, adapter: Any = None) -> Applied:
    """Choose the Hermes profile of a server (``None``: the bot's own profile, no plugin entry)."""
    runner = runner_of(adapter)
    owner = eligibility.owner_profile_name(owner_home)
    outcome = Applied(profile)
    if profile is not None:
        verdict = eligibility.check(profile, runner, owner_home) if runner is not None else _offline_check(profile, owner)
        if not verdict.eligible:
            raise Refused(f"{profile}: {verdict.reason}. Next step: {verdict.next_step}.")
    else:
        own = eligibility.check(None, runner, owner_home)
        if not own.eligible:
            outcome.warning = f"{own.reason}; {own.next_step}"
    live = list(getattr(getattr(runner, "config", None), "profile_routes", None) or [])
    conflict = _core_conflict(_config_entries(hermes.default_root()), live, guild_id, owner)
    if conflict is not None:
        raise Refused(f"This server is routed by Hermes' configuration (entry \"{conflict.get('name')}\") to "
                      f"{conflict.get('profile')}. Next step: take it over to manage it here — nothing changes for the "
                      "server.", core_entry=conflict)

    def transform_for(chosen: Optional[str], owner_name: str) -> Any:
        return lambda entries: table.with_profile(entries, bot_id, guild_id, chosen, owner_name)

    effects = _effects(bot_id=bot_id, guild_id=guild_id, owner_home=owner_home, adapter=adapter, outcome=outcome,
                       transform_for=transform_for)
    outcome.snapshot = ops.set_routing(store, bot_id, guild_id, profile, by=by, via=via, expected_rev=expected_rev,
                                       effects=effects)
    refresh_status(runner, owner_home, store, bot_id)
    return outcome


def adopt(store: PolicyStore, bot_id: str, guild_id: str, core_name: str, *, owner_home: Any, by: str,
          via: Optional[str] = None, expected_rev: Optional[int] = None, adapter: Any = None) -> Applied:
    """Take over the hand-written guild route ``core_name`` of this server: renamed to the plugin entry,
    profile and discriminators unchanged, so the parsed table differs only in the name."""
    runner = runner_of(adapter)
    owner = eligibility.owner_profile_name(owner_home)
    root = hermes.default_root()
    entries = _config_entries(root)
    if hermes.config_managed() is None:
        _, profile = table.adopted(entries, core_name, bot_id, guild_id, owner)
    else:
        live_entries = [hermes.route_to_entry(r) for r in getattr(getattr(runner, "config", None), "profile_routes", [])]
        _, profile = table.adopted(live_entries, core_name, bot_id, guild_id, owner)
    outcome = Applied(profile)

    def transform_for(_chosen: Optional[str], owner_name: str) -> Any:
        def transform(current: list[dict]) -> list[dict]:
            if any(table.is_own_entry(e, bot_id, guild_id) for e in current):
                return current  # already taken over (live table after the configuration write)
            return table.adopted(current, core_name, bot_id, guild_id, owner_name)[0]
        return transform

    effects = _effects(bot_id=bot_id, guild_id=guild_id, owner_home=owner_home, adapter=adapter, outcome=outcome,
                       transform_for=transform_for)
    outcome.snapshot = ops.adopt_routing(store, bot_id, guild_id, profile, by=by, via=via,
                                         expected_rev=expected_rev, effects=effects)
    refresh_status(runner, owner_home, store, bot_id)
    return outcome


def reapply(store: PolicyStore, bot_id: str, guild_id: str, *, owner_home: Any, adapter: Any, by: str = "policy",
            via: Optional[str] = None) -> Applied:
    """Apply the stored choice again (after a restart, a hand edit or a managed configuration). Takes no
    profile value: only what the store holds is applied."""
    guild = store.read().guild(bot_id, guild_id) or {}
    profile = (guild.get("routing") or {}).get("profile")
    return set_profile(store, bot_id, guild_id, profile, owner_home=owner_home, by=by, via=via, adapter=adapter)


def _offline_check(profile: str, owner: str) -> eligibility.Candidate:
    """Checks possible without the gateway (settings page in another process); the gateway re-checks the
    target per message (A4 armed, A4-P locked)."""
    if not hermes.valid_profile_name(profile):
        return eligibility.Candidate(profile, reason="not a valid profile name", next_step="choose another profile")
    from .. import config

    locked = config.locked_profiles(hermes.default_root())
    if locked is None or profile in locked:
        return eligibility.Candidate(profile, reason="locked in the Hermes configuration (settings.locked_profiles)",
                                     next_step="choose another profile")
    if profile == owner:
        return eligibility.Candidate(profile, reason="this is the bot's own profile",
                                     next_step="choose the bot's own profile instead")
    try:
        exists = hermes.profile_dir(profile).is_dir()
    except ValueError:
        exists = False
    if not exists:
        return eligibility.Candidate(profile, reason="no such profile", next_step="create it first")
    return eligibility.Candidate(profile, eligible=True)


# ── bind and status ──────────────────────────────────────────────────────────────────────────────────


def on_bind(adapter: Any, binding: Any) -> None:
    """When a bot binds (S3 platform-handler factory): re-apply chosen profiles into the running table if
    the configuration is managed (strategy 2), and report agreement of configuration and policy."""
    runner = runner_of(adapter)
    try:
        snapshot = binding.store.read()
        guilds = ((snapshot.data.get("bots") or {}).get(binding.bot_id) or {}).get("guilds") or {}
        owner = eligibility.owner_profile_name(binding.owner_home)
        if probe_live(runner)[0] and hermes.config_managed():
            chosen = {g: (c.get("routing") or {}).get("profile") for g, c in guilds.items()}

            def transform(entries: list[dict]) -> list[dict]:
                for gid, profile in chosen.items():
                    if profile is None:
                        continue
                    core = table.core_guild_entries(entries, gid, owner)
                    if not core:
                        entries = table.with_profile(entries, binding.bot_id, gid, profile, owner)
                    elif len(core) == 1 and core[0].get("profile") == profile:
                        # Taken over while the configuration was managed: Hermes kept the hand-written name.
                        entries = table.adopted(entries, str(core[0].get("name")), binding.bot_id, gid, owner)[0]
                    # Otherwise a hand-written guild route leads elsewhere: a second one of the same
                    # specificity would make Hermes refuse every message of the server. Leave it; A4-P keeps
                    # members closed and the panel shows the conflict.
                return entries

            replace_live(runner, transform)
            for gid, profile in chosen.items():
                if profile is not None:
                    check.mark_touched(binding.bot_id, gid)
    except Exception:
        logger.exception("ctx_discord_policy: R1 re-apply at bind failed; chosen profiles stay closed")
    refresh_status(runner, binding.owner_home, binding.store, binding.bot_id)


def refresh_status(runner: Any, owner_home: Any, store: PolicyStore, bot_id: str) -> Result:
    """Record R1 for this bot: saved+live / live only / pending restart / off."""
    try:
        snapshot = store.read()
        guilds = ((snapshot.data.get("bots") or {}).get(str(bot_id)) or {}).get("guilds") or {}
        chosen = {g: (c.get("routing") or {}).get("profile") for g, c in guilds.items()}
        routed = sum(1 for p in chosen.values() if p)
        ok, why = probe_live(runner)
        if not ok:
            result = Result(INTERVENTION, SECURITY, Outcome.OFF if routed else Outcome.OK,
                            f"off: {why}" + (f"; {routed} servers with a chosen profile admit members only while "
                                             "the running table routes them there" if routed else
                                             " (no server uses profile selection)"), "R1 probe")
            return RUNTIME.status.record(result)
        live = runner.config.profile_routes
        pending = [g for g, p in chosen.items()
                   if p != next((r.profile for r in live if table.is_own_entry(r, bot_id, g)), None)]
        owner = eligibility.owner_profile_name(owner_home)
        core = sum(1 for r in live if not table.is_plugin_entry(r) and table.bot_profile_key(r.bot_profile)
                   == table.bot_profile_key(owner) and str(r.platform) == "discord")
        managed = hermes.config_managed()
        head = "live only (managed configuration)" if managed else "saved+live"
        if pending:
            result = Result(INTERVENTION, SECURITY, Outcome.FALLBACK,
                            f"pending restart ({len(pending)}): members' messages there stay closed", "R1 probe")
        else:
            result = Result(INTERVENTION, SECURITY, Outcome.OK,
                            f"{head} · {routed} servers routed · {core} core routes", why)
    except Exception as exc:
        result = Result(INTERVENTION, SECURITY, Outcome.OFF, f"off: {type(exc).__name__}: {exc}", "R1 probe")
    return RUNTIME.status.record(result)
