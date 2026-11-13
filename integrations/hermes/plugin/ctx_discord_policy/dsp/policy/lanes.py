"""Public capability lane: which tools a non-operator turn may use.

S1 placeholder: the code catalog is empty, so every tool call of a public route is blocked. The catalog
with its argument validators arrives with the security core (S3).
"""

from __future__ import annotations

from typing import Any


def public_tool_allowed(route: Any, tool_name: str, args: Any) -> bool:
    return False


def capability_lane_text(route: Any) -> str:
    if route.is_operator:
        return "[Capability lane: operator]\nThis turn was written by the operator; tool authority applies."
    return ("[Capability lane: public]\nThis turn was written by a member of this Discord server who is not "
            "the operator. No tools are available for it.")
