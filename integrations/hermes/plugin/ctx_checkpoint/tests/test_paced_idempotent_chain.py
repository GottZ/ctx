"""Plugin 0.10.0 (GottZ/ctx#287): a chain longer than one rate window is paced instead of
refused, a retry meets its own titles, and a refused write waits for the window.

The double below stands for one ctx server behind Hermes' MCP dispatch, with the parts of
both that the chain writer's timing depends on:

- ctx: the write limit per key over a sliding 60 s window (``go/internal/handler/
  stage_gates.go`` checks ``count >= query.rate_limit_write`` over ``context_access_log``,
  ``go/internal/store/blocks.go`` ``CheckRateLimitByAction``); a store whose title, content,
  metadata, tags, sensitivity and type equal the stored block's is answered "No change
  needed" BEFORE that check and books no write (``go/internal/handler/mcp.go``,
  ``IdenticalContentMatch``); upsert by (category, title).
- Hermes: one lock per MCP server, held for the duration of one call
  (``tools/mcp_tool_handlers.py`` ``async with server._rpc_lock``), and the per-server circuit
  breaker that opens after three consecutive error answers (``tools/mcp_tool.py``
  ``_CIRCUIT_BREAKER_THRESHOLD``; an error payload of the tool counts, ``_record_call_outcome``).

Time is a fake clock: the plugin's sleep and monotonic seams move it, every call costs 50 ms.
"""

from __future__ import annotations

import hashlib
import importlib.util
import json
import sys
import threading
from pathlib import Path

import pytest

_PROVIDER_PATH = Path(__file__).resolve().parents[1] / "__init__.py"
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_paced_chain", _PROVIDER_PATH, submodule_search_locations=[str(_PROVIDER_PATH.parent)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
CtxCheckpointMemoryProvider = _MODULE.CtxCheckpointMemoryProvider
CtxCheckpointError = _MODULE.CtxCheckpointError

HOST_BREAKER_THRESHOLD = 3  # tools/mcp_tool.py _CIRCUIT_BREAKER_THRESHOLD
CALL_LATENCY_S = 0.05
# The store answers as ctx words them: the fixture ctx's own test pins its handlers against
# (go/internal/handler/mcp_checkpoint_answers_integration_test.go, GottZ/ctx#290).
_STORE_ANSWERS = json.loads(
    (Path(__file__).resolve().parents[1] / "memory" / "tests" / "mcp_answers.json").read_text(encoding="utf-8")
)["store"]


class Clock:
    def __init__(self) -> None:
        self.now = 1_000.0
        self.slept: list[float] = []


class CtxServerDouble:
    """One ctx MCP server as Hermes dispatches to it (see the module docstring)."""

    def __init__(self, clock: Clock, *, write_limit: int = 100, window_s: float = 60.0) -> None:
        self.clock = clock
        self.write_limit = write_limit
        self.window_s = window_s
        self.rpc_lock = threading.Lock()  # Hermes: server._rpc_lock, one call at a time
        self.blocks: dict[tuple[str, str], dict] = {}
        self.booked: list[float] = []  # context_access_log 'write' rows of the key
        self.counter = 0
        self.refused = 0
        self.noops = 0
        self.consecutive_errors = 0
        self.max_consecutive_errors = 0
        self.calls: list[tuple[str, dict]] = []

    # -- Hermes side ---------------------------------------------------------------------

    def dispatch(self, name: str, args: dict, **_kwargs) -> str:
        with self.rpc_lock:
            self.clock.now += CALL_LATENCY_S
            tool = name.rsplit("__", 1)[-1]
            self.calls.append((tool, args))
            answer = getattr(self, f"_{tool}")(args)
        if "error" in answer:
            self.consecutive_errors += 1
            self.max_consecutive_errors = max(self.max_consecutive_errors, self.consecutive_errors)
            return json.dumps({"error": answer["error"]})
        self.consecutive_errors = 0
        return json.dumps({"result": answer["result"]})

    # -- ctx side ------------------------------------------------------------------------

    def _store(self, args: dict) -> dict:
        key = (args["category"], args["title"])
        incoming = {
            "content": args["content"],
            "tags": list(args.get("tags") or []),
            "sensitivity": args.get("sensitivity"),
            "type": args.get("type"),
        }
        metadata = dict(args.get("metadata") or {})
        stored = self.blocks.get(key)
        if (
            stored is not None
            and all(stored[k] == v for k, v in incoming.items())
            and all(stored["metadata"].get(k) == v for k, v in metadata.items())
        ):
            self.noops += 1
            return {"result": _STORE_ANSWERS["no_change"].format(title=args["title"], id=stored["id"], category=key[0])}
        self.booked = [t for t in self.booked if t > self.clock.now - self.window_s]
        if len(self.booked) >= self.write_limit:
            self.refused += 1
            return {"error": f"Rate limit exceeded: max {self.write_limit} writes per 60 seconds"}
        self.booked.append(self.clock.now)
        if stored is None:
            self.counter += 1
            stored = {"id": f"019f7000-0000-7000-8000-{self.counter:012d}"}
            self.blocks[key] = stored
        stored.update(incoming, metadata=metadata, title=args["title"], category=args["category"])
        return {
            "result": _STORE_ANSWERS["stored"].format(title=args["title"], id=stored["id"], category=args["category"])
        }

    def _search(self, args: dict) -> dict:
        tags = set(args.get("tags") or [])
        rows = [
            {"id": b["id"], "title": b["title"], "category": b["category"], "type": "checkpoint", "untrusted": True}
            for b in self.blocks.values()
            if args["query"] in b["title"] and tags <= set(b["tags"])
        ]
        return {"result": json.dumps(rows[: int(args.get("limit") or 10)])}

    def _get(self, args: dict) -> dict:
        for b in self.blocks.values():
            if b["id"] == args["id"]:
                return {"result": json.dumps({k: b[k] for k in ("id", "title", "category", "content", "metadata")})}
        return {"error": f"block not found: {args['id']}"}

    # -- what the tests read -------------------------------------------------------------

    def titled(self, needle: str) -> list[dict]:
        return [b for b in self.blocks.values() if needle in b["title"]]

    def parts(self) -> list[dict]:
        return [b for b in self.blocks.values() if " part " in b["title"]]

    def manifests(self) -> list[dict]:
        return [b for b in self.blocks.values() if " manifest" in b["title"]]

    def head(self) -> dict:
        (head,) = self.titled("Compaction checkpoint head")
        return head


