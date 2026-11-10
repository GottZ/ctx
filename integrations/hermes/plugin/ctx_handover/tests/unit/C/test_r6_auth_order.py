"""R6-B: the listener checks a peer request's signature before it looks at the peer's rights or reads the body
(review of PR #190, §3 "the rights check and body read run before the signature check").

The signature covers the body only through the ``X-Ctxh-Body-Sha256`` header, so it can be verified before a byte of
the body is read; the body is then read under its cap and compared with that header, and the nonce is spent last.
Before the fix a request that merely named a paired device's id learned from 403-vs-401 which rights that device
holds, and the listener read the whole body (up to 8 MiB) of a request whose signature was never valid."""

from __future__ import annotations

from ctx_handover.engine.blocks import MEDIA_BLOCKS
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Right
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.ledger_stub import make_ledger
from ctx_handover.tests.unit.C.test_server import Dialer, Listener, body_of, run


def _forger(lst: Listener, tmp_path, victim: Dialer) -> Dialer:
    """Signs with a key of its own under the victim's peer id."""
    return Dialer(new_identity(make_ledger(tmp_path / "forger" / "l.sqlite3")), victim.peer_id, victim.listener_id)


def test_rights_of_a_peer_are_not_revealed_before_the_signature(tmp_path):
    """★ A forged request under a push-only device's id to a pull route gets the same 401 as any forged request —
    not the 403 that tells the forger this device lacks ``pull``.

    Red proof (base ``cab3a492``): ``handle_stream`` compares the route right with the rights of the peer named in
    the header before ``finish`` verifies the signature: 403."""
    lst = Listener(tmp_path)
    push_only = lst.pair(tmp_path, "pusher", rights={Right.PUSH})
    forged = _forger(lst, tmp_path, push_only)
    for route, body in (("sessions/list", {"limit": 1}), ("offers/list", {})):
        resp = lst.handle(forged.req("POST", route, body))
        assert resp.status == 401, (route, resp.status)
        assert body_of(resp)["error"]["code"] == "unauthorized"
    # the honest device still gets its 403 for a right it does not hold
    assert lst.handle(push_only.req("POST", "sessions/list", {"limit": 1})).status == 403


def test_body_of_a_forged_request_is_not_read(tmp_path):
    """★ A request whose signature is wrong is refused before its body is read (a forged 8 MiB upload costs the
    listener nothing).

    Red proof (base ``cab3a492``): ``_read_capped`` consumes all 8 chunks before ``finish`` refuses."""
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    forged = _forger(lst, tmp_path, d)
    consumed = 0

    async def chunks():
        nonlocal consumed
        for _ in range(8):
            consumed += 1
            yield b"\0" * (64 * 1024)

    req = forged.req("PUT", f"jobs/{ulid()}/blocks", raw=b"", ctype=MEDIA_BLOCKS, epoch=1)
    resp = run(lst.server.handle_stream(req.method, req.path, req.headers, chunks(), req.client_ip))
    assert resp.status == 401
    assert consumed == 0


def test_valid_signature_with_another_body_is_still_refused(tmp_path):
    """The body is still bound: a correctly signed header set with a different body ⇒ 401, and the nonce of that
    request is not spent (the honest request with the same nonce still goes through)."""
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    good = d.req("POST", "sessions/list", {"limit": 2})
    tampered = type(good)(good.method, good.path, good.headers, b'{"limit": 3}', good.client_ip)
    assert lst.handle(tampered).status == 401
    assert lst.handle(good).status == 200
