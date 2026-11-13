"""A PluginContext double that records what a plugin registers."""

from __future__ import annotations

from typing import Any, Callable, Optional


class FakeCtx:
    def __init__(self, profile_name: str = "default", *, settings: Optional[dict] = None,
                 fail_hook_at: Optional[int] = None) -> None:
        self.profile_name = profile_name
        self.plugin_id = "ctx_discord_policy"
        self.hooks: list[tuple[str, Callable]] = []
        self.unload: list[Callable[[], None]] = []
        self.cli: list[dict[str, Any]] = []
        self.platform_handlers: list[tuple[str, Callable]] = []
        self.settings = settings if settings is not None else {}
        self._fail_hook_at = fail_hook_at

    def register_hook(self, name: str, callback: Callable, **_: Any) -> object:
        if self._fail_hook_at is not None and len(self.hooks) + 1 == self._fail_hook_at:
            raise RuntimeError("synthetic hook registration failure")
        self.hooks.append((name, callback))
        return object()

    def on_unload(self, callback: Callable[[], None]) -> object:
        self.unload.append(callback)
        return object()

    def register_cli_command(self, name: str, help: str, setup_fn: Callable, handler_fn=None,
                             description: str = "") -> object:
        self.cli.append({"name": name, "help": help, "setup_fn": setup_fn, "handler_fn": handler_fn})
        return object()

    def register_platform_handler(self, platform: str, factory: Callable) -> None:
        self.platform_handlers.append((platform, factory))

    def get_config(self, key: str, default: Any = None) -> Any:
        return self.settings.get(key, default)

    def run_unload(self) -> None:
        for callback in reversed(self.unload):
            callback()
