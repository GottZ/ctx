"""Layer 3 (lab) — the surfaces of ctx_handover on two real gateways (owner: cluster F; BUILD-ORDER §5,
TESTPLAN §4; R7, R12, R14, R24, BX P1/P2/P6).

Not part of the unit run. The stand comes from ``tests/e2e/conftest.py::lab`` (session fixture, BUILD-ORDER §5
steps 1–2): ``ctx_handover`` copied into and enabled in ``lab/a`` and ``lab/b``, ``llm gw-a gw-b serve-a serve-b``
running with fixed dashboard tokens (``serve-a`` on 127.0.0.1:47111), ``lab/b/.env`` holding
``CTX_HANDOVER_PEER_TOKEN`` = serve-a's token — the value the Desktop settings form would store for the secret field
*Dashboard token of the other gateway* — and both directions already paired (on B the dial peer ``server``).

Run with ``CTXH_E2E=1`` (otherwise the module is skipped), ``--basetemp=…/lab/pytest-e2e``; the fixture stops the
lab afterwards and checks ``labctl.py status`` (0 lab processes). R24 on a messaging adapter has no lab platform;
it is covered by ``tests/unit/F/test_commands.py::test_admin_verbs_refused_on_messaging_platform``.
Runtime estimate: 2–3 min.
"""

from __future__ import annotations

import json
import os
import re
import time
from typing import Any

import pytest

SERVE_A = "http://127.0.0.1:47111"
PAIR_RE = re.compile(r"(ctxh-pair:[A-Za-z0-9_=-]+)")

pytestmark = pytest.mark.skipif(
    os.environ.get("CTXH_E2E") != "1", reason="layer-3 lab test (BUILD-ORDER §5): set CTXH_E2E=1 with the lab running"
)


@pytest.fixture(scope="module")
def lab(lab):
    """The shared stand of ``tests/e2e/conftest.py`` in the shape these tests use (``ctl``, ``GW``, ``token``,
    ``ws``, ``handover``, ``restore_pairing``)."""
    labctl, token = lab.ctl, lab.tokens["a"]
    for inst in ("a", "b"):
        if labctl.identify(inst) is None:
            pytest.fail(f"gateway {inst} is not running: labctl.py start llm gw-a gw-b serve-a serve-b")
        if not (labctl.home(inst) / "plugins" / "ctx_handover" / "plugin.yaml").is_file():
            pytest.fail(f"ctx_handover is not installed in lab/{inst}/plugins (BUILD-ORDER §5 step 1)")
    env_b = (labctl.home("b") / ".env").read_text(encoding="utf-8") if (labctl.home("b") / ".env").exists() else ""
    if f"CTX_HANDOVER_PEER_TOKEN={token}" not in env_b:
        pytest.fail("lab/b/.env needs CTX_HANDOVER_PEER_TOKEN equal to the token of serve-a (BUILD-ORDER §5 step 1)")
    shape = {"ctl": labctl, "GW": lab.GW, "token": token, "ws": lab.ws}
    shape.update(handover=lab.handover, restore_pairing=lab.restore_pairing)
    return type("Lab", (), shape)


def _hermes(lab, inst: str, *args: str, timeout: float = 120) -> Any:
    return lab.ctl.run_hermes(inst, list(args), timeout=timeout)


def _session(lab, inst: str, cwd: str | None = None) -> str:
    gw = lab.GW(inst)
    try:
        reply = gw.rpc(
            "session.create",
            {"request_id": f"e2e-{time.time()}", "source": "cli", "cwd": cwd or str(lab.ctl.home(inst) / "work")},
        )
    finally:
        gw.close()
    result = reply.get("result") or {}
    sid = result.get("session_id") or (result.get("ref") or {}).get("session_id") or result.get("id")
    assert sid, reply
    return str(sid)


def _slash(lab, inst: str, sid: str, command: str, *, timeout: float = 180, linger: float = 1.0) -> tuple[str, list]:
    gw = lab.GW(inst)
    try:
        gw.rpc("session.resume", {"session_id": sid})
        reply = gw.rpc("slash.exec", {"session_id": sid, "command": command}, timeout=timeout)
        gw.linger(linger)
        events = list(gw.events)
    finally:
        gw.close()
    result = reply.get("result") or {}
    return str(result.get("output") or reply.get("error") or ""), events


def test_e2e_registration_slash_and_cli(lab):
    """R12, L1/L2: ``/handover`` answers through the gateway (slash.exec) and ``hermes handover`` in a CLI process;
    ``--json`` gives a parseable result with exit code 0."""
    sid = _session(lab, "a")
    out, _ = _slash(lab, "a", sid, "/handover help")
    assert "ctx_handover moves this session" in out
    res = _hermes(lab, "a", "handover", "help")
    assert res.returncode == 0 and "ctx_handover moves" in res.stdout
    res = _hermes(lab, "a", "handover", "status", "--json")
    assert res.returncode == 0, res.stderr
    payload = json.loads(res.stdout.strip().splitlines()[-1])
    assert payload["ok"] is True and payload["exit_code"] == 0


