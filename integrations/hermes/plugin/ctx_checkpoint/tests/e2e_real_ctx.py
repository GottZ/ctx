"""Checkpoint chain against a real ctx MCP endpoint, through Hermes' own MCP client (GottZ/ctx#290).

Not a pytest module: ``go/internal/handler/mcp_checkpoint_e2e_integration_test.go`` starts the
ctx MCP handler over HTTP on a fresh database and runs this script against it (the workflow
``hermes-ctx-checkpoint.yml``, job ``e2e``). The chain goes the way it goes in Hermes: the
plugin's ``McpClient`` over ``tools.registry.registry.dispatch``, Hermes' MCP client over
Streamable HTTP to ctx. Nothing is faked.

Checked: a first write stores every part, the manifest and the head; the same transcript
again (a retry after a compaction that did not land) is answered "No change needed" for every
part and rewrites manifest and head in place (same ids); get returns the head and the
manifest with the ids the receipt names; another root with the same transcript lists the
stored parts (every part "No change needed") under a manifest and head of its own, and a part
names no conversation (GottZ/ctx#344). A fact stored as the plugin stores it carries the block type
``session-digest`` and reads back framed; a type the server does not know is refused through Hermes'
MCP client and the fact is stored without it (GottZ/ctx#289; ``CTX_E2E_SERVER=before-196``: the
endpoint itself does not know ``session-digest``).

Environment: ``CTX_E2E_URL`` (the MCP endpoint), ``CTX_E2E_KEY`` (a key of that database),
``HERMES_REPO`` (the hermes-agent checkout on ``sys.path``). Exit 0 = pass; the last line of the
output is a JSON summary.
"""

from __future__ import annotations

import importlib.util
import json
import os
import sys
from pathlib import Path

ENGINE_DIR = Path(__file__).resolve().parents[1] / "memory"
HANDOVER_ENGINE_DIR = Path(__file__).resolve().parents[2] / "ctx_handover" / "engine"
ROOT = "20261116_120000_e2e290"
OTHER_ROOT = "20261118_120000_e2e344"
TYPES_REFUSED = json.loads((ENGINE_DIR / "tests" / "mcp_answers.json").read_text(encoding="utf-8"))["search"][
    "types_refused"
]
NEVER_RANKED = ("checkpoint", "system-meta")
SESSION_DIGEST = json.loads((ENGINE_DIR / "tests" / "mcp_answers.json").read_text(encoding="utf-8"))["session_digest"]


def _fail(message: str) -> None:
    print(f"FAIL: {message}", flush=True)
    raise SystemExit(1)


def _load_engine():
    spec = importlib.util.spec_from_file_location(
        "ctx_plugin", ENGINE_DIR / "__init__.py", submodule_search_locations=[str(ENGINE_DIR)]
    )
    if spec is None or spec.loader is None:
        _fail(f"cannot load the engine from {ENGINE_DIR}")
    module = importlib.util.module_from_spec(spec)
    sys.modules["ctx_plugin"] = module
    spec.loader.exec_module(module)


def _transcript() -> list[dict[str, str]]:
    turns = []
    for n in range(4):
        turns.append({"role": "user", "content": f"Frage {n}: wie ist der Stand der Welle? " * 18})
        turns.append({"role": "assistant", "content": f"Antwort {n}: der Stand ist belegt und gemessen. " * 18})
    return turns


class Server:
    """What a search reaches: the real ctx MCP endpoint, or that endpoint seen as another ctx would answer
    (GottZ/ctx#342). ``refuses``: a search whose ``types`` name ``checkpoint`` gets the error a ctx server before
    #342 answers — the fixture's ``search.types_refused``, which the ctx side checks against its own code — in the
    form Hermes hands a tool error over (``{"error": "<text>"}``). ``drops_never_ranked``: a search without
    ``types`` answers without checkpoint and system-meta rows, as the later step of #342 will."""

    def __init__(self) -> None:
        self.refuses = False
        self.drops_never_ranked = False
        self.searches: list[dict] = []

    def answer(self, tool: str, args: dict, call):
        if tool != "search":
            return call(args)
        self.searches.append(dict(args))
        if self.refuses and "checkpoint" in (args.get("types") or []):
            return json.dumps({"error": TYPES_REFUSED}, ensure_ascii=False)
        raw = call(args)
        if not self.drops_never_ranked or args.get("types"):
            return raw
        envelope = json.loads(raw)
        rows = [row for row in json.loads(envelope["result"]) if row.get("type") not in NEVER_RANKED]
        return json.dumps({"result": json.dumps(rows, ensure_ascii=False)}, ensure_ascii=False)


