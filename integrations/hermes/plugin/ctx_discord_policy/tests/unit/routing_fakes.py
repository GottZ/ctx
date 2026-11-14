"""Stand-ins for the Hermes side of profile routing and models (S12/S13 unit and replay tests).

:class:`FakeHermes` replaces the functions in ``dsp.hermes`` that need Hermes with a small in-memory
model that follows Hermes' rules as read in ``gateway/profile_routing.py`` (most specific route first,
a route applies only to its bot's messages, invalid profile names are dropped by the parser) and in
``hermes_cli.config`` (``profile_routes`` in the default root's config.yaml). The real behaviour is
pinned by the contract tests (``tests/contract/test_routing_contract.py``, ``test_models_contract.py``).
"""

from __future__ import annotations

import contextlib
import re
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Optional

_PROFILE = re.compile(r"[a-z0-9][a-z0-9_-]{0,63}")


class FakeRoute:
    def __init__(self, name="", platform="discord", profile="", guild_id=None, chat_id=None, thread_id=None,
                 user_id=None, bot_profile=None, enabled=True):
        self.name, self.platform, self.profile = name, platform, profile
        self.guild_id, self.chat_id, self.thread_id, self.user_id = guild_id, chat_id, thread_id, user_id
        self.bot_profile, self.enabled = bot_profile, enabled

    @property
    def specificity(self) -> int:
        return 2 * bool(self.guild_id) + 4 * bool(self.chat_id) + 8 * bool(self.thread_id) + 16 * bool(self.user_id)

    def matches(self, platform, guild_id=None, chat_id=None, thread_id=None, parent_chat_id=None,
                adapter_profile=None, user_id=None) -> bool:
        if not self.enabled or self.platform != platform:
            return False
        if (self.bot_profile or None) != (adapter_profile or None):
            return False
        if self.user_id is not None and self.user_id != user_id:
            return False
        if self.thread_id and self.thread_id != thread_id:
            return False
        if self.chat_id and self.chat_id not in (chat_id, parent_chat_id):
            return False
        return not (self.guild_id and self.guild_id != guild_id)

    def __repr__(self) -> str:
        return f"FakeRoute({self.name!r} -> {self.profile!r})"


def parse(entries: list) -> list:
    out = []
    for entry in entries or []:
        if not isinstance(entry, dict) or not _PROFILE.fullmatch(str(entry.get("profile", ""))):
            continue
        bot = entry.get("bot_profile")
        out.append(FakeRoute(**{k: entry.get(k) for k in ("name", "platform", "profile", "guild_id", "chat_id",
                                                         "thread_id", "user_id")},
                             bot_profile=None if bot in (None, "", "default") else bot,
                             enabled=entry.get("enabled", True)))
    out.sort(key=lambda r: r.specificity, reverse=True)
    return out


def match(routes: list, source: Any, adapter_profile: Optional[str]) -> Any:
    for route in routes:
        if route.matches(str(getattr(source, "platform", "")), guild_id=getattr(source, "guild_id", None),
                         chat_id=getattr(source, "chat_id", None), thread_id=getattr(source, "thread_id", None),
                         parent_chat_id=getattr(source, "parent_chat_id", None), adapter_profile=adapter_profile,
                         user_id=getattr(source, "user_id", None)):
            return route
    return None


class FakeRunner:
    """The runner surface profile routing reads: config (multiplex, route table) and the home resolver."""

    def __init__(self, root: Path, routes: Optional[list] = None, *, multiplex: bool = True) -> None:
        self.root = Path(root)
        self.config = SimpleNamespace(multiplex_profiles=multiplex, profile_routes=parse(routes or []))
        self._profile_bot_platforms: dict = {}

    def _profile_name_for_source(self, source, adapter_profile=None):
        if not self.config.multiplex_profiles:
            return None
        hit = match(self.config.profile_routes, source, adapter_profile)
        return hit.profile if hit else None

    def _resolve_profile_home_for_source(self, source):
        name = getattr(source, "profile", None) or self._profile_name_for_source(source)
        return self.root if not name or name == "default" else self.root / "profiles" / name


class FakeAdapter:
    """Builds sources the way ``BasePlatformAdapter.build_source`` does: profile stamped from the table."""

    def __init__(self, runner: FakeRunner, owner_profile: Optional[str] = None) -> None:
        self.gateway_runner = runner
        self._owner_profile = owner_profile

    def build_source(self, chat_id, chat_type="group", user_id=None, thread_id=None, guild_id=None,
                     parent_chat_id=None, message_id=None, scope_id=None, **_):
        src = SimpleNamespace(platform="discord", chat_id=str(chat_id), chat_type=chat_type, user_id=user_id,
                              thread_id=thread_id, guild_id=guild_id, scope_id=scope_id or guild_id,
                              parent_chat_id=parent_chat_id, message_id=message_id, profile=None,
                              profile_route_rejected=False)
        src.profile = self.gateway_runner._profile_name_for_source(src, self._owner_profile) or self._owner_profile
        return src


