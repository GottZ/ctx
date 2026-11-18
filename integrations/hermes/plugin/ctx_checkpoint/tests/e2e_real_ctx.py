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
names no conversation (GottZ/ctx#344).

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
ROOT = "20261116_120000_e2e290"
OTHER_ROOT = "20261118_120000_e2e344"


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

        summary = {
            "parts": parts,
            "first": {"writes": first.writes, "reused": first.reused, "lookup": first.lookup},
            "again": {"writes": again.writes, "reused": again.reused, "lookup": again.lookup},
            "other": {"writes": other.writes, "reused": other.reused, "lookup": other.lookup},
            "head": first.head_id,
        }
        print(json.dumps(summary, sort_keys=True), flush=True)
        return 0
    finally:
        shutdown_mcp_servers()


if __name__ == "__main__":
    raise SystemExit(main())
