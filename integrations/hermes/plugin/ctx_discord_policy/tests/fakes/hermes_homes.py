"""Real Hermes plugin managers over temporary homes (contract tests only)."""

from __future__ import annotations

import re
import shutil
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
ANCHOR = "ctx_discord_policy"
PLUGIN = "ctx_discord_policy"


def _evict(prefix: str) -> None:
    for key in [k for k in sys.modules if k == prefix or k.startswith(prefix + ".")]:
        sys.modules.pop(key, None)


def install_plugin(target: Path, *, version: str | None = None) -> Path:
    target.mkdir(parents=True, exist_ok=True)
    shutil.copy2(REPO / "__init__.py", target / "__init__.py")
    shutil.copy2(REPO / "plugin.yaml", target / "plugin.yaml")
    shutil.copytree(REPO / "dsp", target / "dsp", ignore=shutil.ignore_patterns("__pycache__"))
    if version is not None:
        entry = target / "__init__.py"
        entry.write_text(re.sub(r'PLUGIN_VERSION = "[^"]+"', f'PLUGIN_VERSION = "{version}"',
                                entry.read_text(encoding="utf-8")), encoding="utf-8")
    return target


class Homes:
    """A launch home with ``profiles/<name>`` homes, the plugin installed once and linked per profile."""

    def __init__(self, base: Path) -> None:
        self.root = base / "root"
        (self.root / "plugins").mkdir(parents=True)
        self.plugin_dir = install_plugin(self.root / "plugins" / PLUGIN)
        self.enable(self.root)

    def profile(self, name: str, *, enabled: bool = True, plugin_dir: Path | None = None) -> Path:
        home = self.root / "profiles" / name
        (home / "plugins").mkdir(parents=True)
        (home / "plugins" / PLUGIN).symlink_to(plugin_dir or self.plugin_dir, target_is_directory=True)
        if enabled:
            self.enable(home)
        else:
            (home / "config.yaml").write_text("plugins:\n  enabled: []\n", encoding="utf-8")
        return home

    @staticmethod
    def enable(home: Path) -> None:
        (home / "config.yaml").write_text(f"plugins:\n  enabled: [{PLUGIN}]\n", encoding="utf-8")


def load_in(home: Path):
    from hermes_cli import plugins
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    token = set_hermes_home_override(str(home))
    try:
        manager = plugins.get_plugin_manager()
        manager.discover_and_load(force=True)
        return manager
    finally:
        reset_hermes_home_override(token)


def dispatch_tool(home: Path, tool: str = "terminal", **ids):
    from hermes_cli import plugins
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    token = set_hermes_home_override(str(home))
    try:
        return plugins._dispatch_pre_tool_call_hooks(tool, {"command": "id"}, **ids)[0]
    finally:
        reset_hermes_home_override(token)
