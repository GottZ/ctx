"""The only place that imports Hermes modules.

Every Hermes interaction goes through these small functions so that (a) imports stay lazy (nothing here
runs at plugin import time), (b) unit tests can run without Hermes, and (c) a missing or renamed Hermes
symbol surfaces as one well-defined probe failure instead of an ``AttributeError`` deep in a hook.
"""

from __future__ import annotations

import contextlib
import os
from pathlib import Path
from typing import Any, Iterator, Optional


def _fallback_home() -> Path:
    raw = os.environ.get("HERMES_HOME", "").strip()
    return Path(raw).expanduser() if raw else Path.home() / ".hermes"


def current_home() -> Path:
    """The Hermes home active in this context (profile override, else ``HERMES_HOME``)."""
    try:
        from hermes_constants import get_hermes_home
    except ImportError:
        return _fallback_home()
    return Path(get_hermes_home())


def home_key(path: Optional[os.PathLike | str] = None) -> str:
    """Stable identity of a Hermes home, exactly as Hermes keys its per-home plugin managers."""
    try:
        from hermes_constants import hermes_home_key
    except ImportError:
        candidate = Path(path) if path is not None else current_home()
        return os.path.normcase(str(candidate.expanduser().resolve(strict=False)))
    return hermes_home_key(path)


@contextlib.contextmanager
def home_override(path: os.PathLike | str) -> Iterator[None]:
    """Run a block as if ``path`` were the active Hermes home (context-local, like Hermes' profile scope)."""
    try:
        from hermes_constants import reset_hermes_home_override, set_hermes_home_override
    except ImportError:
        previous = os.environ.get("HERMES_HOME")
        os.environ["HERMES_HOME"] = str(path)
        try:
            yield
        finally:
            if previous is None:
                os.environ.pop("HERMES_HOME", None)
            else:
                os.environ["HERMES_HOME"] = previous
        return
    token = set_hermes_home_override(str(path))
    try:
        yield
    finally:
        reset_hermes_home_override(token)


class ConfigUnreadable(RuntimeError):
    """Hermes could not read config.yaml and serves a fallback (last-known-good or defaults)."""


def isolation_mode() -> tuple[str, str]:
    """``(mode, evidence)`` for ``plugins.isolation`` of the active profile."""
    if os.environ.get("HERMES_PLUGIN_HOST_PROCESS") == "1":
        # Inside the isolated plugin host child Hermes reports in_process; the gateway is elsewhere.
        return "host", "HERMES_PLUGIN_HOST_PROCESS=1 (plugin host child process)"
    try:
        from hermes_cli.plugin_isolation import isolation_mode as _mode
    except ImportError:
        return "in_process", "hermes_cli.plugin_isolation absent (pre-isolation Hermes runs plugins in-process)"
    return str(_mode()), "hermes_cli.plugin_isolation.isolation_mode()"


def hermes_version() -> str:
    try:
        from hermes_cli.version_info import get_version_info

        return str(get_version_info().base_version)
    except Exception:  # version is informational only
        return "unknown"


def profile_name_for_home(path: os.PathLike | str) -> Optional[str]:
    try:
        from hermes_constants import profile_name_for_home as _name
    except ImportError:
        return None
    try:
        return _name(path)
    except Exception:
        return None


def plugin_data_dir(home: os.PathLike | str) -> Path:
    """``<home>/plugin-data/ctx_discord_policy`` (same root Hermes uses for ``PluginState``)."""
    return Path(home) / "plugin-data" / "ctx_discord_policy"


def read_plugin_setting(ctx: Any, home: os.PathLike | str, key: str, default: Any) -> Any:
    """``plugins.entries.<plugin>.settings.<key>`` from the config of ``home`` (not the caller's scope).

    Uses the official ``ctx.get_config`` under a context-local home override, so Hermes' own config
    loader (signature-cached, picks up edits without a restart, honours managed overlays) decides.
    Raises :class:`ConfigUnreadable` when Hermes serves a fallback because config.yaml is broken: an
    operator removed in that broken edit must not keep authority from the last-known-good copy.
    A home that does not exist yields ``default`` without creating anything.
    """
    if not Path(home).is_dir():
        return default
    with home_override(home):
        try:
            from hermes_cli.config import load_config_readonly
            from hermes_cli.config_read_errors import FailedConfigRead
        except ImportError:
            pass
        else:
            if isinstance(load_config_readonly(), FailedConfigRead):
                raise ConfigUnreadable(f"{Path(home) / 'config.yaml'} could not be read")
        return ctx.get_config(key, default)


