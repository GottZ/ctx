"""Round G′, cluster C — Erfolgs-Sweep: an answer of a peer counts as success
only with an explicitly positive value. A missing key, a wrong type or an unsigned status signal is no success.

Covered here (each red on ``c31da4c3``): ``RemoteBlocks.missing`` (``blocks/need`` without ``missing`` read as "the
target has every block"), ``RemoteSource.heartbeat`` (an answer without ``state`` read as "the source is alive"),
``RemoteTarget.continue_session`` (``bool("false")``), ``WireClient.put_blocks`` (an answer without ``stored`` /
``rejected`` read as "nothing rejected") and ``WireClient.probe_gate`` (an unsigned 404/429 on a probe read as
"the body passed every proxy"). The wire tests run against the real ``Server`` in process (``_l2r_helpers.Pair``);
only the answers under test are replaced, signed with the listener's key where the case needs a signed answer.
"""

from __future__ import annotations

import hashlib
import json

import httpx
import pytest

from ctx_handover.engine.blocks import encode_frame
from ctx_handover.engine.crypto import H_RESP_SIG, response_string
from ctx_handover.engine.errors import BadResponseSignature, HandoverError, InvalidState
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import GateAuth, GateMode, Mode, Peer
from ctx_handover.engine.remote import RemoteBlocks, RemoteSource, RemoteTarget
from ctx_handover.engine.wire import MIB, PROBE_SIZES_MIB, WireClient
from ctx_handover.tests.unit.C._l2r_helpers import ALL_RIGHTS, Pair, job_context


class _Wire:
    """A ``WireClient`` stand-in: opening a job succeeds, every other call answers ``answer``."""

    def __init__(self, answer: dict) -> None:
        self.answer = answer
        self.peer_caps = frozenset()

    def call(self, method, route, body=None, *, epoch=None):
        if route == "jobs":
            return {"job_id": body["job_id"], "state": "open", "epoch": 1}
        return self.answer

    def conductor_info(self) -> dict:
        return {}


@pytest.mark.parametrize("answer", [{}, {"missing": None}, {"missing": "ab"}, {"missing": [1]}, {"ids": []}])
def test_blocks_need_without_a_list_of_missing_ids_is_no_answer(answer):
    """★ ``blocks/need`` decides which blocks are sent; an answer without a list of ids must not mean "the target
    has them all" (the conductor's final ``missing`` check would pass with nothing transferred).

    Red proof: on ``c31da4c3`` ``.get("missing", [])`` ⇒ ``[]``. Mutant 06 (default restored).
    """
    blocks = RemoteBlocks(_Wire(answer), ulid(), 1)
    with pytest.raises(BadResponseSignature):
        blocks.missing(["a" * 64])


def test_blocks_need_with_a_list_is_read():
    assert RemoteBlocks(_Wire({"missing": ["b" * 64]}), ulid(), 1).missing(["a" * 64, "b" * 64]) == ["b" * 64]
    assert RemoteBlocks(_Wire({"missing": []}), ulid(), 1).missing(["a" * 64]) == []


@pytest.mark.parametrize("answer", [{}, {"state": None}, {"state": 3}])
def test_a_keepalive_answer_without_a_state_is_no_keepalive(answer):
    """★ Route #9 answers ``{state}`` (DESIGN §4.7, participant registry); an answer without a state is not "the
    source holds its leases".

    Red proof: on ``c31da4c3`` ``heartbeat`` returned None (success). Mutant 07 (missing state = alive).
    """
    with pytest.raises(HandoverError) as info:
        RemoteSource(_Wire(answer)).heartbeat(job_context(mode=Mode.PULL))
    assert isinstance(info.value, BadResponseSignature), info.value


def test_a_keepalive_with_a_live_state_returns():
    assert RemoteSource(_Wire({"state": "locked"})).heartbeat(job_context(mode=Mode.PULL)) is None


@pytest.mark.parametrize(
    ("answer", "started"),
    [({"started": True}, True), ({"started": "false"}, False), ({"started": 1}, False), ({}, False)],
)
def test_continue_started_only_when_the_peer_says_true(answer, started):
    """★ The completion message says "continued on <peer>" from ``ContinueResult.started``; only JSON ``true``
    counts.

    Red proof: on ``c31da4c3`` ``bool(data.get("started"))`` ⇒ ``"false"`` and ``1`` were started. Mutant 08.
    """
    result = RemoteTarget(_Wire(answer)).continue_session(job_context(mode=Mode.PUSH), "go on")
    assert result.started is started


@pytest.mark.parametrize("state", ["aborted", "expired"])
def test_target_abort_returns_when_the_peer_reports_it_aborted(state):
    assert RemoteTarget(_Wire({"state": state})).abort(job_context(mode=Mode.PUSH), "x") is None


@pytest.mark.parametrize("answer", [{}, {"state": None}, {"state": "open"}, {"state": "committed"}])
def test_target_abort_without_an_aborted_state_is_no_abort(answer):
    """★ The conductor reads a returning ``target.abort`` as "the target aborted" and then releases the source
    (``Conductor._aborting``). Route #32 answers ``{state}`` after the abort; only ``aborted``/``expired`` confirm
    it, a committing/committed target keeps its state in the refusal (the conductor goes in doubt, V37).

    Red proof: on ``c31da4c3`` ``RemoteTarget.abort`` ignored the answer. Mutant 11 (answer ignored again).
    """
    with pytest.raises(InvalidState) as info:
        RemoteTarget(_Wire(answer)).abort(job_context(mode=Mode.PUSH), "x")
    assert info.value.details.get("state") == answer.get("state")
    assert info.value.retryable is False


