"""First review round, point 4 (review M5):

* the C ledger stub's ``quota_set`` still kept ``MAX(quota, new)`` while the real ledger sets ``MAX(used, new)`` (M5:
  a smaller re-negotiated plan lowers the quota) — a layer-1 run (``CTXH_C_LEDGER=stub``) tested other rules than
  production;
* ``Server._sink`` fell back to the target's plain block store when the target offered no quota-bound sink
  (``blocks_for``): fail-open for the R28 quota. Production never took that branch (``LocalTarget.blocks_for``); only
  the test double lacked the method. Now the double has it and the server refuses instead
  (``capability_missing``)."""

from __future__ import annotations

from ctx_handover.engine import jsonutil
from ctx_handover.engine.blocks import MEDIA_BLOCKS, encode_frame
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Role
from ctx_handover.tests.fakes.blocks_fake import MemoryBlockStore
from ctx_handover.tests.unit.C.helpers import FakeParticipant
from ctx_handover.tests.unit.C.ledger_stub import LedgerStub
from ctx_handover.tests.unit.C.test_server import Listener, _open, body_of


def _quota_story(ledger) -> list:
    job = ulid()
    out: list = []
    ledger.quota_set(job, Role.TARGET, 1000)
    out.append(ledger.quota_get(job, Role.TARGET))
    out.append(ledger.quota_charge(job, Role.TARGET, 300))
    ledger.quota_set(job, Role.TARGET, 500)  # a smaller plan, negotiated last
    out.append(ledger.quota_get(job, Role.TARGET))
    ledger.quota_set(job, Role.TARGET, 100)  # below what is already stored
    out.append(ledger.quota_get(job, Role.TARGET))
    out.append(ledger.quota_charge(job, Role.TARGET, 1))
    ledger.quota_set(job, Role.TARGET, 2000)
    out.append(ledger.quota_get(job, Role.TARGET))
    return out


def test_rv_final_ledger_stub_quota_follows_the_real_ledger(tmp_path):
    """★ The stub's quota rules equal ``engine.ledger.Ledger``'s on one story: set, charge, lower, lower below
    ``used``, charge over, raise.

    Red before the fix: the stub kept 1000 after the smaller plan (``(1000, 300)`` against ``(500, 300)``)."""
    real = Ledger(tmp_path / "real.sqlite3")
    try:
        want = _quota_story(real)
    finally:
        real.close()
    assert want[2] == (500, 300) and want[3] == (300, 300)  # the real rule, M5
    assert _quota_story(LedgerStub(tmp_path / "stub.sqlite3")) == want


class NoQuotaSinkTarget(FakeParticipant):
    """A target that offers no quota-bound sink (no ``blocks_for``)."""

    blocks_for = None


def test_rv_final_a_target_without_a_quota_sink_stores_nothing(tmp_path):
    """★ M5 (C part): without ``blocks_for`` ``PUT blocks`` (#25) and ``objects/put`` (#23) are refused with
    ``capability_missing`` and nothing lands in the store — no unbounded fallback to ``api.blocks()``.

    Red before the fix: ``assert 200 == 400`` — the request was stored through the plain store."""
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    lst.registry.target = NoQuotaSinkTarget(lst.registry, "target")
    src = MemoryBlockStore()
    m = src.put_bytes(b"payload  " * 200, "application/octet-stream")
    job = _open(lst, d)
    frames = b"".join(encode_frame(b, src.get(b), peer_caps=frozenset({"enc.zlib"})) for b in m.blocks)
    resp = lst.handle(d.req("PUT", f"jobs/{job}/blocks", raw=frames, ctype=MEDIA_BLOCKS, epoch=1))
    assert resp.status == 400, resp.body
    err = body_of(resp)["error"]
    assert err["code"] == "capability_missing" and err["details"]["capability"] == "blocks_for", err
    assert lst.registry.store.missing(list(m.blocks)) == list(m.blocks)
    body = {"epoch": 1, "manifests": [jsonutil.to_wire(m)]}
    resp = lst.handle(d.req("POST", f"jobs/{job}/objects/put", body, epoch=1))
    assert resp.status == 400 and body_of(resp)["error"]["code"] == "capability_missing", resp.body