def turn_runner_class() -> Any:
    """``gateway.run_turn_runner.TurnRunner`` (runs a turn's progress loop), ``None`` if unavailable."""
    try:
        from gateway.run_turn_runner import TurnRunner
    except ImportError:
        return None
    return TurnRunner


def spill_config() -> Any:
    """Hermes' resolved hook-output spill configuration (``None`` when unavailable: default limit)."""
    try:
        from tools.hook_output_spill import get_spill_config
    except ImportError:
        return None
    try:
        return get_spill_config()
    except Exception:
        return None


def hook_registered(home: os.PathLike | str, hook_name: str, callback: Any) -> Optional[bool]:
    """Whether ``callback`` is in the live hook registry of the plugin manager of ``home``.

    ``None`` when Hermes is not importable (unit tests). Behaviour-based arming check (A8): an instance
    whose load timed out gets ``None`` from ``register_hook`` and has no callback in the registry.
    """
    try:
        from hermes_cli import plugins
    except ImportError:
        return None
    with home_override(home):
        manager = plugins.get_plugin_manager()
    registry = getattr(manager, "_hooks", None)
    if not isinstance(registry, dict):
        return False
    return any(cb is callback for cb in registry.get(hook_name, ()))


# --- S12/S13 --- profile routing and models per server (PROFILE-PER-GUILD §3, §B3, §B4) -------------
#
# Every function below degrades to a defined answer when the Hermes symbol it needs is missing; the
# callers (dsp.routing, dsp.models) turn that into a probe failure with a reason, never a crash.


class HermesUnavailable(RuntimeError):
    """A Hermes function this feature needs is missing (message names it)."""


def default_root() -> Path:
    """The Hermes root (the default profile's home): owner of ``profile_routes`` (P2, ``profile_channels``)."""
    try:
        from hermes_constants import get_default_hermes_root
    except ImportError:
        return _fallback_home()
    return Path(get_default_hermes_root())


def profile_dir(name: str) -> Path:
    """Home of profile ``name`` (``default`` = the root); raises ValueError for an invalid name."""
    try:
        from hermes_cli.profiles import get_profile_dir
    except ImportError:
        root = default_root()
        return root if name == "default" else root / "profiles" / name
    return Path(get_profile_dir(name))


