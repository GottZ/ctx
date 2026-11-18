"""Connection test, outcome of the latest checkpoint and the MCP status of ctx_checkpoint.

Shared by the provider (``hermes memory status``, the setup forms, the outcome of every
checkpoint, the checkpoint head its system prompt block names) and the CLI (``hermes
ctx_checkpoint status``), so both surfaces print the same report. Stdlib only at import
time — ``cli.py`` imports it during argparse setup; Hermes' own modules are imported where
they are used.
"""

from __future__ import annotations

import ipaddress
import json
import logging
import os
import re
import tempfile
import threading
import time
from collections.abc import Callable, Iterable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib.parse import unquote, urlsplit

try:
    import fcntl
except ImportError:  # Windows: writers are serialized within one process only
    fcntl = None  # type: ignore[assignment]

from .config import (
    FACT_STATE_DIR,
    _as_bool,
    ctx_config_path,
    load_ctx_config,
    overlay_from_umbrella,
    shorten,
    umbrella_section,
)
from .mcp_client import colliding_servers, collision_text, mcp_tool_name

logger = logging.getLogger(__name__)

# The outcome of the latest checkpoint of a profile, in ``$HERMES_HOME/ctx_state/``. Not a
# ``*.json`` name on purpose: ``digest.prune_state_dir`` removes ``*.json`` files untouched for
# ``state_retention_days``, and the status must still name the last checkpoint of a profile
# that compacted nothing for longer than that.
STATUS_FILE = "checkpoint-status.state"
# Writers of one profile (gateway sessions, a CLI session, threads of one process) take an
# exclusive lock on this file around reading ``last_failure`` and replacing the outcome file,
# so a success does not put back a failure older than one saved in between. A writer waits at
# most STATE_LOCK_WAIT_S and then writes unserialized: the checkpoint must not hang on it.
STATE_LOCK_FILE = "checkpoint-status.lock"
STATE_LOCK_WAIT_S = 2.0
_PROCESS_STATE_LOCK = threading.Lock()
# The outcome file also keeps the checkpoint head of each root session (``heads``), so the system
# prompt block names it in every process of the profile (gateway messages, a restart, a resumed
# session). Bounded to the roots checkpointed last: a session older than that shows the pointer
# again after its next successful checkpoint.
KNOWN_HEADS_MAX = 512
_BLOCK_ID_RE = re.compile(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}")
# Error texts in the outcome file, the status and the user warning are cut to this length
# (after redaction): long enough for the ctx error text, short enough for one notice.
ERROR_MAX_CHARS = 300
# Connect timeout of one connection test. A form save or `hermes memory status` waits for it, so
# it is shorter than Hermes' own `hermes mcp test` (the entry's connect_timeout, 30 s by
# default); an entry that sets a longer connect_timeout keeps its own. Hermes adds 10 s for the
# tool listing.
CONNECTION_TEST_TIMEOUT_S = 15.0
# MCP tools of the ctx server that every checkpoint calls (head lookup, head read, store).
CHECKPOINT_TOOLS = ("search", "get", "store")
# Plain http is fine on this computer and in the local network, not across the internet.
_LOCAL_NETWORKS = tuple(
    ipaddress.ip_network(net)
    for net in (
        "127.0.0.0/8",
        "10.0.0.0/8",
        "172.16.0.0/12",
        "192.168.0.0/16",
        "169.254.0.0/16",
        "::1/128",
        "fc00::/7",
        "fe80::/10",
    )
)
_LOCAL_NAME_SUFFIXES = (".localhost", ".local", ".internal", ".lan", ".home.arpa")
# Setup stores the ctx key in Hermes' .env under this name and references it from the MCP server
# entry; the REST transport signs in with the key under REST_API_KEY_ENV.
SETUP_API_KEY_ENV = "CTX_API_KEY"
REST_API_KEY_ENV = "CTX_KEY"
# The headers ctx reads a key from (docs/mcp.md), and the ``${VAR}`` / ``${env:VAR}`` references
# Hermes expands in config.yaml values (``hermes_cli.config``); other sources stay unresolved.
_KEY_HEADERS = ("authorization", "x-context-key")
_ENV_REFERENCE_RE = re.compile(r"\$\{\s*(?:env:\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\}")
_AUTH_SCHEME_PREFIX_RE = re.compile(r"^(?:bearer|basic|token)\s+", re.IGNORECASE)


