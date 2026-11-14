"""Prompt engine: neutral templates, whitelisted placeholders, per-turn context assembly (DESIGN §7).

Pure module: no Hermes, runtime, store or policy imports. Inputs are plain data (global settings,
guild state, lane, mode, budget, turn facts), outputs are text plus metadata.
"""

from .assemble import (
    LANES,
    MODES,
    Assembled,
    GuidanceCheck,
    assemble,
    budget_from_spill,
    check_guidance,
    reaction_wakeup_text,
)
from .render import PLACEHOLDERS, TEMPLATES, Rendered, placeholder_values, render, sanitize_value

__all__ = [
    "Assembled", "GuidanceCheck", "LANES", "MODES", "PLACEHOLDERS", "Rendered", "TEMPLATES", "assemble",
    "budget_from_spill", "check_guidance", "placeholder_values", "reaction_wakeup_text", "render",
    "sanitize_value",
]