@pytest.fixture
def clock(monkeypatch):
    clock = Clock()
    monkeypatch.setattr(_MODULE, "_monotonic", lambda: clock.now)
    return clock


def _sleep_outside_the_lock(monkeypatch, clock: Clock, server: CtxServerDouble) -> None:
    def sleep(seconds: float) -> None:
        # A wait between two calls must leave the server's lock free for every other caller
        # of the profile (recall, the agent's ctx tools, other sessions' checkpoints).
        assert not server.rpc_lock.locked(), "the chain writer slept while holding the server's RPC lock"
        clock.slept.append(seconds)
        clock.now += seconds

    monkeypatch.setattr(_MODULE, "_sleep", sleep)


def _provider(server: CtxServerDouble, session: str = "session-paced", **config) -> tuple:
    notices: list[str] = []
    provider = CtxCheckpointMemoryProvider(config=dict(config), dispatch=server.dispatch)
    provider.initialize(session, platform="cli", warning_callback=notices.append)
    return provider, notices


def _transcript(n_messages: int, size: int = 900, tag: str = "m") -> list[dict]:
    return [
        {"role": "user" if i % 2 == 0 else "assistant", "content": f"{tag}{i:04d} " + "w" * size}
        for i in range(n_messages)
    ]


def _manifest_of_head(server: CtxServerDouble) -> dict:
    manifest_id = server.head()["metadata"]["latest_manifest_id"]
    (manifest,) = [b for b in server.manifests() if b["id"] == manifest_id]
    return manifest


def test_chain_longer_than_one_rate_window_is_paced_and_completes(monkeypatch, clock):
    """Above max_parts the chain used to be refused before its first write — under
    compression.checkpoint_required every later compaction of the session failed the same way."""
    server = CtxServerDouble(clock, write_limit=6)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, notices = _provider(server, chunk_chars=1_000, max_parts=3)  # 5 writes per window
    frame = provider.on_pre_compress(_transcript(12), require_checkpoint=True)

    assert "ctx checkpoint head:" in frame
    manifest = _manifest_of_head(server)
    assert len(manifest["metadata"]["source_block_ids"]) == 12 == len(server.parts())
    # Paced: never more than max_parts + 2 writes inside one window, so ctx never refused one.
    assert server.refused == 0
    assert len(clock.slept) == 2 and all(60.0 < s <= 62.0 for s in clock.slept), clock.slept
    # The user learns before the first wait that the compaction waits for a paced checkpoint.
    assert notices and "14 writes" in notices[0] and "minute" in notices[0], notices


def test_a_paced_chain_fits_the_budget_it_derives_from_its_part_count(monkeypatch, clock):
    """budget_s covers one rate window; every further window the part count needs is added."""
    server = CtxServerDouble(clock, write_limit=6)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, chunk_chars=1_000, max_parts=3, budget_s=5)
    provider.on_pre_compress(_transcript(12), require_checkpoint=True)
    assert len(server.parts()) == 12 and clock.slept


def test_a_retry_meets_its_own_titles_and_reuses_the_parts_ctx_confirms(monkeypatch, clock):
    """A failed attempt used to leave its parts behind under titles no later attempt met (the
    checkpoint id hashed the time): every retry wrote the whole transcript again."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, chunk_chars=1_000)
    messages = _transcript(10)
    real_store = server._store

    server._store = _failing_at_part(server, 6)
    with pytest.raises(CtxCheckpointError):
        provider.on_pre_compress(messages, require_checkpoint=True)
    assert len(server.parts()) == 5 and not server.manifests()

    server._store = real_store
    booked_before = len(server.booked)
    provider.on_pre_compress(messages, require_checkpoint=True)

    # The five stored parts were confirmed by ctx (same title, identical content), not written again.
    assert server.noops == 5
    assert len(server.booked) - booked_before == 5 + 2  # parts 6-10, manifest, head
    referenced = set(_manifest_of_head(server)["metadata"]["source_block_ids"])
    assert {b["id"] for b in server.parts()} == referenced, "orphaned parts left behind"
    assert provider._last_chain_receipt.reused == 5


def test_a_repeated_identical_checkpoint_writes_no_part_and_never_points_at_itself(monkeypatch, clock):
    """The host can compact the same transcript again after a checkpoint (the summary failed,
    a required fact store failed): the manifest is re-written in place under its own parent."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, chunk_chars=1_000)
    provider.on_pre_compress(_transcript(4, tag="first"), require_checkpoint=True)
    parent_id = server.head()["metadata"]["latest_manifest_id"]
    messages = _transcript(6, tag="second")
    provider.on_pre_compress(messages, require_checkpoint=True)
    manifest_id = server.head()["metadata"]["latest_manifest_id"]
    parts_before = {b["id"] for b in server.parts()}

    provider.on_pre_compress(messages, require_checkpoint=True)

    assert {b["id"] for b in server.parts()} == parts_before
    assert server.head()["metadata"]["latest_manifest_id"] == manifest_id
    manifest = _manifest_of_head(server)
    assert manifest["metadata"]["parent_manifest_id"] == parent_id != manifest_id
    assert len(server.manifests()) == 2


