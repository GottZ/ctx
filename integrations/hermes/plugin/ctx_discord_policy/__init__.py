"""Hermes plugin ``ctx_discord_policy``: per-server Discord policy, configured in Discord.

This file is the plugin entry point Hermes imports (once per profile that enables the plugin). It stays
thin: the policy runtime lives in the ``dsp`` package next to it.
"""

from __future__ import annotations

import logging
from typing import Any

PLUGIN_VERSION = "3.0.0"
RUNTIME_API = 1

logger = logging.getLogger(__name__)


def register(ctx: Any) -> None:
    """Scaffold only: the policy runtime is not wired yet, so the plugin registers nothing."""
    logger.info("ctx_discord_policy %s: scaffold build, no policy active", PLUGIN_VERSION)