class FakeHermes:
    """In-memory Hermes configuration, profiles and providers, patched into ``dsp.hermes``."""

    def __init__(self, root: Path) -> None:
        self.root = Path(root)
        self.config_entries: list = []
        self.managed: Optional[str] = None
        self.write_error: Optional[Exception] = None
        self.writes = 0
        self.profiles: dict = {}               # name -> info dict (display_name, model, provider)
        self.served: Optional[set] = None      # None: every profile in self.profiles plus default
        self.credentials: dict = {}            # profile -> {provider: state}
        self.contexts: dict = {}               # (provider, model) -> int
        self.catalog: dict = {}                # profile -> [ {slug, name, models, total} ]
        self.scope: Optional[str] = None       # profile whose scope is "active"

    def add_profile(self, name: str, *, display_name: str = "", model: str = "m", provider: str = "p") -> Path:
        home = self.root / "profiles" / name
        home.mkdir(parents=True, exist_ok=True)
        self.profiles[name] = {"name": name, "path": home, "display_name": display_name, "model": model,
                               "provider": provider, "previous_names": []}
        return home

    def name_of(self, home: Any) -> str:
        path = Path(home)
        return "default" if path == self.root else path.name

    # dsp.hermes replacements -------------------------------------------------------------------

    def default_root(self):
        return self.root

    def config_managed(self):
        return self.managed

    def read_route_entries(self, root):
        return [dict(e) for e in self.config_entries]

    def write_route_entries(self, root, transform):
        if self.managed:
            raise RuntimeError("managed")
        if self.write_error is not None:
            raise self.write_error
        before = [dict(e) for e in self.config_entries]
        self.config_entries = transform([dict(e) for e in before])
        self.writes += 1
        return before, self.config_entries

    def served_profiles(self, runner):
        names = set(self.profiles) | {"default"} if self.served is None else set(self.served) | {"default"}
        return {n: (self.root if n == "default" else self.root / "profiles" / n) for n in names}

    def list_profiles(self):
        return [{"name": "default", "path": self.root, "display_name": "", "model": "m-root", "provider": "p",
                 "previous_names": []}] + [dict(v) for _, v in sorted(self.profiles.items())]

    @contextlib.contextmanager
    def profile_scope(self, home):
        previous, self.scope = self.scope, self.name_of(home)
        try:
            yield
        finally:
            self.scope = previous

    def scope_home(self):
        return None if self.scope is None else str(self.root if self.scope == "default"
                                                    else self.root / "profiles" / self.scope)

    def credential_state(self, provider):
        state = self.credentials.get(self.scope or "default", {}).get(provider, "no credential")
        return state, "fake"

    def resolve_provider_runtime(self, provider, model):
        return {"provider": provider, "base_url": f"https://{provider}.invalid/v1", "api_key": "FAKE-SECRET-VALUE",
                "model": "runtime-model"}

    def context_length(self, provider, model, runtime):
        return self.contexts.get((provider, model), 128_000)

    def picker_providers(self, current_provider="", current_model=""):
        return [dict(r) for r in self.catalog.get(self.scope or "default", [])]

    def profile_model(self):
        info = self.profiles.get(self.scope or "", {})
        return info.get("model", "m-root"), info.get("provider", "p")

    def install(self, monkeypatch, hermes_module) -> "FakeHermes":
        for name in ("default_root", "config_managed", "read_route_entries", "write_route_entries", "served_profiles",
                     "list_profiles", "profile_scope", "scope_home", "credential_state", "resolve_provider_runtime",
                     "context_length", "picker_providers", "profile_model"):
            monkeypatch.setattr(hermes_module, name, getattr(self, name))
        monkeypatch.setattr(hermes_module, "provider_runtime_present", lambda: True)
        monkeypatch.setattr(hermes_module, "parse_routes", parse)
        monkeypatch.setattr(hermes_module, "match_route", match)
        monkeypatch.setattr(hermes_module, "profile_name_for_home",
                            lambda home: "default" if Path(home) == self.root else Path(home).name)
        return self


def arm(anchor, home: Path, profile: str) -> None:
    """Register a plugin instance in ``home`` (the profile is then armed)."""
    import os

    from fakes.ctx import FakeCtx

    previous = os.environ.get("HERMES_HOME")
    os.environ["HERMES_HOME"] = str(home)
    try:
        anchor.entry.register(FakeCtx(profile))
    finally:
        if previous is None:
            os.environ.pop("HERMES_HOME", None)
        else:
            os.environ["HERMES_HOME"] = previous
