"""CI sets DSP_REQUIRE_DISCORD=1: then a missing discord.py is an error, never a silent skip."""

import os

if os.environ.get("DSP_REQUIRE_DISCORD") == "1":
    import discord  # noqa: F401