# -- The ctx keys of a profile (redaction only) ------------------------------------------


def env_value(name: str) -> str:
    """Scope-aware environment or .env value (the host's own lookup), ``""`` if unset."""
    try:
        from hermes_cli.config import get_env_value

        return get_env_value(name) or ""
    except Exception:
        return ""


def _expand_references(raw: str) -> str | None:
    """``raw`` with every ``${VAR}`` / ``${env:VAR}`` resolved; ``None`` when one does not resolve
    or another source (``${vault:…}``) remains."""
    missing: list[str] = []

    def expand(match: re.Match) -> str:
        value = env_value(match.group(1))
        if not value:
            missing.append(match.group(1))
        return value

    value = _ENV_REFERENCE_RE.sub(expand, raw)
    return None if missing or "${" in value else value


def entry_keys(entry: Any) -> list[str]:
    """The key values an MCP server entry signs in with: its ``Authorization`` / ``X-Context-Key``
    headers, references resolved through the host's own lookup (``.env`` included), the auth
    scheme dropped. A header with a reference that does not resolve is skipped; an entry that signs
    in with ``auth`` (OAuth) or runs a command has no key here."""
    headers = entry.get("headers") if isinstance(entry, dict) else None
    keys: list[str] = []
    for header, raw in headers.items() if isinstance(headers, dict) else ():
        if str(header).strip().lower() not in _KEY_HEADERS or not isinstance(raw, str):
            continue
        value = _expand_references(raw)
        if value is None:
            continue
        value = _AUTH_SCHEME_PREFIX_RE.sub("", value.strip()).strip()
        if value:
            keys.append(value)
    return keys


def _server_entry(config: Any, name: str) -> Any:
    servers = config.get("mcp_servers") if isinstance(config, dict) else None
    return servers.get(name) if isinstance(servers, dict) else None


def _user_config() -> Any:
    from hermes_cli.config import read_user_config_raw

    return read_user_config_raw()


def _loaded_config() -> Any:
    from hermes_cli.config import load_config

    return load_config()


def profile_ctx_keys(name: str) -> tuple[str, ...]:
    """The ctx keys of this profile, for redaction only — the one resolution the provider binds and
    status, connection test and CLI redact with (#308): the keys of ``mcp_servers.<name>`` — as
    written (references resolved here, ``.env`` included) and as Hermes loads it (expanded, with a
    managed overlay) — and the :data:`SETUP_API_KEY_ENV` setup stores. The REST transport adds its
    own :data:`REST_API_KEY_ENV`. Reads config.yaml and .env, never the network; never raises."""
    keys: list[str] = []
    for read in (_user_config, _loaded_config):
        try:
            keys.extend(entry_keys(_server_entry(read(), name)))
        except Exception:
            logger.debug("ctx_checkpoint: the key of mcp_servers.%s was not resolved for redaction", name)
    from .checkpoint import bound_secret_values

    return bound_secret_values([*keys, env_value(SETUP_API_KEY_ENV)])


# -- Connection test ---------------------------------------------------------------------


def host_probe(name: str, entry: dict[str, Any], timeout: float) -> list[str]:
    """Hermes' own MCP connection test, the one behind ``hermes mcp test`` and the MCP page of
    Desktop and dashboard: connect to the server entry, list its tools, disconnect.

    ``entry`` may carry ``${VAR}`` templates (Hermes resolves them from the profile's ``.env``).
    Returns the tool names; raises on any failure, ``ImportError`` on a Hermes without it or with
    a different signature (the function is not a public plugin API).
    """
    import inspect

    from hermes_cli.mcp_config import _probe_single_server

    try:
        parameters = inspect.signature(_probe_single_server).parameters
    except (TypeError, ValueError) as exc:
        raise ImportError(f"_probe_single_server is not inspectable: {exc}") from exc
    if "connect_timeout" not in parameters:
        raise ImportError("_probe_single_server takes no connect_timeout on this Hermes")
    return [str(tool) for tool, _description in _probe_single_server(name, dict(entry), connect_timeout=timeout)]


# The function the plugin tests a connection with (test seam: replaced by a fake in the suites).
probe_server: Callable[[str, dict[str, Any], float], list[str]] = host_probe


@dataclass(frozen=True)
class ConnectionTest:
    """``ok`` is ``None`` when this Hermes cannot run the test; ``detail`` holds no secret."""

    ok: bool | None
    detail: str
    tools: int = 0


