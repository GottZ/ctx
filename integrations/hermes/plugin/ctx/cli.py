"""CLI commands for the CTX memory plugin: ``hermes ctx <status|sync|query|store|search|get>``.

Discovered by the host at argparse setup time (``register_cli``), before the provider is
loaded. The host registers the plugin's parent package first, so the relative imports
below resolve without executing ``__init__.py``; ``client`` and ``config`` are stdlib-only
at import time. Active-provider gating applies: the commands only appear when
``memory.provider: ctx`` is set.

All commands go through the same ``CtxClient`` and helpers as the provider, so CLI writes
carry the provenance tags (``hook:cli``) and the configured sensitivity, and reads honour
``prefetch_types_exclude``.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path
from typing import Any

from .client import CtxApiError, CtxClient, store_action, store_block_id
from .config import (
    CATEGORY_MEMORY,
    CATEGORY_USER,
    UNTRUSTED_LABEL,
    VALID_SENSITIVITIES,
    ctx_config_path,
    derive_profile_name,
    derive_title,
    env_secret,
    load_ctx_config,
    merge_tags,
    resolve_base_url,
)

# Built-in memory files separate entries with a line holding only "§" (host ENTRY_DELIMITER
# "\n§\n"); a "§" inside an entry ("gemäß § 22 UStG") must not split it.
_ENTRY_SEPARATOR = "§"
_HOOK = "cli"
_SUBCOMMANDS = "status|sync|query|store|search|get"


def _hermes_home() -> Path:
    try:
        from hermes_constants import get_hermes_home

        return Path(str(get_hermes_home()))
    except Exception:
        return Path(os.environ.get("HERMES_HOME", str(Path.home() / ".hermes")))


def _ctx_json(home: Path) -> dict[str, Any]:
    """Keys explicitly set in ctx.json (for ``status``); {} when missing or unreadable."""
    try:
        raw = json.loads(ctx_config_path(str(home)).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return raw if isinstance(raw, dict) else {}


def _client(config: dict[str, Any]) -> CtxClient | None:
    base, key = resolve_base_url(config.get("ctx_base_url")), env_secret("CTX_KEY")
    if not (base and key):
        print("✗ CTX not configured: set CTX_KEY (and CTX_BASE_URL or ctx_base_url in ctx.json).")
        return None
    return CtxClient(base, key, timeout=config["api_timeout"], synthesis_timeout=config["synthesis_timeout"])


def _types_exclude(args, config: dict[str, Any]) -> list[str]:
    return [] if getattr(args, "all_types", False) else list(config.get("prefetch_types_exclude") or [])


def _parse_memory_file(path: Path) -> list[str]:
    """Entries of a built-in MEMORY.md/USER.md, split on lines holding only ``§``."""
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return []
    entries: list[list[str]] = [[]]
    for line in lines:
        if line.strip() == _ENTRY_SEPARATOR:
            entries.append([])
        else:
            entries[-1].append(line)
    return [text for text in ("\n".join(entry).strip() for entry in entries) if text]


def _hit_line(item: dict[str, Any]) -> str:
    marker = f"{UNTRUSTED_LABEL} " if item.get("untrusted") is True else ""
    btype = f" <{item['type']}>" if item.get("type") else ""
    return f"- {marker}[{item.get('category')}] {item.get('title')}{btype}  (id: {item.get('id')})"


# ---------------------------------------------------------------------------
# Commands (each returns a process exit code)
# ---------------------------------------------------------------------------


def cmd_status(args, home: Path, config: dict[str, Any]) -> int:
    base, key = resolve_base_url(config.get("ctx_base_url")), env_secret("CTX_KEY")
    print("CTX memory plugin — status")
    print(f"  profile:  {derive_profile_name(str(home))} ({home})")
    print(f"  base URL: {base or '(not set)'}")
    print(f"  key:      {'set' if key else 'NOT set'}")
    if not (base and key):
        print("  ✗ Configure CTX_KEY and CTX_BASE_URL (env, or ctx_base_url in ctx.json) first.")
        return 1
    rc = 0
    try:
        info = CtxClient(base, key, timeout=config["api_timeout"]).whoami()
    except CtxApiError as exc:
        info = {"success": False, "error": str(exc)}
    if info.get("success"):
        scope, label, role = info.get("home_scope") or "?", info.get("label") or "?", info.get("role") or "?"
        print(f"  ✓ connected · scope: {scope} · label: {label} · role: {role}")
    else:
        print(f"  ✗ {info.get('error') or 'whoami failed'}")
        rc = 1
    settings = _ctx_json(home)
    if settings:
        print("  ctx.json settings:")
        for name, value in sorted(settings.items()):
            print(f"    {name}: {value}")
    return rc


def cmd_sync(args, home: Path, config: dict[str, Any]) -> int:
    """Push built-in MEMORY.md/USER.md entries to CTX (same blocks as the live mirror)."""
    client = _client(config)
    if client is None:
        return 1
    tags = merge_tags(None, derive_profile_name(str(home)), _HOOK)
    sources = (
        (home / "memories" / "MEMORY.md", CATEGORY_MEMORY, "memory"),
        (home / "memories" / "USER.md", CATEGORY_USER, "user"),
    )
    total = failed = 0
    for path, category, label in sources:
        entries = _parse_memory_file(path)
        actions: dict[str, int] = {}
        for entry in entries:
            title = derive_title(entry, prefix=f"{label}: ")
            try:
                resp = client.store(category, title, entry, sensitivity=config["sensitivity_default"], tags=tags)
            except CtxApiError as exc:
                resp = {"success": False, "error": str(exc)}
            if resp.get("success"):
                action = store_action(resp)
                actions[action] = actions.get(action, 0) + 1
            else:
                failed += 1
                print(f"  ✗ {label}: {title}: {resp.get('error') or 'unknown error'}")
        stored = sum(actions.values())
        total += stored
        detail = ", ".join(f"{n} {a}" for a, n in sorted(actions.items()))
        print(f"  {label}: {stored}/{len(entries)} entries mirrored" + (f" ({detail})" if detail else ""))
    print(f"Done — {total} blocks stored" + (f", {failed} failed." if failed else "."))
    return 1 if failed else 0


def cmd_query(args, home: Path, config: dict[str, Any]) -> int:
    client = _client(config)
    if client is None:
        return 1
    resp = client.query(
        args.text,
        synthesize=bool(args.synthesize),
        include_content=False,
        types_exclude=_types_exclude(args, config),
    )
    if not resp.get("success"):
        print(f"✗ {resp.get('error') or 'query failed'}")
        return 1
    if args.synthesize and resp.get("answer"):
        print(resp["answer"])
        print()
    sources = resp.get("sources") or []
    for source in sources:
        print(_hit_line(source))
    if not sources:
        print("(no hits)")
    return 0


def cmd_store(args, home: Path, config: dict[str, Any]) -> int:
    client = _client(config)
    if client is None:
        return 1
    resp = client.store(
        args.category,
        args.title,
        args.content,
        sensitivity=args.sensitivity or config["sensitivity_default"],
        tags=merge_tags(args.tag, derive_profile_name(str(home)), _HOOK),
    )
    if not resp.get("success"):
        print(f"✗ {resp.get('error') or 'store failed'}")
        return 1
    block_id = store_block_id(resp)
    print(f"✓ {store_action(resp)}: [{args.category}] {args.title}" + (f" (id: {block_id})" if block_id else ""))
    return 0


def cmd_search(args, home: Path, config: dict[str, Any]) -> int:
    client = _client(config)
    if client is None:
        return 1
    resp = client.search(
        args.text,
        category=args.category or None,
        tags=args.tag or None,
        types_exclude=_types_exclude(args, config) or None,
    )
    if not resp.get("success"):
        print(f"✗ {resp.get('error') or 'search failed'}")
        return 1
    results = resp.get("results") or []
    print(f"{resp.get('count', len(results))} result(s):")
    for item in results:
        print(_hit_line(item))
    return 0


def cmd_get(args, home: Path, config: dict[str, Any]) -> int:
    client = _client(config)
    if client is None:
        return 1
    resp = client.get_block(args.block_id)
    if not resp.get("success"):
        print(f"✗ {resp.get('error') or 'get failed'}")
        return 1
    print(json.dumps(resp, indent=2, ensure_ascii=False))
    return 0


_COMMANDS = {
    "status": cmd_status,
    "sync": cmd_sync,
    "query": cmd_query,
    "store": cmd_store,
    "search": cmd_search,
    "get": cmd_get,
}


def ctx_command(args) -> None:
    """argparse handler for ``hermes ctx …`` (also found by the host as ``<provider>_command``)."""
    command = _COMMANDS.get(getattr(args, "ctx_command", None) or "")
    if command is None:
        print(f"Usage: hermes ctx <{_SUBCOMMANDS}>")
        sys.exit(2)
    home = _hermes_home()
    try:
        rc = command(args, home, load_ctx_config(str(home)))
    except CtxApiError as exc:
        print(f"✗ {exc}")
        rc = 1
    if rc:
        sys.exit(rc)


def register_cli(subparser) -> None:
    """Build the ``hermes ctx`` argparse tree (called by plugin CLI discovery)."""
    subparser.set_defaults(func=ctx_command)
    subs = subparser.add_subparsers(dest="ctx_command")

    p_status = subs.add_parser("status", help="Show CTX connection status and ctx.json settings")
    p_status.set_defaults(func=ctx_command)

    p_sync = subs.add_parser("sync", help="Mirror built-in MEMORY.md/USER.md entries to CTX")
    p_sync.set_defaults(func=ctx_command)

    p_query = subs.add_parser("query", help="Query CTX (retrieval, optional LLM synthesis)")
    p_query.add_argument("text", help="The question or topic")
    p_query.add_argument("--synthesize", action="store_true", help="Run LLM synthesis over the hits (slower)")
    p_query.add_argument("--all-types", action="store_true", help="Ignore prefetch_types_exclude")
    p_query.set_defaults(func=ctx_command)

    p_store = subs.add_parser("store", help="Store (upsert) a knowledge block")
    p_store.add_argument("category", help="Block category")
    p_store.add_argument("title", help="Block title (upsert key together with the category)")
    p_store.add_argument("content", help="Block content")
    p_store.add_argument(
        "--sensitivity", choices=VALID_SENSITIVITIES, help="Sensitivity (default: ctx.json sensitivity_default)"
    )
    p_store.add_argument("--tag", action="append", help="Topic tag (repeatable)")
    p_store.set_defaults(func=ctx_command)

    p_search = subs.add_parser("search", help="Lightweight search (no LLM)")
    p_search.add_argument("text", help="Search query")
    p_search.add_argument("--category", help="Category filter")
    p_search.add_argument("--tag", action="append", help="Tag filter, any-of (repeatable)")
    p_search.add_argument("--all-types", action="store_true", help="Ignore prefetch_types_exclude (e.g. checkpoints)")
    p_search.set_defaults(func=ctx_command)

    p_get = subs.add_parser("get", help="Fetch a full block by id")
    p_get.add_argument("block_id", help="CTX block id (UUID)")
    p_get.set_defaults(func=ctx_command)
