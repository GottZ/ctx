"""Review round rv-f — surface texts checked on the real engine path (two homes, a real listener process; the same
harness as ``tests/integration``):

* M31 (``review-code/REVIEW.md`` §2, g6 M5, hardening no. 14): the final message of an ``import`` names the note the
  agent gets and the origin — taken from the job, not from a renderer fed by hand.
* M30 (rest, R35): after a finished pull, ``reclaim`` on the source says that the copy on the other side is not told;
  the sentence follows the result's structured fields (``state``, ``target_notified``), not the wording of a warning.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine import jobs, lock
from ctx_handover.engine.model import Mode, Options, PlanHints, Right, SessionRef
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration._int3_helpers import arrival, bounded, run_job, session_in, surface
from ctx_handover.tests.integration.harness import Home, Listener, pair


@pytest.fixture
def online(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    server, laptop = Home(tmp_path, "server"), Home(tmp_path, "laptop")
    server.identity()
    listener = Listener(server)
    try:
        peer = pair(server, laptop, listener)
        yield server, laptop, listener, peer
    finally:
        listener.stop()
        jobs._reset_for_tests()


def test_rv_f_m31_import_final_message_names_note_and_origin(online):
    """★ M31: ``hermes handover export`` on the laptop, ``hermes handover import <file>`` on the server ⇒ the import's
    final message carries the origin line and the note text the agent gets, both as the job recorded them.

    Red proof (private tree copy, ``lab/L2-redproof/rv-f``): ``JobRunner`` status summary without ``note``/``origin``
    (``jobs.py`` summary ``"note": None, "origin": None``) ⇒ ``AssertionError: no origin line``."""
    server, laptop, _listener, _peer = online
    bounded(laptop)
    bounded(server)
    work = laptop.home / "notes"
    work.mkdir()
    (work / "todo.md").write_text("- write the report\n", encoding="utf-8")
    sid = session_in(laptop, work, "Note and origin")
    exp, _ = surface(laptop, f"export --session {sid} --yes --json")
    assert exp.exit_code == 0, ("export failed", exp.out)
    bundles = sorted(p for p in (laptop.home / "hermes-handover").iterdir() if p.name.endswith(".ctxh.tar"))
    assert len(bundles) == 1, bundles
    imp, _ = surface(server, f"import {bundles[0]} --yes --json")
    assert imp.exit_code == 0, ("import failed", imp.out)
    row = arrival(server, imp.data.get("job_id"))
    assert row is not None and row.get("note"), row
    text = "\n".join(imp.out)
    status = server.runner().status(imp.data["job_id"])["job"]
    origin = status.get("origin")
    assert isinstance(origin, str) and origin, status
    assert f"Origin: {origin}." in text, ("no origin line", text)
    assert "The agent gets this note with your first message" in text, text
    assert str(row["note"]).strip().splitlines()[0] in text, ("the note text is missing", text)


@pytest.mark.parametrize("reworded", [False, True], ids=["lock_wording", "other_wording"])
def test_rv_f_m30_reclaim_after_a_pull_says_the_copy_is_not_told(online, monkeypatch, reworded):
    """★ M30 (rest): the laptop pulls a session from the server; ``reclaim`` on the server (the source) says that the
    copy on the laptop stays active and is not told — also when B words its warning differently (the sentence hangs
    on ``state == "reclaimed"`` and ``target_notified``, not on the text).

    Red before the fix (``other_wording``): the notice hung on the substring "copies are now independent" of
    ``lock.reclaim``'s warning — ``AssertionError: … not told …``."""
    server, laptop, _listener, peer = online
    bounded(laptop)
    bounded(server)
    sid = session_in(server, server.home, "Pull me")
    opts = Options(refine="off", yes=True, hints=PlanHints(no_workspace=True))
    res = run_job(laptop, Mode.PULL, peer, SessionRef(id=sid), options=opts)
    assert res.rc == 0, res.why()
    if reworded:
        real = lock.reclaim

        def reworded_reclaim(*a, **kw):
            out = real(*a, **kw)
            out["warnings"] = ["the session is also on the laptop"] if out.get("warnings") else []
            return out

        monkeypatch.setattr(lock, "reclaim", reworded_reclaim)
    call, _ = surface(server, f"reclaim {sid} --json")
    result = call.data.get("reclaim") or {}
    assert result.get("state") == "reclaimed" and result.get("target_notified") is not True, result
    text = "\n".join(call.out)
    assert "not told about this reclaim" in text, text
    assert "was told" not in text, text


def test_rv_f_m30_reclaim_after_an_export_names_the_bundle_not_a_peer(online):
    """M30 (rest), the other side of the structured rule: an exported session reclaimed on its own gateway ⇒ the
    output speaks of the bundle (A's warning) and does not claim that a peer was or was not told (``export`` is True,
    ``target_notified`` absent)."""
    _server, laptop, _listener, _peer = online
    bounded(laptop)
    work = laptop.home / "export-me"
    work.mkdir()
    sid = session_in(laptop, work, "Export me")
    exp, _ = surface(laptop, f"export --session {sid} --yes --json")
    assert exp.exit_code == 0, ("export failed", exp.out)
    call, _ = surface(laptop, f"reclaim {sid} --json")
    result = call.data.get("reclaim") or {}
    assert result.get("state") == "reclaimed" and result.get("export") is True, result
    text = "\n".join(call.out)
    assert "bundle" in text, text
    assert "not told about this reclaim" not in text and "was told" not in text, text


@pytest.fixture
def unpaired(tmp_path):
    require_upstream()
    jobs._reset_for_tests()
    server, laptop = Home(tmp_path, "server"), Home(tmp_path, "laptop")
    server.identity()
    listener = Listener(server)
    try:
        yield server, laptop, listener
    finally:
        listener.stop()
        jobs._reset_for_tests()


def test_rv_f_h1_pairing_through_the_surface_stores_the_credential_per_peer(unpaired):
    """★ H1, F part (rv-c §5): ``hermes handover pair <string>`` on the laptop (``EngineBackend.redeem_pair``) hands
    cluster C the profile ``.env`` as ``pairing.SecretStore`` ⇒ the stored peer sends the variable of this peer alone
    (``CTX_HANDOVER_PEER_TOKEN_<peer id>``), and the profile ``.env`` holds the form's token there.

    Red before the fix: ``gate.token_env == 'CTX_HANDOVER_PEER_TOKEN'`` (the one shared form slot)."""
    from ctx_handover.engine import pairing
    from ctx_handover.tests.integration.harness import GATE_ENV

    server, laptop, listener = unpaired
    ident = server.identity()
    with server.active():
        code = pairing.create_code(
            jobs._profile().ledger, rights=frozenset({Right.PULL}), name_hint="laptop", ttl_s=600
        )
        text = pairing.pair_string(code, url=listener.url, fingerprint=ident.fingerprint(), name="server", profile=None)
    laptop.extra_env[GATE_ENV] = listener.token
    call, _ = surface(laptop, f"pair {text} --allow-insecure --json")
    assert call.exit_code == 0, call.out
    (peer,) = [p for p in laptop.ledger().peers() if p.direction == "dial"]
    assert peer.gate is not None and peer.gate.token_env == pairing.PEER_TOKEN_PREFIX + peer.peer_id, peer.gate
    env_file = (laptop.hermes_home / ".env").read_text(encoding="utf-8")
    assert f"{pairing.PEER_TOKEN_PREFIX}{peer.peer_id}=" in env_file
    assert listener.token in env_file