def _load_handover_records():
    """ctx_handover's ``engine/ctxrecords.py``, loaded by path the way its dashboard loads the engine."""
    spec = importlib.util.spec_from_file_location(
        "ctxh_engine", HANDOVER_ENGINE_DIR / "__init__.py", submodule_search_locations=[str(HANDOVER_ENGINE_DIR)]
    )
    if spec is None or spec.loader is None:
        _fail(f"cannot load the ctx_handover engine from {HANDOVER_ENGINE_DIR}")
    module = importlib.util.module_from_spec(spec)
    sys.modules["ctxh_engine"] = module
    spec.loader.exec_module(module)
    import ctxh_engine.ctxrecords as ctxrecords

    return ctxrecords


def _head_lookups(registry, McpClient, ChainWriter, first) -> dict:
    """The head lookups of both plugins (GottZ/ctx#342) against this endpoint: with ``types`` on this server, and
    on the same server answering as a server before #342, as one after the later step, and as one that is updated
    from the first to the second while the client runs. ``CTX_E2E_SERVER=before-342`` says the endpoint itself is
    a server before #342: then the first lookup must be refused and fall back."""
    from hermes_cli.plugins_context import PluginContext
    from tools.mcp_tool_handlers import _make_tool_handler

    ctxrecords = _load_handover_records()
    real_before_342 = os.environ.get("CTX_E2E_SERVER", "") == "before-342"
    real_leaves_out = os.environ.get("CTX_E2E_SERVER", "") == "leaves-out-never-ranked"
    out: dict = {}

    def checkpoint_lookup(label: str, server: Server, client=None):
        def dispatch(name, args):
            return server.answer(name.rsplit("__", 1)[-1], args, lambda a: registry.dispatch(name, a))

        from ctx_plugin.checkpoint import _ChainRun

        client = client or McpClient(dispatch, "ctx")
        writer = ChainWriter(client=client, chunk_chars=1_000)
        run = _ChainRun(deadline=writer.monotonic() + 60.0, budget_s=60.0, window_writes=writer._window_writes())
        manifest_id, _hashes, _checkpoint_id = writer.load_previous_head(_head_title(), run)
        if manifest_id != first.manifest_id:
            _fail(f"{label}: ctx_checkpoint's head lookup answered manifest {manifest_id!r}, want {first.manifest_id}")
        out[f"checkpoint {label}"] = [s.get("types") for s in server.searches]
        server.searches.clear()
        return client

    def handover_lookup(label: str, server: Server, sent: list, *, finds: bool = True):
        """ctx_handover's head search for ROOT: it finds the head (or, ``finds=False``, not) and sends ``sent``."""

        def call_mcp(name, tool, args, timeout):
            handler = _make_tool_handler(name, tool, max(1.0, float(timeout)))
            return PluginContext._mcp_envelope(server.answer(tool, dict(args), handler))

        head = ctxrecords._checkpoint_head(call_mcp, "ctx", ROOT, 10.0)
        if head != (first.head_id if finds else None):
            _fail(f"{label}: ctx_handover found head {head!r}, want {first.head_id if finds else None}")
        out[f"handover {label}"] = [s.get("types") for s in server.searches]
        if out[f"handover {label}"] != sent:
            _fail(f"{label}: ctx_handover sent {out[f'handover {label}']}, want {sent}")
        server.searches.clear()

    real = Server()
    client = checkpoint_lookup("this server", real)
    if out["checkpoint this server"][:1] != [["checkpoint"]] or bool(client._refused_types) != real_before_342:
        _fail(f"this server: searches {out['checkpoint this server']}, refused {client._refused_types}")
    ctxrecords._types_refused.clear()
    handover_lookup("this server", real, [["checkpoint"], None] if real_before_342 else [["checkpoint"]])

    if real_leaves_out:
        # The endpoint itself leaves never-ranked types out of a search without types (search.leave_out_never_ranked
        # on): both lookups above found the head because they name the type, and a search without it does not. A
        # server that refuses the type cannot also leave it out, so the simulations below do not apply.
        bare = McpClient(registry.dispatch, "ctx")
        if any(
            row.get("id") == first.head_id for row in bare.search(_head_title(), tags=["checkpoint-head"])["results"]
        ):
            _fail("this server leaves never-ranked types out, yet a search without types finds the head")
        return out

    old = Server()
    old.refuses = True
    client = checkpoint_lookup("before 342", old)
    if client._refused_types != {"checkpoint"}:
        _fail(f"before 342: the client did not remember the refusal ({client._refused_types})")
    ctxrecords._types_refused.clear()
    handover_lookup("before 342", old, [["checkpoint"], None])
    handover_lookup("before 342, remembered", old, [None])

    if not real_before_342:
        later = Server()
        later.drops_never_ranked = True
        bare = McpClient(lambda name, args: later.answer("search", args, lambda a: registry.dispatch(name, a)), "ctx")
        if any(
            row.get("id") == first.head_id for row in bare.search(_head_title(), tags=["checkpoint-head"])["results"]
        ):
            _fail("after the later step: a search without types still finds the head (the simulation is not one)")
        later.searches.clear()
        checkpoint_lookup("after the later step", later)
        ctxrecords._types_refused.clear()
        handover_lookup("after the later step", later, [["checkpoint"]])

        # A client that met a server before #342, the server then updated to the later step.
        updated = Server()
        updated.refuses = True
        client = checkpoint_lookup("updated meanwhile (before)", updated)
        updated.refuses, updated.drops_never_ranked = False, True
        checkpoint_lookup("updated meanwhile (after)", updated, client)
        if out["checkpoint updated meanwhile (after)"] != [None, ["checkpoint"]] or client._refused_types:
            _fail(f"updated meanwhile: ctx_checkpoint sent {out['checkpoint updated meanwhile (after)']}")
        # ctx_handover remembers the refusal; after the update the first search without the type misses (that
        # record goes without the link) and drops the memory, so the next asks with the type and finds the head.
        ctxrecords._types_refused.clear()
        updated.refuses, updated.drops_never_ranked = True, False
        handover_lookup("updated meanwhile (before)", updated, [["checkpoint"], None])
        handover_lookup("updated meanwhile (before, remembered)", updated, [None])
        updated.refuses, updated.drops_never_ranked = False, True
        handover_lookup("updated meanwhile (after, the miss)", updated, [None], finds=False)
        handover_lookup("updated meanwhile (after)", updated, [["checkpoint"]])
    return out


