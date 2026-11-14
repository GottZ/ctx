"""First review round — Medium/Low findings of the first review §2 in cluster C files (listener, remote blocks,
bundle staging, identity load): M15, M16, M17, M34, L7, L8.
"""

from __future__ import annotations

import asyncio
import hashlib
import time

import pytest

from ctx_handover.engine import jsonutil
from ctx_handover.engine.blocks import BLOCK_SIZE, decode_frames
from ctx_handover.engine.bundle import STATE_FILE, BundleTarget
from ctx_handover.engine.crypto import IDENTITY_ENV, Identity, random_b64, request_string
from ctx_handover.engine.errors import BadRequest, ConflictError
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Right
from ctx_handover.engine.remote import RemoteBlocks
from ctx_handover.engine.server import PeerSignatureAuth, Request, Server
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.ledger_stub import make_ledger
from ctx_handover.tests.unit.C.test_bundle import Exporter, job_ctx
from ctx_handover.tests.unit.C.test_server import Listener, body_of

# ---------------------------------------------------------------------------------------------------------
# M17: a job id of another peer is not available in any role
# ---------------------------------------------------------------------------------------------------------


def test_a_pull_peer_cannot_open_a_source_job_under_another_peers_job_id(tmp_path):
    """★ M17 (the reviewer's probe ``test_probe_cross_role_job_id_reads_other_peers_objects``): peer P pushes to
    this listener (target job X with its objects). Pull peer Q opens a *source* job under the same id X. The objects
    a job may serve are keyed by the job id alone, so Q would read P's transcript. Refused with ``job_conflict``;
    Q reads nothing.

    Red on ``08cfe0f0``: ``POST /jobs`` answered 200 (ownership was checked per role only), ``objects/list`` and
    ``blocks/get`` returned P's manifest and blocks.
    """
    lst = Listener(tmp_path, real_registry=True)
    p = lst.pair(tmp_path, name="pusher", rights=frozenset({Right.PUSH}))
    q = lst.pair(tmp_path, name="puller", rights=frozenset({Right.PULL}))
    secret = b"transcript of the pusher's session " * 100
    m = lst.registry.store.put_bytes(secret, "application/json")
    x = ulid()
    open_t = {"job_id": x, "epoch": 1, "role": "target", "mode": "push", "conductor": {"name": "p"}, "options": {}}
    assert lst.handle(p.req("POST", "jobs", open_t)).status == 200
    put = {"epoch": 1, "manifests": [jsonutil.to_wire(m)]}
    assert lst.handle(p.req("POST", f"jobs/{x}/objects/put", put, epoch=1)).status == 200

    open_s = {"job_id": x, "epoch": 1, "role": "source", "mode": "pull", "conductor": {"name": "q"}, "options": {}}
    resp = lst.handle(q.req("POST", "jobs", open_s))
    assert resp.status == 409 and body_of(resp)["error"]["code"] == "job_conflict", (resp.status, body_of(resp))
    listed = lst.handle(q.req("POST", f"jobs/{x}/objects/list", {"epoch": 1, "object_ids": [m.object_id]}, epoch=1))
    assert listed.status != 200 and not body_of(listed).get("manifests")
    got = lst.handle(q.req("POST", f"jobs/{x}/blocks/get", {"epoch": 1, "ids": list(m.blocks)}, epoch=1))
    frames = [raw for _i, raw, _r in decode_frames(got.body, block_size=BLOCK_SIZE) if raw] if got.status == 200 else []
    assert b"".join(frames) != secret and got.status != 200
    # the pusher itself may re-open its own job (idempotent re-open is unchanged)
    assert lst.handle(p.req("POST", "jobs", open_t)).status == 200


# ---------------------------------------------------------------------------------------------------------
# M16: strict header formats before the nonce cache — with the malformed value covered by a valid signature
# ---------------------------------------------------------------------------------------------------------


def _signed_hello(lst, d, *, nonce: str | None = None, time_text: str | None = None, sig_suffix: str = "") -> Request:
    """A signed ``GET hello`` whose signature is valid for the header values as sent: a malformed nonce is signed
    as it is; a malformed time ``time_text`` reads as the signed integer (``int("+1700000000")``)."""
    ts = int(time.time())
    nonce = nonce if nonce is not None else random_b64(16)
    sha = hashlib.sha256(b"").hexdigest()
    msg = request_string("GET", "/v1/hello", ts, nonce, sha, d.peer_id, lst.identity.instance_id, "")
    headers = {
        "x-ctxh-peer": d.peer_id,
        "x-ctxh-time": time_text if time_text is not None else str(ts),
        "x-ctxh-nonce": nonce,
        "x-ctxh-body-sha256": sha,
        "x-ctxh-sig": d.identity.sign(msg) + sig_suffix,
    }
    return Request(method="GET", path="hello", headers=headers, body=b"", client_ip="10.0.0.9")