def _published_wording(server: CtxServerDouble) -> dict[str, str]:
    """Content of every part the head's manifest references, by block id."""
    ids = _manifest_of_head(server)["metadata"]["source_block_ids"]
    by_id = {b["id"]: b["content"] for b in server.parts()}
    return {block_id: by_id[block_id] for block_id in ids}


def _failing_at_part(server: CtxServerDouble, n: int):
    """The store of the double, failing the n-th part write it receives (a part title names no
    position since 0.15.0, so the count of part stores stands for the index)."""
    real_store = server._store
    seen = 0

    def store(args):
        nonlocal seen
        if args["title"].startswith("Compaction source part "):
            seen += 1
            if seen == n:
                return {"error": "store failed: connection reset by peer"}
        return real_store(args)

    return store


def test_a_retry_with_another_split_never_rewrites_a_published_part(monkeypatch, clock):
    """Same transcript, same part count, cut elsewhere (chunk_chars 1000 → 1100): a part's
    title must name its exact content, or the retry overwrites a published part with another
    cut and a failure after it leaves the published manifest on a mix of both splits. Since
    0.15.0 the title is the digest of the whole write (``part_title_for``)."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, chunk_chars=1_000)
    messages = [{"role": "user", "content": "s" * 2_500}]
    provider.on_pre_compress(messages, require_checkpoint=True)
    published = _published_wording(server)
    assert len(published) == 3

    provider._chain().chunk_chars = 1_100  # still three parts
    server._store = _failing_at_part(server, 2)
    with pytest.raises(CtxCheckpointError):
        provider.on_pre_compress(messages, require_checkpoint=True)

    assert _published_wording(server) == published


def test_a_retry_after_a_session_switch_never_rewrites_a_published_part(monkeypatch, clock):
    """Same root and transcript, another active session. Up to 0.14.0 the part header named the
    session, so the content differed under the same root, digest, index and count; since 0.15.0
    a part names no session and the retry's parts are the stored ones. Either way a failure in
    between leaves the published wording as it was."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, chunk_chars=1_000)
    messages = _transcript(3)
    provider.on_pre_compress(messages, require_checkpoint=True)
    published = _published_wording(server)

    provider.on_session_switch("session-paced-2", reason="compression")
    assert provider._checkpoint_root() == "session-paced"
    server._store = _failing_at_part(server, 2)
    with pytest.raises(CtxCheckpointError):
        provider.on_pre_compress(messages, require_checkpoint=True)

    assert _published_wording(server) == published


def test_repeating_the_first_checkpoint_of_a_root_keeps_one_manifest(monkeypatch, clock):
    """The head names this transcript and its manifest has no parent: the manifest is written
    again in place, not doubled with the process's cached manifest id as parent."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server)
    messages = _transcript(2, tag="only")
    provider.on_pre_compress(messages, require_checkpoint=True)
    manifest_id = server.head()["metadata"]["latest_manifest_id"]
    provider.on_pre_compress(messages, require_checkpoint=True)
    assert [b["id"] for b in server.manifests()] == [manifest_id]
    assert server.manifests()[0]["metadata"]["parent_manifest_id"] is None


def test_a_transcript_that_returns_after_another_checkpoint_never_closes_a_cycle(monkeypatch, clock):
    """T1, T2, then T1 again (an undo after two failed compactions): T1's manifest keeps its
    parent; the repeat gets a manifest of its own, so no walk along parent_manifest_id loops."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server)
    t1 = _transcript(2, tag="one")
    provider.on_pre_compress(t1, require_checkpoint=True)
    m1 = server.head()["metadata"]["latest_manifest_id"]
    provider.on_pre_compress(t1 + _transcript(2, tag="two"), require_checkpoint=True)
    m2 = server.head()["metadata"]["latest_manifest_id"]
    provider.on_pre_compress(t1, require_checkpoint=True)

    by_id = {b["id"]: b for b in server.manifests()}
    assert by_id[m1]["metadata"]["parent_manifest_id"] is None
    assert by_id[m2]["metadata"]["parent_manifest_id"] == m1
    m3 = server.head()["metadata"]["latest_manifest_id"]
    assert m3 not in (m1, m2) and by_id[m3]["metadata"]["parent_manifest_id"] == m2
    seen, cursor = set(), m3
    while cursor:
        assert cursor not in seen, "parent_manifest_id loops"
        seen.add(cursor)
        cursor = by_id[cursor]["metadata"].get("parent_manifest_id")