def _session_digest_type(client) -> dict:
    """GottZ/ctx#289: a fact stored as the plugin stores it (``store_typed`` with the entry's state) carries
    the block type ``session-digest`` and comes back framed; a type the server does not know is refused
    through Hermes' MCP client with the text the plugin reads, and the write is stored without it, once.
    ``CTX_E2E_SERVER=before-196`` says the endpoint itself does not know ``session-digest``."""
    from ctx_plugin.checkpoint import EntryState, store_typed

    before_196 = os.environ.get("CTX_E2E_SERVER", "") == "before-196"
    out: dict = {}
    for label, block_type in (("plugin_type", SESSION_DIGEST["type"]), ("unknown_type", "session-digest-e2e-unknown")):
        entry = EntryState()
        refused = block_type != SESSION_DIGEST["type"] or before_196
        title = f"fact: e2e 289 {label} — the staging proxy keeps its previous image as rollback"
        data = store_typed(
            entry,
            block_type,
            client.store,
            category="learnings",
            title=title,
            content=f"The staging proxy keeps its previous image as rollback ({label}).",
            sensitivity="internal",
        )
        block_id = (data.get("block") or {}).get("id") if isinstance(data, dict) else None
        if not block_id:
            _fail(f"{label}: the store answered {data!r}")
        block = client.get_block(block_id).get("block") or {}
        want = "knowledge" if refused else SESSION_DIGEST["type"]
        if block.get("type") != want:
            _fail(f"{label}: stored as {block.get('type')!r}, want {want!r}")
        if (block.get("untrusted") is True) == refused:
            _fail(f"{label}: untrusted={block.get('untrusted')!r} for a block of type {block.get('type')!r}")
        verdict = entry.type_to_send(block_type)
        want_verdict = "" if refused else "known"
        if verdict != want_verdict:
            _fail(f"{label}: the entry's verdict on {block_type!r} is {verdict!r}, want {want_verdict!r}")
        out[label] = {"type": block.get("type"), "untrusted": bool(block.get("untrusted")), "refused": refused}
    return out


def _head_title() -> str:
    from ctx_plugin.checkpoint import head_title_for

    return head_title_for(ROOT)