def test_malformed_signed_headers_never_reach_the_nonce_cache(tmp_path, monkeypatch):
    """★ M16 / hardening Nr. 2: nonce, time and signature are checked against their strict formats before the
    nonce cache is touched — also when the malformed value is covered by a valid signature (the existing
    ``test_header_formats_strict_before_nonce`` set the bad values after signing, so the signature check alone
    refused them and a loosened ``_NONCE_RE`` stayed green).

    Red proof: mutant 05 (``_NONCE_RE`` accepts 21–23 characters incl. ``=``, ``+``, ``/``) ⇒ ``nonce_seen`` is
    called for the padded and the 23-character nonce; mutant 06 (``_TIME_RE`` accepts a leading ``+``) ⇒ called
    for ``+<now>``.
    """
    lst = Listener(tmp_path)
    d = lst.pair(tmp_path)
    seen: list = []
    real = lst.ledger.nonce_seen
    monkeypatch.setattr(lst.ledger, "nonce_seen", lambda *a, **k: seen.append(a) or real(*a, **k))
    good = random_b64(16)
    malformed = [
        {"nonce": good[:21] + "="},
        {"nonce": good + "A"},
        {"nonce": good[:20] + "+/"},
        {"time_text": "+" + str(int(time.time()))},
        {"sig_suffix": "=="},
    ]
    for kw in malformed:
        resp = lst.handle(_signed_hello(lst, d, **kw))
        assert resp.status == 401, kw
    assert seen == [], seen
    assert lst.handle(_signed_hello(lst, d)).status == 200  # the helper signs validly
    assert len(seen) == 1


# ---------------------------------------------------------------------------------------------------------
# M15: ``inbound`` is read per request when the composition root hands a reader
# ---------------------------------------------------------------------------------------------------------


def test_inbound_off_closes_the_routes_without_a_restart(tmp_path):
    """★ M15: ``plugin.yaml`` says ``inbound: off`` = never; the server used to read it once at construction. With
    ``inbound_setting`` the current value decides every request: ``off`` ⇒ the routes answer like a missing plugin
    at once, ``on`` again ⇒ they answer; an unreadable setting counts as ``off`` (fail closed).

    Red on ``08cfe0f0``: ``Server`` has no ``inbound_setting`` parameter (the value of the constructor held until
    the process restarted). Mutant 07 (the reader is ignored) ⇒ the hello after ``off`` answers 200.
    """
    ledger = make_ledger(tmp_path / "listener" / "ledger.sqlite3")
    identity = new_identity(ledger)
    current: list[object] = ["on"]

    def reader():
        if isinstance(current[0], Exception):
            raise current[0]
        return current[0]

    from ctx_handover.engine.participant import ParticipantRegistry

    server = Server(
        ledger,
        identity,
        ParticipantRegistry(ledger, lambda role, peer: None),
        {"name": "server", "inbound": "on"},
        [PeerSignatureAuth(ledger, identity)],
        inbound_setting=reader,
    )

    def hello() -> int:
        req = Request(method="GET", path="hello", headers={}, body=b"", client_ip="127.0.0.1")
        return asyncio.run(server.handle(req)).status

    assert hello() == 200
    current[0] = "off"
    assert hello() == 404
    current[0] = True  # YAML ``on``
    assert hello() == 200
    current[0] = OSError("config.yaml unreadable")
    assert hello() == 404


# ---------------------------------------------------------------------------------------------------------
# M34: the identity is read from the ``.env`` afresh only when ``get_secret`` does not reproduce it
# ---------------------------------------------------------------------------------------------------------