def test_a_refused_write_waits_for_the_window_and_never_trips_the_host_breaker(monkeypatch, clock):
    """Two sessions over one key: the second chain is refused once the window is full. The
    backoff used to end after 2+4+8 s against a 60 s window, whatever the budget, after four
    refusals in a row — one more than Hermes' circuit breaker allows before it pauses every
    ctx call of the profile for a minute."""
    server = CtxServerDouble(clock, write_limit=10)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    first, _ = _provider(server, "session-one", chunk_chars=1_000)
    # Default budget (60 s): the slot frees a whole window after the other chain's writes, so
    # the first refusal of a chain grants it that window on top.
    second, _ = _provider(server, "session-two", chunk_chars=1_000)
    first.on_pre_compress(_transcript(6, tag="one"), require_checkpoint=True)  # 8 writes
    second.on_pre_compress(_transcript(6, tag="two"), require_checkpoint=True)  # refused at its 3rd

    assert server.refused == 1
    assert server.max_consecutive_errors < HOST_BREAKER_THRESHOLD
    assert len(server.titled("Compaction checkpoint head")) == 2
    assert all(len(_manifests_of(server, root)) == 1 for root in ("session-one", "session-two"))


def _manifests_of(server: CtxServerDouble, root: str) -> list[dict]:
    return [b for b in server.manifests() if b["metadata"]["root_session_id"] == root]


def test_a_window_that_stays_full_fails_after_two_refusals(monkeypatch, clock):
    """Other writers keep the key's window full: the chain gives up after the second refusal
    in a row (the third would open the host's breaker), with the parts it stored kept for the
    next attempt."""
    server = CtxServerDouble(clock, write_limit=3)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, notices = _provider(server, chunk_chars=1_000, budget_s=600)
    real_store = server._store

    def busy_key(args):
        server.booked = [clock.now] * server.write_limit  # foreign writes, always fresh
        return real_store(args)

    server._store = busy_key
    with pytest.raises(CtxCheckpointError, match="rate-limited"):
        provider.on_pre_compress(_transcript(6), require_checkpoint=True)
    assert server.refused == 2
    assert server.max_consecutive_errors == 2
    assert clock.slept and all(s <= 62.0 for s in clock.slept)
    assert any("rate-limited" in n or "write limit" in n for n in notices), notices


# -- GottZ/ctx#344: a part is stored once per text ------------------------------------------
#
# Measured on the reference instance (2026-11-18): 44 % of all checkpoint characters were exact
# duplicates of a part ctx already held, 34 % of them across conversations (one burst wrote 23
# roots with the same 34-part text). Every part named its root, active session, transcript digest
# and position in its header and title, so no copy could ever meet the stored one.


def _listed(server: CtxServerDouble, root: str, generation: int = 0) -> list[str]:
    return _manifests_of(server, root)[generation]["metadata"]["source_block_ids"]


def test_a_second_conversation_with_the_same_text_lists_the_parts_already_stored(monkeypatch, clock):
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    first, _ = _provider(server, session="root-a", chunk_chars=1_000)
    second, _ = _provider(server, session="root-b", chunk_chars=1_000)
    messages = _transcript(8)

    first.on_pre_compress(messages, require_checkpoint=True)
    booked = len(server.booked)
    second.on_pre_compress(messages, require_checkpoint=True)

    assert len(_listed(server, "root-a")) == 8
    assert _listed(server, "root-b") == _listed(server, "root-a")
    assert len(server.parts()) == 8
    # ctx answered every part "No change needed": root-b booked its manifest and its head only.
    assert server.noops == 8 and len(server.booked) - booked == 2
    assert second._last_chain_receipt.reused == 8 and second._last_chain_receipt.writes == 2
    # Each conversation keeps its own manifest and head.
    assert len(server.titled("Compaction checkpoint head")) == 2


def test_a_branch_lists_the_stored_parts_of_the_turns_it_carries_over(monkeypatch, clock):
    """A branch is a root of its own and carries its parent's turns (memory/digest.py
    resolve_lineage_root): its first checkpoint used to store all of them again."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    parent, _ = _provider(server, session="root-parent", chunk_chars=1_000)
    branch, _ = _provider(server, session="root-branch", chunk_chars=1_000)
    carried = _transcript(6)

    parent.on_pre_compress(carried, require_checkpoint=True)
    branch.on_pre_compress(carried + _transcript(3, tag="b"), require_checkpoint=True)

    parent_parts, branch_parts = _listed(server, "root-parent"), _listed(server, "root-branch")
    # One part per turn. The parent's last part ends its transcript and keeps the final newline;
    # in the branch that turn is followed by another and the cut trims it (split_chunks), so the
    # branch shares the five parts before it.
    assert len(parent_parts) == 6 and len(branch_parts) == 9
    assert branch_parts[:5] == parent_parts[:5]
    assert not set(branch_parts[5:]) & set(parent_parts)
    assert len(server.parts()) == 6 + 4


def test_the_next_compaction_lists_the_stored_parts_of_the_turns_it_keeps(monkeypatch, clock):
    """Hermes hands the whole live transcript to every compaction (agent/conversation_compression.py
    _pre_compress_memory_context): the protected first turns come again under the same ordinals."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, session="root-gen", chunk_chars=1_000)
    kept = _transcript(3, tag="h")

    provider.on_pre_compress(kept + _transcript(5, tag="x"), require_checkpoint=True)
    provider.on_pre_compress(kept + _transcript(4, tag="y"), require_checkpoint=True)

    first, second = _listed(server, "root-gen", 0), _listed(server, "root-gen", 1)
    assert second[:3] == first[:3]
    assert not set(second[3:]) & set(first)
    assert provider._last_chain_receipt.reused == 3


