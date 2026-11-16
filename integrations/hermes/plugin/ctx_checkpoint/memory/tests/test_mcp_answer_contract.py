"""The ctx MCP answers this plugin parses, from the fixture the ctx server's own test pins.

``mcp_answers.json`` (next to this file) holds the exact answer texts of the MCP ``store``
tool and the keys of the ``get``/``search`` answers. The ctx side drives its real handlers
with the fixture's write and compares their answers with the file
(``go/internal/handler/mcp_checkpoint_answers_integration_test.go``); this suite hands the
same answers to the plugin, wrapped the way Hermes wraps an MCP text result
(``{"result": "<text>"}``). So a changed answer on the server and a fixture edited to suit
the parser both turn one side red (GottZ/ctx#290).
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from ctx_plugin.checkpoint import ChainWriter, _ChainRun
from ctx_plugin.client import store_action, store_block_id
from ctx_plugin.mcp_client import McpClient

FIXTURE = json.loads((Path(__file__).resolve().parent / "mcp_answers.json").read_text(encoding="utf-8"))
WRITE = FIXTURE["write"]
BLOCK_ID = FIXTURE["get"]["sample_id"]


def _render(template: str) -> str:
    return template.format(title=WRITE["title"], id=BLOCK_ID, category=WRITE["category"])


def _hermes_result(text: str) -> str:
    return json.dumps({"result": text}, ensure_ascii=False)


def _get_answer() -> str:
    """A get answer with exactly the fixture's keys (Go: ``json.MarshalIndent(block, "", "  ")``)."""
    values = {
        "id": BLOCK_ID,
        "title": WRITE["title"],
        "category": WRITE["category"],
        "content": WRITE["changed_content"],
        "metadata": WRITE["metadata"],
        "type": WRITE["type"],
    }
    assert set(values) == set(FIXTURE["get"]["keys"]), "the sample answer must carry exactly the pinned keys"
    return json.dumps(values, indent=2, ensure_ascii=False)


def _search_answer() -> str:
    row = {"id": BLOCK_ID, "title": WRITE["title"], "category": WRITE["category"]}
    assert set(row) == set(FIXTURE["search"]["row_keys"]), "the sample row must carry exactly the pinned keys"
    return json.dumps([row], indent=2, ensure_ascii=False)


def _client(answers: dict[str, str]) -> tuple[McpClient, list]:
    calls: list = []

    def dispatch(name, args):
        calls.append((name, args))
        return _hermes_result(answers[name.rsplit("__", 1)[-1]])

    return McpClient(dispatch), calls


def _store(client: McpClient) -> dict:
    return client.store(
        WRITE["category"],
        WRITE["title"],
        WRITE["content"],
        sensitivity=WRITE["sensitivity"],
        tags=WRITE["tags"],
        type=WRITE["type"],
        metadata=WRITE["metadata"],
        timeout=5.0,
    )


def test_the_fixture_title_carries_a_uuid_ahead_of_the_id_group():
    """The case the id group exists for: a UUID-shaped root session id in the title."""
    assert WRITE["metadata"]["root_session_id"] in WRITE["title"]
    assert WRITE["metadata"]["root_session_id"] != BLOCK_ID


def test_a_stored_answer_yields_the_block_id_of_the_id_group():
    client, calls = _client({"store": _render(FIXTURE["store"]["stored"])})
    data = _store(client)
    assert data["success"] is True
    assert store_block_id(data) == BLOCK_ID
    assert store_action(data) != "noop"
    assert calls[0][0] == "mcp__ctx__store" and calls[0][1]["type"] == WRITE["type"]


def test_a_no_change_answer_is_a_noop_on_the_existing_block():
    """The retry of a chain reuses the parts already stored by this answer (0.10.0)."""
    client, _calls = _client({"store": _render(FIXTURE["store"]["no_change"])})
    data = _store(client)
    assert data["success"] is True
    assert store_action(data) == "noop"
    assert store_block_id(data) == BLOCK_ID


@pytest.mark.parametrize("form", ["stored", "no_change"])
def test_the_chain_writer_counts_each_store_answer_once(form):
    client, _calls = _client({"store": _render(FIXTURE["store"][form])})
    writer = ChainWriter(client=client, category=WRITE["category"])
    run = _ChainRun(deadline=writer.monotonic() + 30.0, budget_s=30.0, window_writes=writer._window_writes())
    block_id = writer._store(
        "head", run, title=WRITE["title"], content=WRITE["content"], metadata=WRITE["metadata"], tags=WRITE["tags"]
    )
    assert block_id == BLOCK_ID
    assert (run.writes, run.reused) == ((1, 0) if form == "stored" else (0, 1))


def test_the_head_lookup_reads_search_and_get_answers():
    """search finds the head by its title, get yields manifest, fact hashes and checkpoint id."""
    client, calls = _client({"search": _search_answer(), "get": _get_answer()})
    writer = ChainWriter(client=client, category=WRITE["category"])
    run = _ChainRun(deadline=writer.monotonic() + 30.0, budget_s=30.0, window_writes=writer._window_writes())
    manifest_id, fact_hashes, checkpoint_id = writer.load_previous_head(WRITE["title"], run)
    meta = WRITE["metadata"]
    assert (manifest_id, fact_hashes, checkpoint_id) == (
        meta["latest_manifest_id"],
        meta["fact_hashes"],
        meta["checkpoint_id"],
    )
    search_args = calls[0][1]
    assert calls[0][0] == "mcp__ctx__search"
    assert search_args["category"] == WRITE["category"]
    assert search_args["tags"] == FIXTURE["search"]["tags"]
    assert search_args["query"] == FIXTURE["search"]["query"]
    assert calls[1] == ("mcp__ctx__get", {"id": BLOCK_ID})