def main() -> int:
    url, key = os.environ.get("CTX_E2E_URL", ""), os.environ.get("CTX_E2E_KEY", "")
    hermes = os.environ.get("HERMES_REPO", "")
    if not (url and key and hermes):
        _fail("CTX_E2E_URL, CTX_E2E_KEY and HERMES_REPO are required")
    sys.path.insert(0, hermes)
    _load_engine()

    from tools.mcp_tool_discovery import register_mcp_servers
    from tools.mcp_tool_lifecycle import shutdown_mcp_servers
    from tools.registry import registry

    from ctx_plugin.checkpoint import ChainWriter
    from ctx_plugin.mcp_client import McpClient

    names = register_mcp_servers({"ctx": {"url": url, "headers": {"Authorization": f"Bearer {key}"}, "timeout": 60}})
    try:
        for tool in ("store", "get", "search"):
            if f"mcp__ctx__{tool}" not in names:
                _fail(f"Hermes did not register mcp__ctx__{tool}; registered: {sorted(names)}")
        client = McpClient(registry.dispatch, "ctx")
        writer = ChainWriter(client=client, chunk_chars=1_000)
        messages = _transcript()

        first = writer.write(messages, root_session_id=ROOT, session_id=ROOT, budget_s=60.0, host_api=2)
        parts = len(first.part_ids)
        if parts < 2 or not first.head_id or not first.manifest_id:
            _fail(f"first write: {parts} parts, head {first.head_id!r}, manifest {first.manifest_id!r}")
        if (first.writes, first.reused) != (parts + 2, 0):
            _fail(f"first write: writes={first.writes} reused={first.reused}, want {parts + 2} and 0")

        again = writer.write(messages, root_session_id=ROOT, session_id=ROOT, budget_s=60.0, host_api=2)
        if (again.head_id, again.manifest_id, again.part_ids) != (first.head_id, first.manifest_id, first.part_ids):
            _fail(f"the retry made other blocks: {again} vs {first}")
        # Parts carry no evidence_date, so ctx answers each "No change needed"; manifest and head
        # keep it and are written again in place (memory/CHANGELOG.md 0.10.0, idempotent retries).
        if (again.reused, again.writes) != (parts, 2):
            _fail(f"the retry: reused={again.reused} writes={again.writes}, want {parts} and 2 (manifest, head)")

        head = client.get_block(first.head_id).get("block") or {}
        manifest = client.get_block(first.manifest_id).get("block") or {}
        head_meta, manifest_meta = head.get("metadata") or {}, manifest.get("metadata") or {}
        if head_meta.get("latest_manifest_id") != first.manifest_id:
            _fail(f"the head names manifest {head_meta.get('latest_manifest_id')!r}, want {first.manifest_id}")
        if manifest_meta.get("source_block_ids") != first.part_ids:
            _fail(f"the manifest names parts {manifest_meta.get('source_block_ids')!r}, want {first.part_ids}")
        if head.get("type") != "checkpoint" or manifest.get("type") != "checkpoint":
            _fail(f"types: head {head.get('type')!r}, manifest {manifest.get('type')!r}")

        # Another conversation with the same text (a branch, GottZ/ctx#344): ctx's own
        # identical-content check answers every part "No change needed" and names the stored
        # block; only the other root's manifest and head are written.
        other = writer.write(messages, root_session_id=OTHER_ROOT, session_id=OTHER_ROOT, budget_s=60.0, host_api=2)
        if other.part_ids != first.part_ids:
            _fail(f"the other root lists other parts: {other.part_ids} vs {first.part_ids}")
        if (other.reused, other.writes) != (parts, 2):
            _fail(f"the other root: reused={other.reused} writes={other.writes}, want {parts} and 2")
        if other.head_id == first.head_id or other.manifest_id == first.manifest_id:
            _fail(f"the other root shares a head or manifest: {other} vs {first}")
        other_manifest = (client.get_block(other.manifest_id).get("block") or {}).get("metadata") or {}
        if (
            other_manifest.get("root_session_id") != OTHER_ROOT
            or other_manifest.get("source_block_ids") != first.part_ids
        ):
            _fail(f"the other root's manifest: {other_manifest!r}")
        part = client.get_block(first.part_ids[0]).get("block") or {}
        if part.get("id") != first.part_ids[0] or "## Direct transcript" not in str(part.get("content") or ""):
            _fail(f"the part {first.part_ids[0]} was not read back: {sorted(part)}")
        part_meta = part.get("metadata") or {}
        if ROOT in str(part.get("content")) or "root_session_id" in part_meta or OTHER_ROOT in str(part.get("content")):
            _fail("a part names a conversation")

        heads = _head_lookups(registry, McpClient, ChainWriter, first)
        session_digest = _session_digest_type(client)

        summary = {
            "parts": parts,
            "first": {"writes": first.writes, "reused": first.reused, "lookup": first.lookup},
            "again": {"writes": again.writes, "reused": again.reused, "lookup": again.lookup},
            "other": {"writes": other.writes, "reused": other.reused, "lookup": other.lookup},
            "head": first.head_id,
            "head_lookups": heads,
            "session_digest": session_digest,
        }
        print(json.dumps(summary, sort_keys=True), flush=True)
        return 0
    finally:
        shutdown_mcp_servers()


if __name__ == "__main__":
    raise SystemExit(main())