def test_a_part_is_shared_only_with_a_write_of_the_same_payload(monkeypatch, clock):
    """A store under a part title must never rewrite a part another manifest lists: the title
    names the whole write (here the sensitivity differs; tags, metadata and category count the
    same way), so another payload makes another block and the stored one stays as it was."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    plain, _ = _provider(server, session="root-s", chunk_chars=1_000)
    strict, _ = _provider(server, session="root-s", chunk_chars=1_000, sensitivity="credentials")
    messages = _transcript(4)

    plain.on_pre_compress(messages, require_checkpoint=True)
    before = {b["id"]: json.dumps(b, sort_keys=True) for b in server.parts()}
    strict.on_pre_compress(messages, require_checkpoint=True)

    assert len(before) == 4 and len(server.parts()) == 8
    for block in server.parts():
        if block["id"] in before:
            assert json.dumps(block, sort_keys=True) == before[block["id"]], "a listed part was rewritten"
        else:
            assert block["sensitivity"] == "credentials"


def test_a_part_names_nothing_of_its_conversation_and_its_title_is_the_digest_of_its_write(monkeypatch, clock):
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, session="root-names", chunk_chars=1_000)
    provider.on_pre_compress(_transcript(3), require_checkpoint=True)

    (manifest,) = server.manifests()
    digest = manifest["metadata"]["sha256"]
    part_writes = [
        args for tool, args in server.calls if tool == "store" and args["title"].startswith("Compaction source part ")
    ]
    assert len(part_writes) == 3
    for args in part_writes:
        assert (
            "root-names" not in args["content"] and digest not in args["content"] and "- Part:" not in args["content"]
        )
        assert "## Direct transcript\n\n### Message " in args["content"]
        for key in ("root_session_id", "active_session_id", "sha256", "part", "parts", "evidence_date"):
            assert key not in args["metadata"], key
        assert sorted(args["metadata"]["ctx_plugin"]) == ["profile", "redaction", "writer"]
        assert args["metadata"]["platform"] == "cli" and "part_sha256" in args["metadata"]
        payload = {key: args[key] for key in ("category", "content", "metadata", "sensitivity", "tags", "type")}
        canonical = json.dumps(payload, sort_keys=True, ensure_ascii=False, separators=(",", ":"))
        assert args["title"] == "Compaction source part " + hashlib.sha256(canonical.encode("utf-8")).hexdigest()[:32]
    # The manifest names the conversation and the parts in their order.
    meta = manifest["metadata"]
    assert meta["root_session_id"] == "root-names" and meta["active_session_id"] == "root-names"
    assert meta["source_block_ids"] == [
        server.blocks[("compaction-checkpoints", a["title"])]["id"] for a in part_writes
    ]


def test_text_that_differs_only_in_a_masked_secret_is_one_part(monkeypatch, clock):
    """The title is taken over the text as stored, after redaction: two conversations that differ
    only in a credential the redaction masks hold the same part, and the value is in neither."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    first, _ = _provider(server, session="root-x", chunk_chars=1_000)
    second, _ = _provider(server, session="root-y", chunk_chars=1_000)

    def turns(secret: str) -> list[dict]:
        return [{"role": "user", "content": f"the deploy password is {secret} for the staging box " + "w" * 800}]

    first.on_pre_compress(turns("Hunter2Hunter2x9"), require_checkpoint=True)
    second.on_pre_compress(turns("Zebra77Zebra77q1"), require_checkpoint=True)

    assert _listed(server, "root-x") == _listed(server, "root-y")
    (part,) = server.parts()
    for value in ("Hunter2Hunter2x9", "Zebra77Zebra77q1"):
        assert value not in part["content"] and value not in part["title"]


def test_a_plugin_update_keeps_the_stored_parts(monkeypatch, clock):
    """The writer's version is the manifest's: a part is shared only for byte-identical text, so
    its version adds nothing to what it holds, and in the part's write it would end the sharing
    at every release for every conversation that carries its turns on."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, session="root-up", chunk_chars=1_000)
    kept = _transcript(3, tag="k")
    provider.on_pre_compress(kept + _transcript(2, tag="a"), require_checkpoint=True)

    monkeypatch.setattr(_MODULE._cp, "PLUGIN_VERSION", "99.0.0")
    provider.on_pre_compress(kept + _transcript(2, tag="b"), require_checkpoint=True)

    first, second = _manifests_of(server, "root-up")
    assert second["metadata"]["ctx_plugin"]["version"] == "99.0.0"
    assert second["metadata"]["source_block_ids"][:3] == first["metadata"]["source_block_ids"][:3]


def test_a_manifest_lists_a_repeated_part_once_per_position(monkeypatch, clock):
    """A transcript that holds the same window twice (hard cuts inside one long message) lists the
    same part at both positions; the listed parts in order give back the whole transcript."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, session="root-rep", chunk_chars=1_000)
    messages = [{"role": "user", "content": "z" * 4_000}]
    provider.on_pre_compress(messages, require_checkpoint=True)

    ids = _listed(server, "root-rep")
    by_id = {b["id"]: b["content"].split("## Direct transcript\n\n", 1)[1] for b in server.parts()}
    assert len(ids) > len(set(ids)) >= 2
    assert "".join(by_id[i] for i in ids) == _MODULE._cp.render_direct_evidence(messages)


