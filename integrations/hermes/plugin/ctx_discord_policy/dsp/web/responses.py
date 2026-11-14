"""Response shape of the settings page backend: JSON bodies, one error form (SETTINGS-PAGE §3.4)."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Optional


@dataclass
class WebResponse:
    status: int
    body: Any
    headers: dict = field(default_factory=dict)


def ok(body: Any, status: int = 200) -> WebResponse:
    return WebResponse(status, body)


def error(status: int, code: str, message: str, *, field_name: Optional[str] = None, **extra: Any) -> WebResponse:
    """``{"error": {"code", "message", "field"?, ...}}``; ``message`` is English text for the page."""
    detail: dict[str, Any] = {"code": code, "message": message}
    if field_name:
        detail["field"] = field_name
    findings = extra.pop("findings", None)
    if findings is not None:
        detail["findings"] = findings
    return WebResponse(status, {"error": detail, **extra})
