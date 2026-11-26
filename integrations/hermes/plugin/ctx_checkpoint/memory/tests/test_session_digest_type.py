"""GottZ/ctx#289 point 4: session digests and facts are stored as block type ``session-digest``.

ctx (migration 196) marks the type untrusted and damps it in recall, so every read surface frames
the transcript text of a digest or a fact. A ctx server without the type refuses such a write before
it stores or books anything — REST 422 ``unknown_type``, over MCP the same text as a tool error
(``mcp_answers.json``, section ``session_digest``). The plugin then sends the write once more without
the type and names it on no later write of the process to that server entry: at most one refused
write per process and entry. These tests run the REST transport against doubles of both servers.
"""

from __future__ import annotations

import json
import threading
from pathlib import Path
from typing import ClassVar

import pytest
from conftest import FakeClient, join_writes

import ctx_plugin.provider as provider_mod
from ctx_plugin import checkpoint as _cp
from ctx_plugin.client import CtxApiError
from ctx_plugin.config import CATEGORY_LEARNINGS

_FIXTURE = json.loads((Path(__file__).resolve().parent / "mcp_answers.json").read_text(encoding="utf-8"))[
    "session_digest"
]
SESSION_DIGEST_TYPE = _FIXTURE["type"]  # config.SESSION_DIGEST_TYPE, pinned in the first test
FACTS = [
    "The proxy on INFRA-WWW terminates TLS for every service with Traefik 3.2 since 2026-10-01.",
    "Backups of the gitea database run nightly at 03:15 to the NAS share /volume1/backup/gitea.",
    "The staging cluster uses PostgreSQL 18.6 with pgvector 0.8.7 on host db-stage-02.",
]


def _fact_turns(facts=FACTS) -> list[dict]:
    # An assistant turn counts as a fact when it closes a turn (config.extract_salient_facts).
    return [
        m for fact in facts for m in ({"role": "user", "content": "And then?"}, {"role": "assistant", "content": fact})
    ]


def _digest_turns(n: int = 4, tag: str = "a") -> list[dict]:
    out = []
    for i in range(n):
        out.append({"role": "user", "content": f"Question {tag}{i}: what is the state of service {tag}{i}?"})
        out.append({"role": "assistant", "content": f"Service {tag}{i} runs on port {8000 + i}, health check green."})
    return out


class OldServer(FakeClient):
    """A ctx server without migration 196: a store naming a type other than ``checkpoint`` is
    refused as ctx's REST client raises it (HTTP 422, code ``unknown_type``); nothing is stored."""

    refused: ClassVar[list] = []

    def store(self, category, title, content, sensitivity=None, tags=None, **extra):
        block_type = extra.get("type")
        if block_type and block_type != "checkpoint":
            OldServer.refused.append({"category": category, "title": title, "type": block_type})
            text = _FIXTURE["unknown_type"].format(type=block_type)
            raise CtxApiError(f"HTTP 422 [{_FIXTURE['rest_code']}]: {text}", status=422, code=_FIXTURE["rest_code"])
        return super().store(category, title, content, sensitivity, tags, **extra)


@pytest.fixture
def old_server(monkeypatch, fake_client_cls):
    OldServer.refused = []
    monkeypatch.setattr(provider_mod, "CtxClient", OldServer)
    return OldServer


def _typed(calls: list, category: str | None = None) -> list:
    return [c.get("type") for c in calls if category is None or c["category"] == category]


def test_digests_and_facts_name_the_type_and_the_memory_mirror_does_not(make_provider):
    from ctx_plugin import config

    assert getattr(config, "SESSION_DIGEST_TYPE", None) == SESSION_DIGEST_TYPE
    p, client = make_provider()
    p.on_pre_compress(_fact_turns())  # best effort: the chain, then the facts in the background
    join_writes(p)
    p.on_session_end(_digest_turns())
    join_writes(p)
    p.on_memory_write("add", "memory", "The user prefers short answers in German.")
    join_writes(p)

    facts = [c for c in client.calls if c["category"] == CATEGORY_LEARNINGS]
    digests = [c for c in client.calls if str(c["title"]).startswith("Session digest ")]
    mirror = [c for c in client.calls if str(c["title"]).startswith("memory: ")]
    assert [c["content"] for c in facts] == FACTS
    assert {c.get("type") for c in facts} == {SESSION_DIGEST_TYPE}
    assert len(digests) == 1 and digests[0].get("type") == SESSION_DIGEST_TYPE
    assert len(mirror) == 1 and "type" not in mirror[0]  # curated memory keeps the server's classification
    p.shutdown()


def test_the_required_facts_name_the_type(make_provider):
    p, client = make_provider()
    receipt = p.on_pre_compress(_fact_turns(), require_checkpoint=True)
    facts = [c for c in client.calls if c["category"] == CATEGORY_LEARNINGS]
    assert f"stored {len(FACTS)} fact block(s)" in receipt
    assert _typed(facts) == [SESSION_DIGEST_TYPE] * len(FACTS)
    p.shutdown()