def test_a_part_is_stored_with_the_tags_its_title_names(monkeypatch, clock):
    """The writer accepts any tags callable: the tags are resolved once per chain and the part is
    stored with exactly the tags its title digest covers."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, session="root-tags", chunk_chars=1_000)
    writer = provider._chain()
    calls = iter(range(1_000))
    resolve = writer.tags_for
    writer.tags_for = lambda tags: [*resolve(tags), f"call:{next(calls)}"]

    provider.on_pre_compress(_transcript(3), require_checkpoint=True)

    part_writes = [a for t, a in server.calls if t == "store" and a["title"].startswith("Compaction source part ")]
    assert len(part_writes) == 3
    for args in part_writes:
        payload = {key: args[key] for key in ("category", "content", "metadata", "sensitivity", "tags", "type")}
        canonical = json.dumps(payload, sort_keys=True, ensure_ascii=False, separators=(",", ":"))
        assert args["title"] == "Compaction source part " + hashlib.sha256(canonical.encode("utf-8")).hexdigest()[:32]


def test_a_failed_head_write_after_a_retry_leaves_the_head_on_a_complete_list_of_its_text(monkeypatch, clock):
    """A retry of the checkpoint the head names rewrites that manifest in place (0.10.0) — here with
    another split, as a 0.15.0 retry of a 0.14.0 checkpoint does with the new part format. Every
    part it lists is stored before the manifest is written, so when the head write fails after it,
    the head still names a manifest whose parts exist and give back the same transcript."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, session="root-retry", chunk_chars=1_000)
    messages = [{"role": "user", "content": "s" * 2_500}]
    provider.on_pre_compress(messages, require_checkpoint=True)
    manifest_id = server.head()["metadata"]["latest_manifest_id"]
    listed_before = _manifest_of_head(server)["metadata"]["source_block_ids"]

    provider._chain().chunk_chars = 1_100  # another split of the same transcript
    real_store = server._store

    def head_fails(args):
        if args["title"].startswith("Compaction checkpoint head"):
            return {"error": "store failed: connection reset by peer"}
        return real_store(args)

    server._store = head_fails
    with pytest.raises(CtxCheckpointError):
        provider.on_pre_compress(messages, require_checkpoint=True)

    assert server.head()["metadata"]["latest_manifest_id"] == manifest_id
    manifest = _manifest_of_head(server)
    listed = manifest["metadata"]["source_block_ids"]
    assert listed != listed_before  # the manifest the head names was rewritten in place
    by_id = {b["id"]: b["content"] for b in server.parts()}
    assert all(block_id in by_id for block_id in listed)
    rendered = _MODULE._cp.render_direct_evidence(messages)
    assert "".join(by_id[i].split("## Direct transcript\n\n", 1)[1] for i in listed) == rendered
    assert manifest["metadata"]["sha256"] == hashlib.sha256(rendered.encode("utf-8")).hexdigest()
    # The parts of the first split stay stored (no part is ever rewritten or removed).
    assert all(block_id in by_id for block_id in listed_before)


# -- GottZ/ctx#299: the host's wait for the compaction, and windows a retry does not use -------
#
# Hermes calls on_pre_compress in the compaction worker before the summary call; its wait for the
# worker counts from the last progress, which only the summary stream reports (300 s by default,
# agent/conversation_compression.py). A chain still writing at the end of that window was
# abandoned and the compaction ran again, so two chains of one root wrote at the same time.


def _host_waits(monkeypatch, seconds):
    # raising=False: on a plugin without the reader the tests run against its behaviour
    monkeypatch.setattr(_MODULE, "_host_compaction_wait_s", lambda: seconds, raising=False)


def test_a_paced_chain_stops_before_the_host_stops_waiting_and_the_next_attempt_continues(monkeypatch, clock):
    """Three windows of writes (61 s apart) do not fit a host that waits 100 s: the chain stops
    before the window it cannot finish, inside the host's wait, with the parts it stored kept,
    and the user learns why; the next attempt reuses them and completes."""
    server = CtxServerDouble(clock, write_limit=6)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    _host_waits(monkeypatch, 100.0)
    provider, notices = _provider(server, chunk_chars=1_000, max_parts=3)  # 5 writes per window
    messages = _transcript(12)  # 12 parts + manifest + head = 14 writes = 3 windows
    started = clock.now

    with pytest.raises(CtxCheckpointError, match="Hermes waits 100s") as failure:
        provider.on_pre_compress(messages, require_checkpoint=True)

    assert clock.now <= started + 100.0 - _MODULE._cp.HOST_WAIT_MARGIN_S, clock.now - started
    assert "before source part 11/12" in str(failure.value)
    assert len(server.parts()) == 10 and not server.manifests()
    assert notices and "Hermes waits 100s" in notices[0], notices
    # The failure notice names the host's setting instead of budget_s.
    assert "compression.context_timeout_seconds" in notices[-1] and "budget_s" not in notices[-1], notices

    _host_waits(monkeypatch, None)  # e.g. Hermes' retry in a window of its own
    provider.on_pre_compress(messages, require_checkpoint=True)
    assert len(_manifest_of_head(server)["metadata"]["source_block_ids"]) == 12 == len(server.parts())
    assert provider._last_chain_receipt.reused == 10


def test_a_budget_beyond_the_host_wait_ends_with_the_host_wait(monkeypatch, clock):
    """budget_s (up to 600 s) is no licence to run past the host's wait: the chain's deadline,
    and the window a refusal grants, end where the host stops waiting."""
    server = CtxServerDouble(clock, write_limit=3)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    _host_waits(monkeypatch, 40.0)
    provider, _ = _provider(server, chunk_chars=1_000, budget_s=600)
    started = clock.now
    # The key's window holds 3 writes; the 4th is refused, and the window the refusal grants
    # (61 s) ends at the host's wait: one more try there, then the chain gives up.
    with pytest.raises(CtxCheckpointError, match="rate-limited"):
        provider.on_pre_compress(_transcript(6), require_checkpoint=True)  # 8 writes
    assert clock.now <= started + 40.0 - _MODULE._cp.HOST_WAIT_MARGIN_S, clock.now - started
    assert server.max_consecutive_errors < HOST_BREAKER_THRESHOLD


