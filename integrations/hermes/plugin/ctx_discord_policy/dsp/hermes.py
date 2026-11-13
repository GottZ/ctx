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


def isolation_mode() -> tuple[str, str]:
    """``(mode, evidence)`` for ``plugins.isolation`` of the active profile."""
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
    """
    with home_override(home):
        return ctx.get_config(key, default)