def _leaf_error(exc: BaseException) -> BaseException:
    # anyio task groups wrap the transport error; the leaf says what went wrong.
    while isinstance(exc, BaseExceptionGroup) and exc.exceptions:
        exc = exc.exceptions[0]
    return exc


def redact_error(error: Any, secrets: Iterable[str] = ()) -> str:
    """One line of error text without secrets: the given secrets, then the checkpoint redaction
    (Hermes' ``redact_sensitive_text`` plus the key/value, auth-header and bearer rules), cut to
    :data:`ERROR_MAX_CHARS`. Never raises; on a redaction failure the text is withheld."""
    if isinstance(error, BaseException):
        leaf = _leaf_error(error)
        text = str(leaf) or type(leaf).__name__
    else:
        text = str(error or "")
    try:
        secrets = tuple(secrets)
        for secret in secrets:
            if secret and len(secret) >= 4:
                text = text.replace(secret, "***")
        from .checkpoint import redact_error_text

        text = redact_error_text(text, secrets)
    except Exception:
        return "(details withheld: redaction failed)"
    return shorten(text, ERROR_MAX_CHARS)


def display_url(url: Any) -> str:
    """The URL without user info, query and fragment (they can carry credentials): scheme, host,
    port and path. A URL that starts with a ``${VAR}`` template keeps the template and loses its
    query, its fragment and any user info after ``://``. Never the text of a URL it cannot take
    apart — no ``scheme://host`` (``user:pass@host/mcp``, ``host:8443/mcp``), an unreadable port or
    host, an ``@`` in the path (``https://user/x:pass@host``: where the user info ends is unclear) —
    those are :data:`UNREADABLE_URL` (#311)."""
    raw = str(url or "").strip()
    if not raw:
        return ""
    if raw.startswith("${"):
        return _display_template_url(raw.split("?", 1)[0].split("#", 1)[0])
    try:
        parts = urlsplit(raw)
        host = parts.hostname or ""
        port = parts.port
    except ValueError:
        return UNREADABLE_URL
    if not (parts.scheme and parts.netloc and host) or "@" in parts.path:
        return UNREADABLE_URL
    if ":" in host:
        host = f"[{host}]"
    return f"{parts.scheme}://{host}{f':{port}' if port else ''}{parts.path}"


# What :func:`display_url` shows for a URL it cannot take apart.
UNREADABLE_URL = "(unreadable URL)"
# The connection test's error detail for such a URL with an ``@`` in it (see run_connection_test).
URL_DETAILS_WITHHELD = (
    "details withheld: the URL cannot be taken apart (write it as https://host/mcp; user info, if any, before the host)"
)


def _display_template_url(head: str) -> str:
    """:func:`display_url` of a URL that starts with a template, query and fragment already cut."""
    if "@" not in head:
        return head
    scheme, separator, rest = head.partition("://")
    authority, slash, path = rest.partition("/")
    if not separator or "@" in path:
        return UNREADABLE_URL
    return f"{scheme}://{authority.rsplit('@', 1)[1]}{slash}{path}"


def url_secret_parts(url: Any) -> tuple[str, ...]:
    """The parts of a configured URL that can carry a credential — user info (whole, user name and
    password, as written and percent-decoded), query and fragment — longest first, for masking where
    a text repeats them outside the URL (``nonnumeric port: '<password>@host'``). ``()`` for a URL
    without them or one that cannot be taken apart."""
    raw = str(url or "").strip()
    try:
        parts = urlsplit(raw)
        userinfo = parts.netloc.rpartition("@")[0]
        values = [userinfo, parts.username or "", parts.password or "", parts.query, parts.fragment]
    except ValueError:
        return ()
    values += [unquote(value) for value in values]
    return tuple(sorted({value for value in values if value}, key=lambda value: (-len(value), value)))


def _test_timeout(entry: dict[str, Any]) -> float:
    """:data:`CONNECTION_TEST_TIMEOUT_S`, or the entry's own longer ``connect_timeout``."""
    try:
        own = float(entry.get("connect_timeout") or 0)
    except (TypeError, ValueError):
        own = 0.0
    return max(CONNECTION_TEST_TIMEOUT_S, own)


