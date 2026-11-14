"""Which model a turn of a server should run, and whether the chosen profile can run it.

Availability is decided inside the profile's own scope from credential NAMES (and stored-credential
presence), never from secret values, plus Hermes resolving a runtime for the provider without an error
(a provider without a key resolves to a runtime without a key instead of failing, so "no error" alone is
not availability). Context windows below Hermes' minimum (64 000 tokens) are refused when saving.
"""

from __future__ import annotations

import concurrent.futures
import logging
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Optional

from .. import hermes
from ..adapt.status import Outcome
from ..runtime import RUNTIME

logger = logging.getLogger("ctx_discord_policy")

CACHE_TTL = 60.0
CONTEXT_TIMEOUT = 8.0
PUBLIC = "public"
OPERATOR = "operator"

_cache: dict = {}


@dataclass(frozen=True)
class Verdict:
    ok: bool
    reason: str = ""              # why not, or a warning when ok
    next_step: str = ""
    context: Optional[int] = None


def effective(guild: Optional[dict], lane: str) -> Optional[dict]:
    """The model a lane runs in this server, or ``None`` (Hermes decides).

    Members: a manager's pick only while the operator still releases it, else the operator's choice.
    Operator turns: only the operator's choice for operator turns."""
    models = (guild or {}).get("models") or {}
    if lane == OPERATOR:
        return models.get("operator")
    pick = models.get("manager_pick")
    if pick is not None and pick in (models.get("public_choices") or []):
        return pick
    return models.get("public")


def resolve_in_scope(provider: str, model: str) -> tuple[bool, str, Optional[dict]]:
    """In the CURRENT scope: ``(available, reason, runtime)``."""
    state, why = hermes.credential_state(provider)
    if state != "available":
        return False, f"no credential for {provider} in this profile ({why})", None
    try:
        runtime = hermes.resolve_provider_runtime(provider, model)
    except Exception as exc:
        return False, f"Hermes cannot use provider {provider}: {type(exc).__name__}", None
    runtime = dict(runtime)
    runtime.pop("model", None)
    return True, "", runtime


def availability(home: Any, provider: str, model: str, *, cached: bool = True) -> tuple[bool, str]:
    """Whether profile ``home`` can run ``provider``/``model`` now (cached for a minute)."""
    key = (hermes.home_key(home), provider, model)
    now = time.monotonic()
    if cached and key in _cache and now - _cache[key][0] < CACHE_TTL:
        return _cache[key][1], _cache[key][2]
    try:
        with hermes.profile_scope(Path(home)):
            ok, why, _ = resolve_in_scope(provider, model)
    except Exception as exc:
        ok, why = False, f"the profile's scope could not be opened ({type(exc).__name__})"
    _cache[key] = (now, ok, why)
    return ok, why


def context_window(home: Any, provider: str, model: str, timeout: float = CONTEXT_TIMEOUT) -> Optional[int]:
    """Hermes' context window for the model in profile ``home`` (None: unknown or timed out)."""

    def run() -> Optional[int]:
        with hermes.profile_scope(Path(home)):
            ok, _, runtime = resolve_in_scope(provider, model)
            return hermes.context_length(provider, model, runtime or {}) if ok else None

    pool = concurrent.futures.ThreadPoolExecutor(max_workers=1, thread_name_prefix="dsp-context")
    try:
        return pool.submit(run).result(timeout=timeout)
    except Exception:
        logger.info("ctx_discord_policy: context window of %s/%s unknown", provider, model, exc_info=True)
        return None
    finally:
        pool.shutdown(wait=False)


def validate_choice(home: Any, provider: str, model: str, *, profile: str) -> Verdict:
    """Save-time check of one model for a server in profile ``home`` (blocking; call off the loop)."""
    ok, why = availability(home, provider, model, cached=False)
    if not ok:
        return Verdict(False, why, f"add the credential to profile {profile} (`hermes -p {profile} auth add "
                                   f"{provider}`), then try again — or choose another model")
    minimum = hermes.minimum_context_length()
    window = context_window(home, provider, model)
    if window is not None and window < minimum:
        return Verdict(False, f"{model} has a context window of {window:,} tokens; Hermes needs at least "
                              f"{minimum:,}".replace(",", " "), "choose another model", window)
    if window is None:
        return Verdict(True, "the context window could not be determined; Hermes checks it on the first turn")
    return Verdict(True, "", context=window)


def public_refusal(runner: Any, event: Any, route: Any, guild: dict, target: Optional[str]) -> Optional[str]:
    """``on_unavailable: close``: no public turn while the members' model can't be applied."""
    models = guild.get("models") or {}
    if models.get("on_unavailable") != "close":
        return None
    choice = effective(guild, PUBLIC)
    if choice is None:
        return None
    k8 = RUNTIME.status.get("K8")
    if k8 is None or k8.outcome is not Outcome.WRAPPED:
        return "the members' model can't be applied on this Hermes and the operator chose to close then"
    resolver = getattr(runner, "_resolve_profile_home_for_source", None)
    source = getattr(event, "source", None)
    if not callable(resolver) or source is None:
        return "the profile of this message can't be determined"
    ok, why = availability(resolver(source), choice["provider"], choice["model"])
    return None if ok else f"the members' model {choice['model']} is unavailable ({why})"
