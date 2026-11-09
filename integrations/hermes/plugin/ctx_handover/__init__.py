"""ctx_handover — Hermes plugin entry point (layer L12; INTERFACES §4.6, DESIGN §14, R12).

``register()`` only registers: the ``/handover`` slash command, the ``hermes handover`` CLI command, the hooks
``pre_llm_call``/``pre_tool_call``/``post_llm_call``/``post_compaction`` and the agent tool ``handover``. It opens
no file, socket, thread or subprocess — Hermes runs it in every CLI process, in ``hermes serve`` and inside
``hermes plugins validate``. The engine starts lazily on the first real use (a command, a hook, the tool).
"""

from __future__ import annotations

DESCRIPTION = "Move this session and its working files to another Hermes gateway you paired (or a bundle file)"
ARGS_HINT = "to|take|export|import|status|peers|pair|reclaim|cancel|resume|setup|doctor|identity|gc|logs …"
CLI_HELP = "Move a Hermes session and its working files to another gateway you paired"


def register(ctx) -> None:
    """Register every surface of the plugin that this host offers (feature detection by attribute). No I/O (R12)."""
    from .engine import agent_tool, commands

    plugin = commands.plugin_for(ctx)
    if callable(getattr(ctx, "register_command", None)):
        ctx.register_command(
            "handover", plugin.slash, description=DESCRIPTION, args_hint=ARGS_HINT, argument_mode="text"
        )
    if callable(getattr(ctx, "register_cli_command", None)):  # absent under plugins.isolation: host
        ctx.register_cli_command("handover", CLI_HELP, commands.cli_setup, plugin.cli, description=CLI_HELP)
    if callable(getattr(ctx, "register_hook", None)):
        for hook_name, callback in plugin.hooks.callbacks().items():
            ctx.register_hook(hook_name, callback)
    if callable(getattr(ctx, "register_tool", None)):
        ctx.register_tool(
            "handover",
            "ctx_handover",
            agent_tool.TOOL_SCHEMA,
            agent_tool.handler(ctx, provider=lambda: plugin),
            description=agent_tool.TOOL_SCHEMA["description"],
        )