def test_an_older_server_refuses_one_write_and_every_checkpoint_completes(make_provider, old_server):
    """The required checkpoint that passes today passes against a server without the type: the one
    refused fact is sent again without it within the checkpoint, and no later write of the process to
    that entry names the type — another binding of the same entry included."""
    p, client = make_provider("prof-a")
    receipt = p.on_pre_compress(_fact_turns(), require_checkpoint=True)
    assert f"stored {len(FACTS)} fact block(s)" in receipt, receipt
    assert "not stored" not in receipt
    assert len(old_server.refused) == 1 and old_server.refused[0]["category"] == CATEGORY_LEARNINGS
    facts = [c for c in client.calls if c["category"] == CATEGORY_LEARNINGS]
    assert [c["content"] for c in facts] == FACTS
    assert _typed(facts) == [None] * len(FACTS)

    entry = _cp.entry_state(p._entry_key())
    assert entry.errors_in_row == 0  # the write after the refusal ended the run

    p.on_session_end(_digest_turns())
    join_writes(p)
    second, client2 = make_provider("prof-b")  # another profile on the same base URL and key
    assert second._entry_key() == p._entry_key()
    second.on_pre_compress(_fact_turns(["A fact of the second profile that is long enough to be kept as one."]))
    join_writes(second)
    assert len(old_server.refused) == 1, old_server.refused
    digests = [c for c in client.calls if str(c["title"]).startswith("Session digest ")]
    assert len(digests) == 1 and "type" not in digests[0]
    assert _typed(client2.calls, CATEGORY_LEARNINGS) == [None]
    second.shutdown()
    p.shutdown()


def test_a_digest_refused_for_its_type_is_stored_without_it(make_provider, old_server):
    p, client = make_provider()
    p.on_session_end(_digest_turns())
    join_writes(p)
    assert [r["title"][:15] for r in old_server.refused] == ["Session digest "]
    digests = [c for c in client.calls if str(c["title"]).startswith("Session digest ")]
    assert len(digests) == 1 and "type" not in digests[0]
    # The digest was stored: its exchanges are not counted again for the next trigger.
    p.on_session_end(_digest_turns())
    join_writes(p)
    assert len([c for c in client.calls if str(c["title"]).startswith("Session digest ")]) == 1
    p.shutdown()


def test_another_error_of_a_typed_write_is_no_verdict_on_the_type(make_provider):
    """A failure that is no refusal of the type (a connection error) is not taken for one: the fact
    is not sent again without the type, and the next write names the type again."""
    p, client = make_provider(config={"checkpoint_chain": False})
    client.fail_on = {0}  # the first fact store raises a connection error
    p.on_pre_compress(_fact_turns(FACTS[:2]), require_checkpoint=False)
    join_writes(p)
    facts = [c for c in client.calls if c["category"] == CATEGORY_LEARNINGS]
    assert [c["content"] for c in facts] == FACTS[:2]
    assert _typed(facts) == [SESSION_DIGEST_TYPE, SESSION_DIGEST_TYPE]
    p.shutdown()


def test_one_write_asks_and_the_others_wait_for_its_verdict():
    """EntryState: the first write of a type asks; while it is out, the others wait for its answer (up to
    ``wait_s``) and go without the type when it does not come; a refusal holds for the process, an answer
    makes the type known, no verdict lets the next write ask."""
    name = SESSION_DIGEST_TYPE
    entry = _cp.EntryState()
    assert entry.type_to_send(name) == _cp.TYPE_ASK
    assert entry.type_to_send(name) == ""  # one asks at a time; no wait
    entry.type_answered(name, None)
    assert entry.type_to_send(name) == _cp.TYPE_ASK
    entry.type_answered(name, True)
    assert [entry.type_to_send(name) for _ in range(3)] == ["known"] * 3
    entry.type_answered(name, None)  # an error of a write that named a known type
    assert entry.type_to_send(name) == "known"

    other = _cp.EntryState()
    assert other.type_to_send(name) == _cp.TYPE_ASK
    other.type_answered(name, False)
    assert [other.type_to_send(name) for _ in range(3)] == ["", "", ""]
    other.type_answered(name, None)
    assert other.type_to_send(name) == ""

    waiting = _cp.EntryState()
    assert waiting.type_to_send(name) == _cp.TYPE_ASK
    got: list = []
    waiter = threading.Thread(target=lambda: got.append(waiting.type_to_send(name, wait_s=10.0)))
    waiter.start()
    waiter.join(0.2)
    assert waiter.is_alive() and got == []  # it waits for the answer
    waiting.type_answered(name, True)
    waiter.join(5)
    assert got == ["known"]


def _side_by_side(knows_the_type: bool) -> tuple[list, list, _cp.EntryState]:
    entry = _cp.EntryState()
    refused: list = []
    stored: list = []
    start = threading.Barrier(8)
    lock = threading.Lock()

    def store(**kwargs):
        if kwargs.get("type") and not knows_the_type:
            with lock:
                refused.append(kwargs)
            raise CtxApiError("HTTP 422 [unknown_type]: refused", status=422, code="unknown_type")
        with lock:
            stored.append(kwargs)
        return {"success": True, "block": {"id": "x"}}

    def writer(i: int) -> None:
        start.wait()
        _cp.store_typed(entry, SESSION_DIGEST_TYPE, store, wait_s=10.0, title=f"fact {i}")

    threads = [threading.Thread(target=writer, args=(i,)) for i in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(15)
    return refused, stored, entry


def test_writes_side_by_side_send_the_type_once_to_a_server_that_refuses_it():
    """Eight writers at once against a server without the type: one refused write in all."""
    refused, stored, entry = _side_by_side(knows_the_type=False)
    assert len(refused) == 1 and len(stored) == 8
    assert all("type" not in s for s in stored)
    assert entry.type_to_send(SESSION_DIGEST_TYPE) == ""


def test_writes_side_by_side_all_name_the_type_on_a_server_that_knows_it():
    """Eight writers at once against a server with the type: the seven that come while the first asks
    wait for its answer and name the type too — none lands without it (a fact would be untyped
    knowledge, unframed)."""
    refused, stored, entry = _side_by_side(knows_the_type=True)
    assert refused == [] and len(stored) == 8
    assert [s.get("type") for s in stored] == [SESSION_DIGEST_TYPE] * 8
    assert entry.type_to_send(SESSION_DIGEST_TYPE) == "known"