def run_connection_test(
    name: str, entry: dict[str, Any], *, secrets: Iterable[str] = (), servers: dict[str, Any] | None = None
) -> ConnectionTest:
    """Test one ctx MCP server entry; never raises. ``servers``: the ``mcp_servers`` entries of the
    profile, for the check that no other entry gets the same tool names. A probe error is redacted
    with ``secrets`` and the keys of ``entry`` itself (:func:`entry_keys`), so the key of the entry
    under test is masked whatever the caller passed (#308)."""
    url = entry.get("url")
    # A probe error can repeat the URL or parts of it (Hermes quotes a URL it refuses): its user info,
    # query and fragment are masked like a key (#311).
    secrets = (*secrets, *entry_keys(entry), *url_secret_parts(url))
    timeout = _test_timeout(entry)
    where = f"mcp_servers.{name}"
    others = colliding_servers(name, servers)
    if others:
        return ConnectionTest(False, f"connection test of {where}: {collision_text(name, others)}")
    if url:
        where += f" ({display_url(url)})"
    try:
        tools = probe_server(name, entry, timeout)
    except ImportError:
        return ConnectionTest(None, f"{where}: this Hermes has no MCP connection test; try `hermes mcp test {name}`")
    except Exception as exc:
        if url and "@" in str(url) and display_url(url) == UNREADABLE_URL:
            # Where its user info ends is unclear, so no part of the error text is known to be free of it.
            return ConnectionTest(False, f"connection test of {where} failed: {URL_DETAILS_WITHHELD}")
        return ConnectionTest(False, f"connection test of {where} failed: {redact_error(exc, secrets)}")
    missing = [tool for tool in CHECKPOINT_TOOLS if tool not in tools]
    if missing:
        return ConnectionTest(
            False,
            f"connection test of {where}: the server answered without the ctx tools {', '.join(missing)}; "
            "the URL must be the /mcp endpoint of a ctx server",
            len(tools),
        )
    detail = f"{where} answered with {len(tools)} tools"
    names = [mcp_tool_name(name, tool) for tool in CHECKPOINT_TOOLS]
    if names != [f"mcp__{name}__{tool}" for tool in CHECKPOINT_TOOLS]:
        detail += f"; Hermes names them {', '.join(names)}"  # the entry name is rewritten (#297)
    return ConnectionTest(True, detail, len(tools))


def insecure_http(url: Any) -> bool:
    """``http://`` to a host outside this computer and the local network."""
    try:
        parts = urlsplit(str(url or "").strip())
        host = (parts.hostname or "").lower()
    except ValueError:
        return False
    if parts.scheme.lower() != "http" or not host:
        return False
    if host == "localhost" or host.endswith(_LOCAL_NAME_SUFFIXES) or ("." not in host and ":" not in host):
        return False  # a name of this machine or the local network (a single label: docker, LAN)
    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        return True  # a public DNS name
    return not any(address in network for network in _LOCAL_NETWORKS)


def http_warning(url: Any) -> str:
    return (
        f"{display_url(url)} uses http:// to a host outside this computer and your local network: the ctx key "
        "and your conversations travel unencrypted. Use https://."
    )


# -- Operating mode ----------------------------------------------------------------------


def checkpoint_required(config: Any) -> bool:
    """``compression.checkpoint_required`` of a loaded Hermes config (Hermes' default: off)."""
    compression = config.get("compression") if isinstance(config, dict) else None
    return isinstance(compression, dict) and _as_bool(compression.get("checkpoint_required"), False)


def mode_text(required: bool) -> str:
    if required:
        return (
            "required — Hermes compacts only after ctx confirmed the checkpoint; while ctx cannot be reached "
            "nothing is compacted (compression.checkpoint_required: true)"
        )
    return (
        "best effort — a failed checkpoint is reported and Hermes compacts anyway; gateway hygiene, /compress "
        "in a chat channel, sub-agents and the curator compact without a checkpoint "
        "(compression.checkpoint_required: false; Settings → Memory → ctx_checkpoint or "
        "`hermes memory setup ctx_checkpoint` turns it on)"
    )


def checkpoint_mode_warning(
    engine_config: dict[str, Any], section: dict[str, Any], hermes_home: str, required: bool
) -> str:
    """The warning for ``checkpoint_mode: off`` (it turns the provider to checkpoint API v1), or ``""``."""
    if str(engine_config.get("checkpoint_mode") or "") != "off":
        return ""
    source = (
        "memory.ctx_checkpoint in config.yaml" if "checkpoint_mode" in section else str(ctx_config_path(hermes_home))
    )
    text = (
        f"ctx_checkpoint: checkpoint_mode: off (from {source}) switches the checkpoint off: the provider reports "
        "checkpoint API v1 and saves no conversation before compaction."
    )
    if required:
        return text + (
            " compression.checkpoint_required is on, so Hermes refuses every compaction; remove checkpoint_mode "
            "or set it to best_effort."
        )
    return text + " Remove checkpoint_mode or set it to best_effort to save checkpoints again."