def test_a_host_without_the_wait_leaves_the_chain_as_it_was(monkeypatch, clock):
    """No window to read (an older Hermes, or the wait switched off): the deadline grows by one
    window per window the writes enter, as before."""
    server = CtxServerDouble(clock, write_limit=6)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    _host_waits(monkeypatch, None)
    provider, _ = _provider(server, chunk_chars=1_000, max_parts=3, budget_s=5)
    provider.on_pre_compress(_transcript(12), require_checkpoint=True)
    assert len(server.parts()) == 12 and provider._last_chain_receipt.windows == 3


def test_a_retry_whose_parts_ctx_confirms_enters_no_window_and_sends_no_notice(monkeypatch, clock):
    """The receipt said windows=3 (and the user was told the compaction waits minutes) although
    ctx confirmed every part and nothing was paced: the windows were planned from the part count."""
    server = CtxServerDouble(clock, write_limit=6)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, notices = _provider(server, chunk_chars=1_000, max_parts=3, budget_s=60)
    messages = _transcript(12)
    provider.on_pre_compress(messages, require_checkpoint=True)
    assert provider._last_chain_receipt.windows == 3 and len(notices) == 1

    clock.now += 120.0  # the key's window is empty again
    slept_before = len(clock.slept)
    provider.on_pre_compress(messages, require_checkpoint=True)
    receipt = provider._last_chain_receipt
    assert receipt.reused == 12 and receipt.writes == 2  # manifest in place, head
    assert receipt.windows == 1
    assert len(notices) == 1, notices
    assert len(clock.slept) == slept_before


def test_a_retry_walks_its_confirmed_parts_within_the_planned_windows(monkeypatch, clock):
    """A part ctx confirms books no write but costs a call: the deadline keeps every window the
    part count plans, so a retry (or the next compaction of a grown transcript) that walks many
    confirmed parts on a slow server is not cut at the one-window budget."""
    server = CtxServerDouble(clock, write_limit=6)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    real_dispatch = server.dispatch

    def slow(name, args, **kwargs):
        clock.now += 0.25  # a slow server: 250 ms per call on top of the double's 50 ms
        return real_dispatch(name, args, **kwargs)

    server.dispatch = slow
    _host_waits(monkeypatch, None)  # the plan alone (six windows) bounds this chain
    provider, _ = _provider(server, chunk_chars=1_000, max_parts=3, budget_s=5)  # 5 writes per window
    messages = _transcript(24)
    provider.on_pre_compress(messages, require_checkpoint=True)

    clock.now += 120.0
    provider.on_pre_compress(messages + _transcript(2, tag="more"), require_checkpoint=True)
    receipt = provider._last_chain_receipt
    assert receipt.reused >= 23, receipt  # about 7 s of confirmed calls against a 5 s budget
    assert len(_manifest_of_head(server)["metadata"]["source_block_ids"]) == len(receipt.part_ids)


def test_the_first_notice_names_the_host_wait_when_the_plan_does_not_fit(monkeypatch, clock):
    """The plan (six windows, 365 s) does not fit a host that waits 200 s, although the second
    window still does: the notice before it says that this attempt saves what fits."""
    server = CtxServerDouble(clock, write_limit=6)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    _host_waits(monkeypatch, 200.0)
    provider, notices = _provider(server, chunk_chars=1_000, max_parts=3)
    with pytest.raises(CtxCheckpointError, match="Hermes waits 200s"):
        provider.on_pre_compress(_transcript(24), require_checkpoint=True)  # 26 writes, 5 per window
    assert "Hermes waits 200s" in notices[0], notices


def test_the_host_wait_is_read_from_the_host_function(monkeypatch):
    """The reader takes the first value of the host's own resolver and falls back to no bound
    for a host without it, a switched-off wait and a failing resolver."""
    import types

    reader = _MODULE._cp.host_compaction_wait_s
    cases = {
        "ctx_cp_t299_present": (lambda: (250.0, 600.0), 250.0),
        "ctx_cp_t299_off": (lambda: (0.0, 600.0), None),
        "ctx_cp_t299_negative": (lambda: (-1.0, 600.0), None),
        "ctx_cp_t299_raises": (lambda: (_ for _ in ()).throw(RuntimeError("config")), None),
    }
    for name, (resolver, want) in cases.items():
        module = types.ModuleType(name)
        module.resolve_context_compression_timeouts = resolver
        monkeypatch.setitem(sys.modules, name, module)
        assert reader(name) == want, name
    monkeypatch.setitem(sys.modules, "ctx_cp_t299_absent", types.ModuleType("ctx_cp_t299_absent"))
    assert reader("ctx_cp_t299_absent") is None
    # The pinned Hermes has the function under the name the plugin reads, in the module it probes.
    from agent import conversation_compression

    host_idle, _ceiling = conversation_compression.resolve_context_compression_timeouts()
    assert reader() == host_idle > 0


