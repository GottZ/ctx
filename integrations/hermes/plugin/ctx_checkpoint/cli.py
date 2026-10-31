"""``hermes ctx_checkpoint <status|sync|query|store|search|get>``.

Hermes discovers this module for the active memory provider at argparse setup time
(``plugins/memory.discover_plugin_cli_commands``: ``register_cli`` plus
``<provider>_command``) and imports it without executing the plugin's ``__init__``.
The commands themselves live in the engine (``memory/cli.py``).
"""

from __future__ import annotations

from .memory.cli import ctx_command, register_cli

ctx_checkpoint_command = ctx_command

__all__ = ["ctx_checkpoint_command", "register_cli"]