def checkpoint_chain_warning(
    engine_config: dict[str, Any], section: dict[str, Any], hermes_home: str, required: bool
) -> str:
    """The warning for ``checkpoint_chain: false`` (no transcript chain is written), or ``""``."""
    if engine_config.get("checkpoint_chain", True) is not False:
        return ""
    source = (
        "memory.ctx_checkpoint in config.yaml" if "checkpoint_chain" in section else str(ctx_config_path(hermes_home))
    )
    text = (
        f"ctx_checkpoint: checkpoint_chain: false (from {source}) writes no transcript chain: no compaction saves "
        "the conversation's wording to ctx (facts of the memory features are not a transcript checkpoint)."
    )
    if required:
        return text + (
            " compression.checkpoint_required is on, so Hermes refuses every compaction; remove checkpoint_chain "
            "or set it to true."
        )
    return text + " Remove checkpoint_chain or set it to true to save checkpoints again."


def setting_warnings(
    engine_config: dict[str, Any], section: dict[str, Any], hermes_home: str, required: bool
) -> list[str]:
    """The warnings for settings that switch the checkpoint off (session start and status)."""
    texts = (
        checkpoint_mode_warning(engine_config, section, hermes_home, required),
        checkpoint_chain_warning(engine_config, section, hermes_home, required),
    )
    return [text for text in texts if text]


# -- Outcome of the latest checkpoint ----------------------------------------------------


def status_path(hermes_home: str) -> Path:
    return Path(hermes_home) / FACT_STATE_DIR / STATUS_FILE