def valid_profile_name(name: Any) -> bool:
    try:
        from hermes_cli.profiles import validate_profile_name
    except ImportError:
        import re

        return isinstance(name, str) and bool(re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", name))
    try:
        validate_profile_name(name)
    except (ValueError, TypeError):
        return False
    return True


def config_managed() -> Optional[str]:
    """Name of the package manager that owns the Hermes configuration, or None (writable)."""
    try:
        from hermes_cli import config as hc
    except ImportError as exc:
        raise HermesUnavailable(f"hermes_cli.config: {exc}") from exc
    if not hc.is_managed():
        return None
    get_system = getattr(hc, "get_managed_system", None)
    return str(get_system() or "a package manager") if callable(get_system) else "a package manager"


@contextlib.contextmanager
def _config_lock() -> Iterator[None]:
    """Hermes' in-process configuration lock (re-entrant; ``save_config`` takes it again)."""
    try:
        from hermes_cli import config as hc
    except ImportError:
        yield
        return
    lock = getattr(hc, "_CONFIG_LOCK", None)
    with (lock if lock is not None else contextlib.nullcontext()):
        yield


def read_route_entries(root: os.PathLike | str) -> list[dict[str, Any]]:
    """``profile_routes`` of ``root``'s config.yaml exactly as written (raw entries, Hermes' reader)."""
    try:
        from hermes_cli.config import read_raw_config
    except ImportError as exc:
        raise HermesUnavailable(f"hermes_cli.config.read_raw_config: {exc}") from exc
    with home_override(root):
        raw = read_raw_config()
    entries = raw.get("profile_routes") if isinstance(raw, dict) else None
    return [dict(e) for e in entries if isinstance(e, dict)] if isinstance(entries, list) else []


def write_route_entries(root: os.PathLike | str, transform: Any) -> tuple[list[dict], list[dict]]:
    """Replace ``profile_routes`` of ``root``'s config.yaml with ``transform(current_entries)``.

    Read and write happen under Hermes' configuration lock with ``root`` bound explicitly (under a
    profile override ``save_config`` would write the profile's file, P2 W4); comments and every other key
    survive (``save_config(merge_existing=True)``, P2 W1). Returns ``(before, after)``. Raises on a
    managed or unreadable configuration (nothing is written then)."""
    try:
        from hermes_cli.config import get_config_path, read_raw_config, save_config
    except ImportError as exc:
        raise HermesUnavailable(f"hermes_cli.config: {exc}") from exc
    if config_managed():
        raise HermesUnavailable("the Hermes configuration is managed by a package manager")
    with home_override(root), _config_lock():
        path = Path(get_config_path())
        raw = read_raw_config()
        if not isinstance(raw, dict) or (not raw and path.is_file() and path.stat().st_size > 0):
            raise HermesUnavailable(f"{path} could not be read; nothing was written")
        current = raw.get("profile_routes")
        before = [dict(e) for e in current if isinstance(e, dict)] if isinstance(current, list) else []
        after = transform([dict(e) for e in before])
        raw["profile_routes"] = after
        save_config(raw, merge_existing=True)
    return before, after


def parse_routes(entries: list[dict[str, Any]]) -> list[Any]:
    """Hermes' own parser (validation, normalisation, most-specific-first order)."""
    try:
        from gateway.profile_routing import parse_profile_routes
    except ImportError as exc:
        raise HermesUnavailable(f"gateway.profile_routing.parse_profile_routes: {exc}") from exc
    return parse_profile_routes(entries)


def route_to_entry(route: Any) -> dict[str, Any]:
    """A parsed ``ProfileRoute`` back to its config entry (only the fields it sets)."""
    import dataclasses

    if dataclasses.is_dataclass(route):
        values = {f.name: getattr(route, f.name) for f in dataclasses.fields(route)}
    else:
        values = {k: getattr(route, k, None) for k in ("name", "platform", "profile", "guild_id", "chat_id",
                                                       "thread_id", "user_id", "bot_profile", "enabled")}
    entry = {k: v for k, v in values.items() if v is not None}
    if entry.get("enabled", True) is True:
        entry.pop("enabled", None)
    return entry


def match_route(routes: list[Any], source: Any, adapter_profile: Optional[str]) -> Any:
    """The route Hermes' matcher picks for ``source`` received by the bot of ``adapter_profile``."""
    try:
        from gateway.profile_routing import match_profile_route
    except ImportError as exc:
        raise HermesUnavailable(f"gateway.profile_routing.match_profile_route: {exc}") from exc
    platform = getattr(getattr(source, "platform", None), "value", getattr(source, "platform", None))
    return match_profile_route(
        routes, platform=str(platform or ""), guild_id=getattr(source, "guild_id", None),
        chat_id=getattr(source, "chat_id", None), thread_id=getattr(source, "thread_id", None),
        parent_chat_id=getattr(source, "parent_chat_id", None), adapter_profile=adapter_profile,
        user_id=getattr(source, "user_id", None))


def served_profiles(runner: Any) -> Optional[dict[str, Path]]:
    """Profiles this gateway serves (name -> home), as its own routing check sees them."""
    try:
        from gateway.run import _multiplex_profile_homes
    except ImportError:
        return None
    try:
        return {str(name): Path(home) for name, home in _multiplex_profile_homes(getattr(runner, "config", None))}
    except Exception:
        return None


def list_profiles() -> list[dict[str, Any]]:
    """Installed profiles for a picker: name, label, model, provider (no secrets; P3)."""
    try:
        from hermes_cli import profiles as hp
    except ImportError as exc:
        raise HermesUnavailable(f"hermes_cli.profiles: {exc}") from exc
    out = []
    for info in hp.list_profiles(lazy_skill_count=True):
        display = str(getattr(info, "display_name", "") or "")
        out.append({"name": str(info.name), "path": Path(info.path), "display_name": display,
                    "model": getattr(info, "model", None), "provider": getattr(info, "provider", None),
                    "previous_names": list(getattr(info, "previous_names", None) or [])})
    return out


def profile_parked(home: os.PathLike | str) -> bool:
    try:
        from hermes_cli.profiles import profile_is_parked
    except ImportError:
        return (Path(home) / "gateway.parked").exists()
    return bool(profile_is_parked(Path(home)))


def profile_has_own_bot(runner: Any, name: str, platform: str = "discord") -> bool:
    """Whether profile ``name`` runs a bot of its own on ``platform`` (a credential boundary in Hermes)."""
    if name == "default":  # the primary profile's bots are the runner's own adapters
        primary = getattr(runner, "adapters", None) or {}
        if any(str(getattr(p, "value", p)).lower() == platform for p in primary):
            return True
    owned = (getattr(runner, "_profile_bot_platforms", None) or {}).get(name) or ()
    if any(str(getattr(p, "value", p)).lower() == platform for p in owned):
        return True
    adapters = (getattr(runner, "_profile_adapters", None) or {}).get(name) or {}
    return any(str(getattr(p, "value", p)).lower() == platform for p in adapters)


def synthetic_source(adapter: Any, guild_id: str, *, chat_id: str = "0", user_id: str = "0") -> Any:
    """A guild message source as the bot's adapter builds it (profile stamped from the live table)."""
    build = getattr(adapter, "build_source", None)
    if callable(build):
        return build(chat_id=chat_id, chat_type="group", user_id=user_id, guild_id=str(guild_id),
                     scope_id=str(guild_id))
    try:
        from gateway.config import Platform
        from gateway.session import SessionSource
    except ImportError as exc:
        raise HermesUnavailable(f"gateway.session.SessionSource: {exc}") from exc
    return SessionSource(platform=Platform.DISCORD, chat_id=chat_id, chat_type="group", user_id=user_id,
                         guild_id=str(guild_id), scope_id=str(guild_id))


# Models ------------------------------------------------------------------------------------------


@contextlib.contextmanager
def profile_scope(home: os.PathLike | str) -> Iterator[None]:
    """Hermes' per-turn profile scope: config, credentials (secret scope) and terminal policy of ``home``."""
    try:
        from gateway.run import _profile_runtime_scope
    except ImportError as exc:
        raise HermesUnavailable(f"gateway.run._profile_runtime_scope: {exc}") from exc
    with _profile_runtime_scope(Path(home)):
        yield


def scope_home() -> Optional[str]:
    """Home of the secret scope active in this context (None: no scope; credentials from the process)."""
    try:
        from agent.secret_scope import current_secret_scope_home
    except ImportError:
        return None
    return current_secret_scope_home()


def secret_names() -> frozenset[str]:
    """NAMES of the credentials visible in this context (the profile's secret scope, else the process
    environment). Values are never returned."""
    try:
        from agent.secret_scope import current_secret_scope
        scope = current_secret_scope()
    except ImportError:
        scope = None
    return frozenset(scope.keys()) if scope is not None else frozenset(os.environ.keys())


def provider_runtime_present() -> bool:
    """Whether Hermes' provider runtime resolution (used for channel overrides) is there."""
    try:
        from gateway.run import _resolve_runtime_agent_kwargs_for_provider
    except ImportError:
        return False
    return callable(_resolve_runtime_agent_kwargs_for_provider)


def resolve_provider_runtime(provider: str, model: str) -> dict[str, Any]:
    """Runtime kwargs for ``provider`` in this context, exactly as Hermes resolves a channel override."""
    try:
        from gateway.run import _resolve_runtime_agent_kwargs_for_provider
    except ImportError as exc:
        raise HermesUnavailable(f"gateway.run._resolve_runtime_agent_kwargs_for_provider: {exc}") from exc
    return _resolve_runtime_agent_kwargs_for_provider(provider, target_model=model or None)


def credential_state(provider: str) -> tuple[str, str]:
    """``("available" | "no credential" | "unknown", reason)`` for ``provider`` in this context.

    Decided from credential NAMES and stored-credential presence only: the provider's key variables in
    the active secret scope, entries in the profile's credential pool, an OAuth login, or (custom
    endpoints) the key variable the endpoint declares. No secret value is read for the decision."""
    provider = (provider or "").strip().lower()
    names = secret_names()
    try:
        from hermes_cli import auth
    except ImportError:
        return "unknown", "Hermes' provider registry is not available"
    registry = getattr(auth, "PROVIDER_REGISTRY", {}) or {}
    try:
        pconfig = registry.get(provider)
    except Exception:
        pconfig = None
    try:
        pool = auth.read_credential_pool(provider)
        if pool:
            return "available", "stored credential"
    except Exception:
        pass
    if pconfig is not None:
        env_vars = tuple(getattr(pconfig, "api_key_env_vars", ()) or ())
        if any(v in names for v in env_vars):
            return "available", "key in the profile"
        if getattr(pconfig, "auth_type", "api_key") != "api_key":
            try:
                if auth.get_auth_status(provider).get("logged_in") is True:
                    return "available", "signed in"
            except Exception:
                return "unknown", "sign-in state could not be read"
        return "no credential", ("none of " + ", ".join(env_vars)) if env_vars else "not signed in"
    return _custom_endpoint_state(provider, names)


def _custom_endpoint_state(provider: str, names: frozenset[str]) -> tuple[str, str]:
    try:
        from hermes_cli.config import load_config
        cfg = load_config()
    except Exception:
        return "unknown", "the profile's configuration could not be read"
    name = provider.split(":", 1)[1] if provider.startswith("custom:") else provider
    entry = None
    providers = cfg.get("providers") if isinstance(cfg, dict) else None
    if isinstance(providers, dict) and isinstance(providers.get(name), dict):
        entry = providers[name]
    for item in (cfg.get("custom_providers") or []) if isinstance(cfg, dict) else []:
        if isinstance(item, dict) and str(item.get("name", "")).strip().lower() == name:
            entry = item
    if entry is None:
        return "no credential", "provider not configured in this profile"
    key_env = entry.get("key_env") or entry.get("api_key_env")
    if key_env:
        return ("available", "key in the profile") if str(key_env) in names else ("no credential", str(key_env))
    return "available", "endpoint without a key" if "api_key" not in entry else "key in the configuration"


def minimum_context_length() -> int:
    try:
        from agent.model_metadata import MINIMUM_CONTEXT_LENGTH
    except ImportError:
        return 64_000
    return int(MINIMUM_CONTEXT_LENGTH)


def context_length(provider: str, model: str, runtime: dict[str, Any]) -> Optional[int]:
    """Context window Hermes would use for ``model`` in this context (may fetch provider metadata)."""
    try:
        from agent.model_metadata import get_model_context_length
    except ImportError:
        return None
    # custom_providers=None: Hermes resolves the profile's endpoint list itself (its own normalised form).
    value = get_model_context_length(model, base_url=str(runtime.get("base_url") or ""),
                                     api_key=str(runtime.get("api_key") or ""), provider=provider)
    return int(value) if isinstance(value, int) and value > 0 else None


def picker_providers(current_provider: str = "", current_model: str = "") -> list[dict[str, Any]]:
    """Hermes' own /model picker rows for this context: slug, name, model ids (no key fields; B4)."""
    try:
        from hermes_cli.config import load_config
        from hermes_cli.model_switch_providers import list_picker_providers
    except ImportError as exc:
        raise HermesUnavailable(f"hermes_cli.model_switch_providers.list_picker_providers: {exc}") from exc
    cfg = load_config()
    user = cfg.get("providers") if isinstance(cfg, dict) else None
    custom = cfg.get("custom_providers") if isinstance(cfg, dict) else None
    rows = list_picker_providers(current_provider=current_provider, current_model=current_model,
                                 user_providers=user if isinstance(user, dict) else None,
                                 custom_providers=custom if isinstance(custom, list) else None, max_models=500,
                                 non_blocking_catalogs=True, probe_custom_providers=False,
                                 probe_current_custom_provider=False)
    out = []
    for row in rows or []:
        if not isinstance(row, dict) or not row.get("slug"):
            continue
        models = [str(m.get("id") if isinstance(m, dict) else m) for m in row.get("models") or []]
        out.append({"slug": str(row["slug"]), "name": str(row.get("name") or row["slug"]),
                    "models": [m for m in models if m], "total": int(row.get("total_models") or len(models))})
    return out


def profile_model() -> tuple[Optional[str], Optional[str]]:
    """``(model, provider)`` the active profile is configured with (for display)."""
    try:
        from hermes_cli.config import load_config
        cfg = load_config()
    except Exception:
        return None, None
    model = cfg.get("model") if isinstance(cfg, dict) else None
    if isinstance(model, dict):
        return (model.get("default") or model.get("model")), model.get("provider")
    return (model if isinstance(model, str) else None), None


def normalize_model(model: str, provider: str) -> str:
    """The model id as Hermes canonicalises it for ``provider`` (what ``agent.model`` will read)."""
    try:
        from hermes_cli.model_normalize import _AGGREGATOR_PROVIDERS, normalize_model_for_provider
    except ImportError:
        return model
    try:
        if provider and provider not in _AGGREGATOR_PROVIDERS:
            return normalize_model_for_provider(model, provider)
    except Exception:
        pass
    return model


def launch_policy_applies(runner: Any, source: Any) -> bool:
    """A frozen launch policy owns this session's model (Hermes resolves it on its own route)."""
    try:
        from gateway.session_policy import policy_for_source
    except ImportError:
        return False
    try:
        return source is not None and policy_for_source(runner, source) is not None
    except Exception:
        return True  # cannot tell: leave the session to Hermes


# --- end S12/S13 -------------------------------------------------------------------------------------


# --- SP --- settings page: Hermes access of the dashboard backend (SETTINGS-PAGE §2, §4.4) ----------------
#
# The backend (``dashboard/plugin_api.py`` -> ``dsp/web``) runs in whatever process mounts the plugin API: the
# gateway, a standalone ``hermes dashboard``/``hermes serve``, or a plugin-host child. Every Hermes call it makes
# is here; a missing symbol answers ``None`` (the caller decides open vs. closed), never an AttributeError.

PLUGIN_ID = "ctx_discord_policy"


def admitted(request: Any) -> Optional[bool]:
    """Hermes' own admission of sensitive routes (``_require_token``: native grant, hosted-plugin flag, gated
    session, loopback session token). ``None`` when this Hermes has no such check (the caller fails closed)."""
    try:
        from hermes_cli.plugin_host_http_auth import admitted_for_sensitive_route
    except ImportError:
        return None
    return bool(admitted_for_sensitive_route(request))


def origin_allowed(request: Any) -> Optional[bool]:
    """Hermes' own origin check for state-changing requests (v0.23.0+). ``None`` when absent (v0.22.4)."""
    try:
        from hermes_cli.dashboard_auth.request_utils import cookie_origin_is_allowed
    except ImportError:
        return None
    return bool(cookie_origin_is_allowed(request))


def public_url(app_state: Any = None) -> str:
    """The operator-declared dashboard public URL, or ``""``."""
    try:
        from hermes_cli.dashboard_auth.prefix import resolve_public_url
    except ImportError:
        return ""
    try:
        return str(resolve_public_url(app_state) or "")
    except TypeError:  # v0.22.4: resolve_public_url() takes no state
        try:
            return str(resolve_public_url() or "")
        except Exception:
            return ""
    except Exception:
        return ""


def serving_process(app_state: Any) -> tuple[str, str]:
    """``(process, surface)`` of the app serving this request: process ``gateway`` (the gateway's own listener,
    the Discord client lives here), ``plugin_host`` (an isolated plugin-host child) or ``dashboard``."""
    if os.environ.get("HERMES_PLUGIN_HOST_PROCESS") == "1":
        return "plugin_host", "plugin_host"
    runner = getattr(app_state, "gateway_runner", None)
    try:
        from hermes_cli.web_server_surface import surface as _surface
        surface = str(_surface(app_state))
    except Exception:  # v0.22.4 has no surface table
        surface = "gateway" if runner is not None else "dashboard"
    return ("gateway" if runner is not None else "dashboard"), surface


def plugin_settings() -> dict:
    """``plugins.entries.ctx_discord_policy.settings`` of the active profile scope, read like
    ``ctx.get_config`` does. Raises :class:`ConfigUnreadable` while Hermes serves a fallback copy."""
    from hermes_cli.config import load_config_readonly
    from hermes_cli.plugins_state import _plugin_settings_entry

    config = load_config_readonly()
    try:
        from hermes_cli.config_read_errors import FailedConfigRead
    except ImportError:
        FailedConfigRead = ()  # type: ignore[assignment]
    if FailedConfigRead and isinstance(config, FailedConfigRead):
        raise ConfigUnreadable("config.yaml could not be read")
    entry = _plugin_settings_entry(config or {}, PLUGIN_ID) or {}
    settings = entry.get("settings")
    return dict(settings) if isinstance(settings, dict) else {}


def save_plugin_settings(plugin_dir: os.PathLike | str, values: dict) -> list:
    """Write global settings through the writer Hermes' own settings form uses (schema-checked, file-locked,
    refuses managed installs with ``PermissionError`` and unknown keys with ``ValueError``)."""
    from hermes_cli.plugins_settings import save_plugin_settings as _save

    return list(_save(PLUGIN_ID, Path(plugin_dir), values))


def settings_changed(written: list) -> None:
    """Tell desktop windows of this process that the plugin's settings changed (as ``plugins.manage`` does)."""
    try:
        from hermes_cli.plugin_events import broadcast_plugin_event
    except ImportError:
        return
    try:
        broadcast_plugin_event(PLUGIN_ID, "settings.changed", {"written": list(written)})
    except Exception:  # informational only
        pass


def spill_config() -> Optional[dict]:
    """Hermes' resolved hook-output spill configuration (the context budget, DESIGN A9), or ``None``."""
    try:
        from tools.hook_output_spill import get_spill_config
    except ImportError:
        return None
    try:
        config = get_spill_config()
    except Exception:
        return None
    return config if isinstance(config, dict) else None


def profiles() -> Optional[list]:
    """``[{name, path, display_name, model, provider, is_default}]`` of every Hermes profile, or ``None``."""
    try:
        from hermes_cli.profiles import list_profiles
    except ImportError:
        return None
    try:
        rows = list_profiles(lazy_skill_count=True)
    except Exception:
        return None
    out = []
    for info in rows:
        out.append({"name": str(getattr(info, "name", "")), "path": str(getattr(info, "path", "")),
                    "display_name": str(getattr(info, "display_name", "") or ""),
                    "model": getattr(info, "model", None), "provider": getattr(info, "provider", None),
                    "is_default": bool(getattr(info, "is_default", False))})
    return out


def skills_of(home: os.PathLike | str) -> Optional[list]:
    """Enabled skills installed in the profile ``home`` (``[{name, description, category}]``), or ``None``."""
    try:
        from tools.skills_tool import find_all_skills
    except ImportError:
        return None
    try:
        with home_override(home):
            rows = find_all_skills()        # skip_disabled=True would include the disabled ones (config UI)
    except Exception:
        return None
    out = []
    for row in rows or []:
        if isinstance(row, dict) and row.get("name"):
            out.append({"name": str(row["name"]), "description": str(row.get("description") or "")[:300],
                        "category": str(row.get("category") or "")})
    return out


def installed_skills(home: os.PathLike | str) -> frozenset:
    """Names of the enabled skills installed in the profile ``home`` — what a turn there can load. One list for
    the settings page and the Discord panels. Raises when Hermes cannot tell; callers treat that as unknown."""
    rows = skills_of(home)
    if rows is None:
        raise RuntimeError("Hermes could not list the skills of this profile")
    return frozenset(row["name"] for row in rows)


def model_options(profile: Optional[str]) -> Optional[dict]:
    """Hermes' model picker payload for ``profile`` (the same as ``GET /api/model/options``), or ``None``."""
    try:
        from hermes_cli.inventory import build_model_options_payload, load_picker_context
        from hermes_cli.web_server_profiles import _config_profile_scope
    except ImportError:
        return None
    try:
        with _config_profile_scope(profile):
            payload = build_model_options_payload(load_picker_context(), explicit_only=False,
                                                  include_unconfigured=False, refresh=False)
    except Exception:
        return None
    return payload if isinstance(payload, dict) else None


def rpc_available() -> bool:
    """Whether this Hermes serves plugin RPC methods (``ctx.register_rpc``, Hermes ≥ 04846730f3)."""
    import importlib.util

    try:
        return importlib.util.find_spec("hermes_cli.plugin_rpc") is not None
    except (ImportError, ValueError):
        return False


def gateway_rpc(home: os.PathLike | str, method: str, params: dict, timeout: float) -> Any:
    """Call ``method`` on the running gateway that owns ``home`` (never starts one). Raises on any failure."""
    import asyncio

    from hermes_cli.gateway_client import connect_gateway
    from hermes_cli.gateway_runtime import discover_gateway_endpoint

    discovery = discover_gateway_endpoint(Path(home), timeout=min(timeout, 1.5))
    endpoint = getattr(discovery, "endpoint", None)
    if endpoint is None:
        raise RuntimeError(f"gateway not reachable ({getattr(discovery, 'state', 'unknown')})")

    async def run() -> Any:
        async with connect_gateway(endpoint=endpoint) as client:
            return await client.rpc(method, _timeout=timeout, **params)

    return asyncio.run(asyncio.wait_for(run(), timeout))


def discord_bot_token() -> str:
    """The Discord bot token of the active profile scope (server side only; never returned to a client)."""
    try:
        from agent.secret_scope import get_secret
    except ImportError:
        return ""
    try:
        return str(get_secret("DISCORD_BOT_TOKEN", "") or "").strip()
    except Exception:
        return ""