# ---------------------------------------------------------------------------------------------------------
# WireClient against the real Server in process
# ---------------------------------------------------------------------------------------------------------


class _Answer(httpx.BaseTransport):
    """The in-process listener, except for requests ``rule(request)`` answers itself: ``(status, body, signed)``."""

    def __init__(self, pair: Pair, rule) -> None:
        self.pair = pair
        self.rule = rule
        self.replaced: list[str] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        own = self.rule(request)
        if own is None:
            return self.pair.transport.handle_request(request)
        status, body, signed = own
        self.replaced.append(f"{request.method} {request.url.path.rsplit('/v1/', 1)[-1]}")
        headers = {"content-type": "application/json"}
        if signed:
            msg = response_string(status, request.headers["x-ctxh-nonce"], hashlib.sha256(body).hexdigest())
            headers[H_RESP_SIG] = self.pair.identity.sign(msg)
        return httpx.Response(status, headers=headers, content=body)


def _client(pair: Pair, transport: httpx.BaseTransport) -> WireClient:
    peer = Peer(
        name="server",
        peer_id=pair.peer_id,
        direction="dial",
        url="http://127.0.0.1:9",
        pubkey=pair.identity.public_b64(),
        rights=ALL_RIGHTS,
        gate=GateAuth(mode=GateMode.SESSION_TOKEN),
    )
    return WireClient(
        peer,
        pair.dialer_identity,
        read_secret=lambda name: "gate-token",
        listener_id=pair.identity.instance_id,
        transport=transport,
        max_attempts=1,
        sleep=lambda s: None,
    )


def _put_blocks_answer(body: bytes):
    return lambda request: (200, body, True) if request.method == "PUT" else None


@pytest.mark.parametrize(
    "answer",
    [
        {},
        {"stored": 1},
        {"rejected": []},
        {"stored": "1", "rejected": []},
        {"stored": True, "rejected": []},
        {"stored": -1, "rejected": []},
        {"stored": 1, "rejected": "none"},
        {"stored": 0, "rejected": [{"id": None, "reason": "bad_frame"}]},
        {"stored": 0, "rejected": [{"id": "x"}]},
    ],
)
def test_put_blocks_answer_without_counts_is_no_answer(tmp_path, answer):
    """★ A signed ``PUT …/blocks`` answer without ``stored`` (a count) and ``rejected`` (a list of ``{id, reason}``)
    is malformed, not "stored, nothing rejected".

    Red proof: on ``c31da4c3`` ``data.get("stored", 0)`` / ``data.get("rejected", [])`` ⇒ ``(0, [])`` or a
    ``("None", …)`` entry. Mutant 09 (defaults restored).
    """
    pair = Pair(tmp_path)
    transport = _Answer(pair, _put_blocks_answer(json.dumps(answer).encode()))
    raw = b"block"
    frame = encode_frame(hashlib.sha256(raw).hexdigest(), raw, peer_caps=frozenset())
    with pytest.raises(BadResponseSignature):
        _client(pair, transport).put_blocks(ulid(), 1, [frame])
    assert transport.replaced, "the answer under test was never requested"


def test_put_blocks_reads_a_complete_answer(tmp_path):
    pair = Pair(tmp_path)
    body = json.dumps({"stored": 1, "rejected": [{"id": "ab", "reason": "bad_frame"}]}).encode()
    raw = b"block"
    frame = encode_frame(hashlib.sha256(raw).hexdigest(), raw, peer_caps=frozenset())
    got = _client(pair, _Answer(pair, _put_blocks_answer(body))).put_blocks(ulid(), 1, [frame])
    assert got == (1, [("ab", "bad_frame")])


def _probe_put(status: int, *, signed: bool):
    def rule(request: httpx.Request):
        if request.method == "PUT" and request.url.path.endswith("/jobs/00000000000000000000000000/blocks"):
            return status, b"<html>proxy</html>", signed
        return None

    return rule


@pytest.mark.parametrize("status", [404, 429])
def test_gate_probe_counts_only_answers_of_the_plugin(tmp_path, status):
    """★ ``doctor --measure``: a probe size passes only when the plugin itself answered (a signed refusal of the
    unknown job). An unsigned 404 or 429 in front of it says nothing about the body — it must not become "every
    size passed, nothing to do".

    Red proof: on ``c31da4c3`` ``except HandoverError: pass`` counted ``ListenerMissing``/``RateLimited`` as passed
    ⇒ ``body_limit`` 8 MiB, ``ok`` True. Mutant 10 (the unsigned signals counted again).
    """
    pair = Pair(tmp_path)
    plain = _client(pair, pair.transport).probe_gate(measure_limit=True)
    assert (plain["ok"], plain["body_limit"]) == (True, max(PROBE_SIZES_MIB) * MIB), plain
    transport = _Answer(pair, _probe_put(status, signed=False))
    report = _client(pair, transport).probe_gate(measure_limit=True)
    assert transport.replaced, "no probe reached the replaced answer"
    assert report["body_limit"] == 0 and report["ok"] is not True, report