def read_state(hermes_home: str) -> dict[str, Any]:
    try:
        raw = json.loads(status_path(hermes_home).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return raw if isinstance(raw, dict) else {}


def checkpoint_outcome(
    *,
    required: bool,
    receipt: Any = None,
    error: Any = None,
    skipped: str = "",
    secrets: Iterable[str] = (),
    context: str = "",
) -> dict[str, Any]:
    """Time, mode, head id, part count and the redacted error of one checkpoint attempt;
    ``context`` names the session kind (``<platform>/<agent context>``)."""
    outcome: dict[str, Any] = {
        "at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "mode": "required" if required else "best_effort",
        "context": context,
        "ok": False,
        "head_id": "",
        "parts": 0,
        "error": "",
        "skipped": "",
    }
    if error is not None:
        outcome["error"] = redact_error(error, secrets)
    elif receipt is not None:
        outcome.update(ok=True, head_id=str(receipt.head_id or ""), parts=len(receipt.part_ids or []))
    else:
        outcome["skipped"] = skipped
    return outcome


@contextmanager
def _state_lock(directory: Path) -> Iterator[None]:
    """Exclusive lock of the outcome file's writers (see :data:`STATE_LOCK_FILE`); never raises."""
    if fcntl is None:
        held = _PROCESS_STATE_LOCK.acquire(timeout=STATE_LOCK_WAIT_S)
        try:
            yield
        finally:
            if held:
                _PROCESS_STATE_LOCK.release()
        return
    fd = -1
    try:
        directory.mkdir(parents=True, exist_ok=True)
        fd = os.open(str(directory / STATE_LOCK_FILE), os.O_RDWR | os.O_CREAT, 0o600)
        deadline = time.monotonic() + STATE_LOCK_WAIT_S
        while True:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    logger.debug("ctx_checkpoint: outcome file lock busy; writing unserialized")
                    break
                time.sleep(0.01)
    except OSError:
        logger.debug("ctx_checkpoint: outcome file lock unavailable", exc_info=True)
    try:
        yield
    finally:
        if fd >= 0:
            os.close(fd)  # releases the lock


def record_checkpoint(
    hermes_home: str, outcome: dict[str, Any], *, root: str = "", head_id: str = "", binding: str = ""
) -> None:
    """Replace the outcome file atomically (mode 0600): ``last_checkpoint`` is this outcome,
    ``last_failure`` the latest outcome with an error (kept when this one succeeded, so a session
    that keeps failing stays visible next to others that succeed), ``heads`` the checkpoint head
    of each root session (``head_id`` of this attempt's chain under ``root``, when it wrote one,
    with the ``binding`` it was written through).
    Reading the previous state and the replacement run under :func:`_state_lock`. Never raises:
    the checkpoint path must not depend on a writable state directory."""
    if not hermes_home:
        return
    path = status_path(hermes_home)
    try:
        with _state_lock(path.parent):
            _replace_state(hermes_home, path, outcome, root=root, head_id=head_id, binding=binding)
    except Exception:
        logger.debug("ctx_checkpoint: outcome file not written: %s", path, exc_info=True)


def _known_heads(state: dict[str, Any], root: str, head_id: str, binding: str) -> dict[str, Any]:
    """``heads`` of the previous state with ``root`` → ``head_id`` (and ``binding``) set when given,
    the :data:`KNOWN_HEADS_MAX` roots checkpointed last."""
    raw = state.get("heads")
    heads = {
        key: value
        for key, value in (raw.items() if isinstance(raw, dict) else ())
        if isinstance(value, dict) and isinstance(value.get("at"), (int, float))
    }
    if root and head_id:
        heads[root] = {"head_id": head_id, "binding": binding, "at": time.time()}
    if len(heads) > KNOWN_HEADS_MAX:
        keep = set(sorted(heads, key=lambda key: heads[key]["at"], reverse=True)[:KNOWN_HEADS_MAX])
        heads = {key: value for key, value in heads.items() if key in keep}
    return heads


def known_head(hermes_home: str, root: str, *, binding: str = "") -> str:
    """The checkpoint head id recorded for ``root`` through ``binding``; ``""`` when none, when it
    was written through another binding, or when it is not a block id."""
    if not (hermes_home and root):
        return ""
    heads = read_state(hermes_home).get("heads")
    entry = heads.get(root) if isinstance(heads, dict) else None
    if not isinstance(entry, dict) or str(entry.get("binding") or "") != binding:
        return ""
    head = str(entry.get("head_id") or "")
    return head if _BLOCK_ID_RE.fullmatch(head) else ""


def _replace_state(
    hermes_home: str, path: Path, outcome: dict[str, Any], *, root: str = "", head_id: str = "", binding: str = ""
) -> None:
    previous = read_state(hermes_home)
    state: dict[str, Any] = {"last_checkpoint": outcome}
    failure = outcome if outcome.get("error") else previous.get("last_failure")
    if isinstance(failure, dict):
        state["last_failure"] = failure
    heads = _known_heads(previous, root, head_id, binding)
    if heads:
        state["heads"] = heads
    state["updated"] = time.time()
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".status-", dir=str(path.parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(state, fh)
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def outcome_line(outcome: Any) -> str:
    if not isinstance(outcome, dict) or not outcome.get("at"):
        return "none recorded yet (the next compaction writes one)"
    at = str(outcome["at"]).replace("T", " ").rstrip("Z")[:16]
    mode = "required" if outcome.get("mode") == "required" else "best effort"
    prefix = f"{at} UTC · {mode}"
    if outcome.get("context"):
        prefix += f" · {outcome['context']}"
    if outcome.get("error"):
        return f"{prefix} · FAILED: {outcome['error']}"
    if outcome.get("skipped"):
        return f"{prefix} · not written: {outcome['skipped']}"
    head = str(outcome.get("head_id") or "")
    if not head:
        return f"{prefix} · nothing to archive (no user/assistant text)"
    parts = int(outcome.get("parts") or 0)
    return f"{prefix} · saved · head {head} · {parts} part{'' if parts == 1 else 's'}"


def failure_warning(outcome: dict[str, Any], server: str) -> str:
    """The notice a user gets when a checkpoint failed (Hermes' warning callback)."""
    cause = str(outcome.get("error") or "")
    if outcome.get("mode") == "required":
        head = "ctx checkpoint failed, so Hermes has not compacted this conversation (nothing is lost)."
    else:
        head = "ctx checkpoint failed; Hermes compacted the conversation without saving its wording to ctx."
    return f"{head} Cause: {cause}. {_failure_hint(cause, server)}"


def skip_warning(outcome: dict[str, Any]) -> str:
    """The notice a user gets when a compaction ran without a checkpoint and without an error."""
    return (
        f"ctx checkpoint not written ({outcome.get('skipped') or 'no reason recorded'}); Hermes compacted the "
        "conversation without saving its wording to ctx. `hermes ctx_checkpoint status` shows the settings "
        "and tests the connection."
    )


def _failure_hint(cause: str, server: str) -> str:
    # ``mcp_tool_name(server, "")`` is the prefix of every tool of the entry (a shortened name is
    # matched whole).
    if any(f"Unknown tool: {mcp_tool_name(server, tool)}" in cause for tool in ("", *CHECKPOINT_TOOLS)):
        return (
            f"The ctx MCP server '{server}' is not connected in this Hermes: check it with "
            f"`hermes mcp test {server}`; a new session connects again."
        )
    if "budget" in cause and "exceeded" in cause:
        return (
            "ctx did not finish within budget_s (Settings → Plugins → ctx_checkpoint); a retry of the same "
            "conversation reuses the parts already saved. `hermes ctx_checkpoint status` tests the connection."
        )
    if "rate-limited" in cause:
        return (
            "ctx limits writes per key (100 per minute by default) and other writers on the key kept that "
            "window full; the next compaction tries again, and a retry of the same conversation reuses the "
            "parts already saved."
        )
    if "confirm_writes" in cause:
        return "The ctx key stages its writes for confirmation (confirm_writes); use a key without it."
    return "`hermes ctx_checkpoint status` tests the connection."


# -- Status on the MCP transport ---------------------------------------------------------


def mcp_status(
    *,
    name: str,
    entry: dict[str, Any],
    required: bool,
    engine_config: dict[str, Any],
    section: dict[str, Any],
    hermes_home: str,
    secrets: Iterable[str] = (),
    servers: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """The report of ``hermes memory status`` and ``hermes ctx_checkpoint status`` (one line
    per key). Runs the connection test (``servers``: the profile's ``mcp_servers``, see
    :func:`run_connection_test`); never raises."""
    status: dict[str, Any] = {"summary": "", "ok": False, "transport": "mcp", "mcp_server": f"mcp_servers.{name}"}
    url = entry.get("url") if entry else None
    if url:
        status["url"] = display_url(url)
    if not entry:
        status["summary"] = (
            f"✗ no MCP server entry mcp_servers.{name} — run `hermes memory setup ctx_checkpoint` "
            "or fill in Settings → Memory → ctx_checkpoint"
        )
    elif entry.get("enabled") is False:
        status["summary"] = f"✗ mcp_servers.{name} is disabled — `hermes memory setup ctx_checkpoint` turns it on again"
    else:
        test = run_connection_test(name, entry, secrets=secrets, servers=servers)
        status["ok"] = test.ok
        status["summary"] = (
            f"✓ connected: {test.detail}" if test.ok else f"{'?' if test.ok is None else '✗'} {test.detail}"
        )
    status["mode"] = mode_text(required)
    state = read_state(hermes_home)
    status["last_checkpoint"] = outcome_line(state.get("last_checkpoint"))
    failure = state.get("last_failure")
    if isinstance(failure, dict) and failure != state.get("last_checkpoint"):
        status["last_failure"] = outcome_line(failure)
    warnings = []
    if url and insecure_http(url):
        warnings.append(http_warning(url))
    warnings.extend(setting_warnings(engine_config, section, hermes_home, required))
    for index, warning in enumerate(warnings):
        status["warning" if index == 0 else f"warning_{index + 1}"] = warning
    return status


def mcp_report(hermes_home: str) -> dict[str, Any]:
    """:func:`mcp_status` for the profile Hermes has loaded: the plugin's settings, the named
    ``mcp_servers`` entry as written (``${VAR}`` templates unexpanded), the operating mode and the
    profile's ctx keys (:func:`profile_ctx_keys`, only to keep them out of the report)."""
    config = _loaded_config()
    section = umbrella_section(config)
    name = str(section.get("mcp_server") or "ctx")
    servers = _user_config().get("mcp_servers")
    entry = servers.get(name) if isinstance(servers, dict) else None
    return mcp_status(
        name=name,
        entry=entry if isinstance(entry, dict) else {},
        required=checkpoint_required(config),
        engine_config=load_ctx_config(hermes_home, overlay=overlay_from_umbrella(section)),
        section=section,
        hermes_home=hermes_home,
        secrets=profile_ctx_keys(name),
        servers=servers if isinstance(servers, dict) else {},
    )