def test_identity_load_reads_the_env_file_only_when_get_secret_misses(tmp_path):
    """★ M34 (catalog rules 11 and 13): ``Identity.load`` reads the seed through ``get_secret``; the fresh ``.env``
    read (``hermes_cli.config.get_env_value_prefer_dotenv``) is the fallback for a process that started before the
    key existed or was rotated — not part of every load.

    Red on ``08cfe0f0``: ``read_env_fresh`` was called on every load (``crypto.py:126-127``).
    """
    ledger = make_ledger(tmp_path / "ledger.sqlite3")
    env: dict[str, str] = {}
    created = new_identity(ledger, env)
    fresh_reads: list[str] = []

    def fresh(name: str):
        fresh_reads.append(name)
        return env.get(name)

    loaded = Identity.load(ledger, read_secret=env.get, read_env_fresh=fresh)
    assert loaded.public_b64() == created.public_b64()
    assert fresh_reads == []
    # a process whose secret scope predates the key still finds it in the .env
    loaded = Identity.load(ledger, read_secret=lambda name: None, read_env_fresh=fresh)
    assert loaded.public_b64() == created.public_b64() and fresh_reads == [IDENTITY_ENV]


# ---------------------------------------------------------------------------------------------------------
# L7: a manifest page that names a cursor must make progress
# ---------------------------------------------------------------------------------------------------------


class _Pages:
    """A listener's ``objects/list`` answers in order (the last one repeats); more than 20 calls is a loop."""

    def __init__(self, pages) -> None:
        self.pages, self.calls = list(pages), 0

    def call(self, method, route, body=None, *, epoch=None):
        self.calls += 1
        assert self.calls <= 20, "manifests() keeps asking the same listener (no progress check)"
        return self.pages[min(self.calls, len(self.pages)) - 1]


def test_manifest_paging_stops_on_a_page_without_progress(store):
    """★ L7: ``RemoteBlocks.manifests`` follows the listener's ``cursor``; a listener that repeats a page (or sends
    empty pages with a cursor) must end the call, not loop forever. Regular paging still works.

    Red on ``08cfe0f0``: the loop followed the cursor without a progress check (``remote.py:257-269``) — the stand-in
    listener's guard fires after 20 calls.
    """
    a = store.put_bytes(b"first object " * 50, "application/json")
    b = store.put_bytes(b"second object " * 50, "application/json")
    wa, wb = jsonutil.to_wire(a), jsonutil.to_wire(b)
    ok = _Pages([{"manifests": [wa], "cursor": "1"}, {"manifests": [wb]}])
    assert [m.object_id for m in RemoteBlocks(ok, ulid(), 1).manifests([a.object_id, b.object_id])] == [
        a.object_id,
        b.object_id,
    ]
    for pages in ([{"manifests": [wa], "cursor": "1"}], [{"manifests": [], "cursor": "0"}]):
        looping = _Pages(pages)
        with pytest.raises(BadRequest) as info:
            RemoteBlocks(looping, ulid(), 1).manifests([a.object_id, b.object_id])
        assert info.value.details.get("field") == "cursor" and looping.calls <= 2


# ---------------------------------------------------------------------------------------------------------
# L8: an export never deletes a ``<out>.partial`` it did not create
# ---------------------------------------------------------------------------------------------------------


def test_export_keeps_a_partial_directory_it_did_not_create(tmp_path):
    """★ L8: ``BundleTarget.open_job`` cleared ``<out>.partial`` recursively when no export of its own was recorded
    there. A directory of the user under that name (no progress document of this plugin) is refused with
    ``ConflictError`` and left as it was; an empty one is taken over; the plugin's own staging of an ended export
    is still cleared.

    Red on ``08cfe0f0``: ``shutil.rmtree`` deleted ``notes.txt`` and the export opened.
    """
    exp = Exporter(tmp_path)
    out = tmp_path / "x.ctxh.tar"
    partial = tmp_path / "x.ctxh.tar.partial"
    partial.mkdir()
    (partial / "notes.txt").write_text("the user's own file", encoding="utf-8")
    target = BundleTarget(out, exp.identity, as_dir=False, declared_facts=None, ledger=exp.ledger)
    with pytest.raises(ConflictError) as info:
        target.open_job(job_ctx())
    assert "was not left there by an export" in info.value.message
    assert (partial / "notes.txt").read_text(encoding="utf-8") == "the user's own file"

    (partial / "notes.txt").unlink()  # an empty directory holds nothing: taken over
    first = job_ctx()
    BundleTarget(out, exp.identity, as_dir=False, declared_facts=None, ledger=exp.ledger).open_job(first)
    assert (partial / STATE_FILE).is_file()
    # the plugin's own staging of another export that is not running is cleared for a new one
    BundleTarget(out, exp.identity, as_dir=False, declared_facts=None, ledger=exp.ledger).open_job(job_ctx())