def test_e2e_settings_form_carries_the_schema(lab):
    """R7, BX P1: Desktop's settings form (``plugins.manage list``) shows the plugin's ``config_schema`` with the two
    secrets as ``.env``-backed fields; no secret value appears in the listing."""
    gw = lab.GW("b")
    try:
        reply = gw.rpc("plugins.manage", {"action": "list"})
    finally:
        gw.close()
    blob = json.dumps(reply)
    assert "ctx_handover" in blob
    assert "CTX_HANDOVER_PEER_TOKEN" in blob and "CTX_HANDOVER_PEER_PASSWORD" in blob
    assert lab.token not in blob


def test_e2e_setup_without_terminal_and_pair_by_paste_string(lab, ev):
    """R7, R17, BX P2: the address is set through the settings writer (``plugins.manage settings``), pairing is one
    pasted line, and that line never contains the dashboard token.

    The second pairing of B's key with A supersedes the stand's entry ``server`` (review M13: one key, one live row on
    A), so ``doctor`` on B may report exactly that entry as ``[FAIL] peer server: … pair again``; any other ``[FAIL]``
    fails the test. Afterwards ``server-f``/``laptop-f`` are removed and the stand is paired again
    (``Lab.restore_pairing``)."""
    gw = lab.GW("a")
    try:
        reply = gw.rpc(
            "plugins.manage", {"action": "settings", "key": "ctx_handover", "values": {"listen_url": SERVE_A}}
        )
        assert not reply.get("error"), reply
    finally:
        gw.close()
    sid_a = _session(lab, "a")
    out_a, _ = _slash(lab, "a", sid_a, "/handover pair --name laptop-f")
    match = PAIR_RE.search(out_a)
    assert match, out_a
    assert lab.token not in out_a
    sid_b = _session(lab, "b")
    try:
        out_b, _ = _slash(lab, "b", sid_b, f"/handover pair {match.group(1)} --name server-f")
        assert "Paired with server-f" in out_b, out_b
        peers_a, _ = _slash(lab, "a", sid_a, "/handover peers")
        peers_b, _ = _slash(lab, "b", sid_b, "/handover peers")
        assert "laptop-f · inbound" in peers_a and "server-f · dial" in peers_b
        doctor, _ = _slash(lab, "b", sid_b, "/handover doctor")
    finally:
        ev.cli("remove-server-f", lab.handover("b", "peers", "remove", "server-f", timeout=120))
        ev.cli("revoke-laptop-f", lab.handover("a", "peers", "revoke", "laptop-f", timeout=120))
        lab.restore_pairing(ev)
    ev.write("doctor-b.txt", doctor)
    fails = [ln for ln in doctor.splitlines() if ln.startswith("[FAIL]")]
    assert "[ok] peer server-f" in doctor, doctor
    assert all(ln.startswith("[FAIL] peer server:") and "pair again" in ln for ln in fails), fails
    assert "[warn] stored dashboard token" in doctor  # R17: the stored token opens A's whole dashboard


def test_e2e_take_progress_notice_and_final_message(lab):
    """R14, BX P6, L2: a pull started with ``/handover take`` returns at once, shows one sticky progress notice
    that ends as the final message (target session, folder, model, offline yes/no, next step), and the arrival
    note is visible through ``/handover status <job>`` on the target.

    A's session works in ``lab/ws-e2e/a`` (outside the Hermes home): a cwd inside it is left out and, since R30, needs
    a person's confirmation that ``--yes`` does not give (``test_l3_findings.py`` covers that path)."""
    sid_a = _session(lab, "a", cwd=str(lab.ws("a")))
    gw = lab.GW("a")
    try:
        gw.rpc("session.resume", {"session_id": sid_a})
        gw.rpc("prompt.submit", {"session_id": sid_a, "text": "hello from a", "input_id": f"e2e-{time.time()}"})
        assert gw.wait(lambda ev: ev.get("type") in {"message.complete", "error"}, 120)
    finally:
        gw.close()
    sid_b = _session(lab, "b")
    gw = lab.GW("b")
    try:
        gw.rpc("session.resume", {"session_id": sid_b})
        reply = gw.rpc(
            "slash.exec", {"session_id": sid_b, "command": f"/handover take server {sid_a} --yes"}, timeout=300
        )
        out = str((reply.get("result") or {}).get("output") or reply)
        assert "is running" in out, out
        job = re.search(r"/handover status ([0-9A-Z]{6})", out).group(1)

        def final(ev: dict) -> bool:
            payload = ev.get("payload") or {}
            text = json.dumps(payload)
            return (
                ev.get("type") == "notification.show"
                and "plugin-message:ctx_handover" in text
                and ("is here now" in text or "is active here" in text or "ended" in text)
            )

        done = gw.wait(final, 600)
        assert done, [e.get("type") for e in gw.events][-20:]
    finally:
        gw.close()
    text = json.dumps(done)
    for needle in ("(session ", "model ", "works offline: "):
        assert needle in text, needle
    status, _ = _slash(lab, "b", sid_b, f"/handover status {job}")
    assert job in status
    assert "not shown in the transcript view" in status and "Origin: verified" in status