def test_a_chain_slower_than_the_host_wait_names_the_host_wait(monkeypatch, clock):
    """budget_s beyond the host's wait and a chain slower than that wait (62 writes at 50 ms
    against a wait that leaves 2 s): the failure names the host's wait, not a budget the chain
    never had."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    _host_waits(monkeypatch, 12.0)
    provider, notices = _provider(server, chunk_chars=1_000, budget_s=600)
    started = clock.now
    with pytest.raises(CtxCheckpointError, match="Hermes waits 12s"):
        provider.on_pre_compress(_transcript(60), require_checkpoint=True)
    assert clock.now <= started + 12.0 - _MODULE._cp.HOST_WAIT_MARGIN_S + CALL_LATENCY_S
    assert server.parts() and "budget_s" not in notices[-1], notices


# -- GottZ/ctx#289: a head whose tags were replaced outside the plugin -----------------------


def _strip_head_tags(server: CtxServerDouble, *, keep_metadata: bool) -> str:
    """An update or store outside the plugin replaced the head's tags (and, ``keep_metadata=False``,
    its metadata: an update replaces the whole object)."""
    head = server.head()
    head["tags"] = ["edited-by-hand"]
    if not keep_metadata:
        head["metadata"] = {"note": "edited"}
    return head["id"]


def test_a_head_without_its_tag_still_continues_the_chain(monkeypatch, clock):
    """The head search names the tag, so a head that lost it was not found: the next checkpoint
    started a new chain without a parent and replaced the head, silently."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, notices = _provider(server, chunk_chars=1_000)
    provider.on_pre_compress(_transcript(4, tag="first"), require_checkpoint=True)
    first_manifest = server.head()["metadata"]["latest_manifest_id"]
    head_id = _strip_head_tags(server, keep_metadata=True)
    provider._last_manifest_by_root.clear()  # another process: no cached manifest to fall back on

    provider.on_pre_compress(_transcript(6, tag="second"), require_checkpoint=True)

    receipt = provider._last_chain_receipt
    assert receipt.lookup == "untagged"
    assert receipt.parent_manifest_id == first_manifest
    assert _manifest_of_head(server)["metadata"]["parent_manifest_id"] == first_manifest
    head = server.head()
    assert head["id"] == head_id and "checkpoint-head" in head["tags"]
    assert not [n for n in notices if "head" in n], notices  # nothing lost, nothing to tell


def test_a_head_without_its_tag_and_its_manifest_is_replaced_with_a_notice(monkeypatch, clock):
    """Tags and metadata replaced: the block under the head title names no manifest; the new
    head is published over it and the user is told (as for a tagged head without one)."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, notices = _provider(server, chunk_chars=1_000)
    provider.on_pre_compress(_transcript(4, tag="first"), require_checkpoint=True)
    head_id = _strip_head_tags(server, keep_metadata=False)
    provider._last_manifest_by_root.clear()

    provider.on_pre_compress(_transcript(6, tag="second"), require_checkpoint=True)

    assert provider._last_chain_receipt.lookup == "head-invalid"
    assert provider._last_chain_receipt.parent_manifest_id is None
    assert any(head_id in n and "latest_manifest_id" in n for n in notices), notices
    assert "checkpoint-head" in server.head()["tags"]


def test_the_untagged_search_runs_only_when_the_tagged_one_finds_nothing(monkeypatch, clock):
    """Cost: one more search for the first checkpoint of a root, none while the head carries its tag."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    provider, _ = _provider(server, chunk_chars=1_000)

    def searches() -> list[dict]:
        return [args for tool, args in server.calls if tool == "search"]

    provider.on_pre_compress(_transcript(4, tag="first"), require_checkpoint=True)
    assert [bool(s.get("tags")) for s in searches()] == [True, False]  # a new root
    server.calls.clear()
    provider.on_pre_compress(_transcript(6, tag="second"), require_checkpoint=True)
    assert [bool(s.get("tags")) for s in searches()] == [True]


def test_required_facts_after_the_chain_end_within_the_host_wait(monkeypatch, clock):
    """Memory features on: the required facts after a stored chain shared its deadline, which a
    budget_s beyond the host's wait (600 s) put past it. They end where the host stops waiting:
    a fact that would start later is left for a later compaction, the checkpoint stands (#299;
    the chain stored, a late fact is no failure since 0.12.0)."""
    server = CtxServerDouble(clock)
    _sleep_outside_the_lock(monkeypatch, clock, server)
    _host_waits(monkeypatch, 40.0)
    real_store = server._store

    def slow_facts(args):
        if args["category"] == "learnings":
            clock.now += 12.0  # a fact store that takes 12 s
        return real_store(args)

    server._store = slow_facts
    provider, _ = _provider(server, chunk_chars=1_000, budget_s=600, memory=True)
    facts = [
        "The proxy on INFRA-WWW terminates TLS for every service with Traefik 3.2 since 2026-10-01.",
        "Backups of the gitea database run nightly at 03:15 to the NAS share /volume1/backup/gitea.",
        "The staging cluster uses PostgreSQL 18.6 with pgvector 0.8.7 on host db-stage-02.",
        "Release tags are annotated and signed with the SSH key of the build host since v5.0.0.",
    ]
    # An assistant turn counts as a fact when it closes a turn (config.extract_salient_facts).
    messages = [
        m for fact in facts for m in ({"role": "user", "content": "And then?"}, {"role": "assistant", "content": fact})
    ]
    started = clock.now

    receipt = provider.on_pre_compress(messages, require_checkpoint=True)

    stored = [b for b in server.blocks.values() if b["category"] == "learnings"]
    assert "ctx checkpoint head:" in receipt
    assert 0 < len(stored) < len(facts), (len(stored), receipt)
    assert "not stored" in receipt, receipt
    # The last fact store started before the host's wait ended (less the margin).
    assert clock.now - 12.0 <= started + 40.0 - _MODULE._cp.HOST_WAIT_MARGIN_S, clock.now - started
