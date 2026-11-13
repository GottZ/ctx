"""Hermes plugin ``ctx_discord_policy``: per-server Discord policy, configured in Discord.

Hermes imports this file once per profile that enables the plugin, each time as a separate module object.
It stays thin on purpose: the policy runtime (``dsp/``) is loaded exactly once per process under the fixed
name ``ctx_discord_policy`` and shared by every profile instance, so routes, the armed register
and the status survive the per-profile module split (DESIGN §3.1).

An instance that finds an anchor of a different version or runtime API in the process (plugin files
replaced without a gateway restart) registers nothing and reports that a restart is required.
"""

from __future__ import annotations

import importlib
import importlib.util
import logging
import sys
from pathlib import Path
from typing import Any, Optional

PLUGIN_VERSION = "3.0.0"
RUNTIME_API = 1
ANCHOR_NAME = "ctx_discord_policy"

logger = logging.getLogger("ctx_discord_policy")


def _evict(name: str) -> None:
    for key in [k for k in sys.modules if k == name or k.startswith(name + ".")]:
        sys.modules.pop(key, None)


def load_anchor(plugin_dir: Path) -> Any:
    """Return the process-wide runtime package, loading it from ``plugin_dir/dsp`` on first use."""
    existing = sys.modules.get(ANCHOR_NAME)
    if existing is not None:
        return existing
    package_dir = plugin_dir / "dsp"
    spec = importlib.util.spec_from_file_location(
        ANCHOR_NAME, package_dir / "__init__.py", submodule_search_locations=[str(package_dir)])
    if spec is None or spec.loader is None:
        raise ImportError(f"cannot load {package_dir}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[ANCHOR_NAME] = module
    try:
        spec.loader.exec_module(module)
        # Import the whole runtime now, from these files: a lazy import after an in-place update would
        # otherwise mix code of two versions inside one anchor.
        importlib.import_module(f"{ANCHOR_NAME}.bootstrap")
    except BaseException:
        _evict(ANCHOR_NAME)
        raise
    return module


def anchor_conflict(anchor: Any) -> Optional[str]:
    version = getattr(anchor, "__version__", None)
    api = getattr(anchor, "RUNTIME_API", None)
    if api != RUNTIME_API or version != PLUGIN_VERSION:
        return (f"process already runs ctx_discord_policy {version} (runtime API {api}); this instance is "
                f"{PLUGIN_VERSION} (runtime API {RUNTIME_API}). Restart the gateway to switch versions.")
    return None


def register(ctx: Any) -> None:
    plugin_dir = Path(__file__).resolve().parent
    anchor = load_anchor(plugin_dir)
    conflict = anchor_conflict(anchor)
    if conflict is not None:
        logger.error("ctx_discord_policy: restart required, registering nothing: %s", conflict)
        try:
            sys.modules[f"{ANCHOR_NAME}.runtime"].note_refused(
                {"plugin_dir": str(plugin_dir), "version": PLUGIN_VERSION, "runtime_api": RUNTIME_API,
                 "reason": "restart required"})
        except Exception:  # an older or newer anchor may not offer the hook; the log line stands
            pass
        return
    sys.modules[f"{ANCHOR_NAME}.bootstrap"].register(ctx, plugin_dir=plugin_dir)
