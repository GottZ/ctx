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
