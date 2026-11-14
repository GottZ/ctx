"""R6-B: the ctx record of a handover links to the checkpoint head under the title ctx_checkpoint really files it
under (review of PR #190, §3 "the link from a handover record to the checkpoint head uses the raw root id, while
ctx_checkpoint shortens ids over 48 chars").

ctx_checkpoint (since 0.6.1) titles a head ``Compaction checkpoint head <token>``: the root id itself when it is made
of ``[A-Za-z0-9_.-]`` and at most 48 characters, else the sanitized id cut to 39 characters plus ``-`` and the first
8 hex digits of the full id's SHA-256. Hermes' ``local-<sha256>`` ids (cli/tui sessions on a gateway's store, 70
characters) are such ids. The vectors below are ctx_checkpoint's own golden vectors
(``ctx_checkpoint/memory/tests/test_p3_checkpoint_chain.py`` ``HEAD_TITLE_GOLDEN``)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import ctxrecords
from ctx_handover.tests.fakes.surfaces_fake import FakeCtxServer, FakeOutboxLedger
from ctx_handover.tests.unit.F.test_ctxrecords import _data

LOCAL_ROOT = "local-" + "0123456789abcdef" * 4
HEAD_TITLE_GOLDEN = {
    "20261031_120000_abcdef": "20261031_120000_abcdef",
    "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c": "3f1c2a9e-8b7d-4c6e-9a1b-2d3e4f5a6b7c",
    "root/session-a": "root_session-a-a0af5788",
    "team/alice:1": "team_alice_1-8e207def",
    "team:alice/1": "team_alice_1-7054b256",
    "x" * 60: "x" * 39 + "-42f2d973",
    "über-sitzung": "_ber-sitzung-a42b9cb4",
    LOCAL_ROOT: "local-0123456789abcdef0123456789abcdef0-1d986fd2",
}


@pytest.mark.parametrize(("root", "token"), sorted(HEAD_TITLE_GOLDEN.items()))
def test_record_links_the_head_under_ctx_checkpoints_title(tmp_path, root, token):
    """★ The record of a root with any id links to the head ctx_checkpoint stored for it.

    Red proof (base ``cab3a492``): the search asks for ``Compaction checkpoint head <raw id>``; for the six ids that
    ctx_checkpoint shortens no head is found and the record carries no ``checkpoint_head``."""
    ledger = FakeOutboxLedger(tmp_path / "l.sqlite3")
    ctxrecords.enqueue(ledger, "departure", _data(source_root=root, source_session=root), redact=lambda s: s)
    server = FakeCtxServer(heads={f"Compaction checkpoint head {token}": "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"})
    assert ctxrecords.flush(ledger, server.call_mcp, "ctx", "handover", now=5.0) == 1
    (store,) = server.stores
    assert store["arguments"]["metadata"].get("checkpoint_head") == "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
    assert server.searches[0]["arguments"]["query"] == f"Compaction checkpoint head {token}"
