"""Providers and models a profile offers, for the model panel (PROFILE-PER-GUILD §B4).

Source: Hermes' own ``/model`` picker path, run in the scope of the server's profile, so only providers
that profile has configured appear. Each provider additionally gets the plugin's own availability
verdict (credential names), because Hermes' list also shows configured endpoints without checking keys.
Blocking (reads catalogs and the profile's configuration): call it off the event loop.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Optional

from .. import hermes


@dataclass(frozen=True)
class Provider:
    slug: str
    name: str
    models: tuple = ()
    total: int = 0
    state: str = "unknown"          # available | no credential | unknown
    reason: str = ""


@dataclass
class Catalog:
    profile_model: Optional[str] = None
    profile_provider: Optional[str] = None
    providers: list = field(default_factory=list)
    error: str = ""

    def provider(self, slug: str) -> Optional[Provider]:
        return next((p for p in self.providers if p.slug == slug), None)


def load(home: Any) -> Catalog:
    """The catalog of profile ``home``; errors become ``Catalog.error`` (never raised)."""
    out = Catalog()
    try:
        with hermes.profile_scope(Path(home)):
            out.profile_model, out.profile_provider = hermes.profile_model()
            rows = hermes.picker_providers(out.profile_provider or "", out.profile_model or "")
            for row in rows:
                state, reason = hermes.credential_state(row["slug"])
                out.providers.append(Provider(row["slug"], row["name"], tuple(row["models"]), row["total"],
                                              state, reason))
    except Exception as exc:
        out.error = f"{type(exc).__name__}: {exc}"[:200]
    out.providers.sort(key=lambda p: (p.state != "available", p.slug))
    return out
