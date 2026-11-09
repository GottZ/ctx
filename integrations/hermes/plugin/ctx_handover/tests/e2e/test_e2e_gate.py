"""Layer 3 — the dashboard gate in front of the plugin routes (TESTPLAN §4 E16, E18, E22; R7, BX H2).

Every test restores what it changed: B's ``.env``/``config.yaml`` byte-exact (``Lab.preserved``), A's
``listen_url``, and ``serve-a`` with its fixed token (E22 runs it with the Basic login provider in between).

Runtime estimate: 2–4 min (E16 ≈ 30 s, E18 ≈ 1 min incl. a second pairing, E22 ≈ 1–2 min incl. two serve-a
restarts).
"""

from __future__ import annotations

import time

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)


def _drop_peers(lab, ev, *, dial: str, inbound: str) -> None:
    """Remove the extra pairing of a test (B: ``peers remove``; A: ``peers revoke``), so later ``doctor`` runs see only
    the stand's peers."""
    ev.cli(f"remove-{dial}", lab.handover("b", "peers", "remove", dial, timeout=120))
    ev.cli(f"revoke-{inbound}", lab.handover("a", "peers", "revoke", inbound, timeout=120))


@pytest.mark.e2e_timeout(300)
def test_e16_wrong_gate_token(lab, ev):
    """E16: with a wrong dashboard token in B's settings (``CTX_HANDOVER_PEER_TOKEN``), ``take`` ends with
    ``gate_rejected`` (exit code 4) and the remedy text of DESIGN §14.1; A's session stays untouched."""
    src = lab.make_source("a", "e16proj", repo=False, turns=("gate",))
    marks = lab.marks("a", src.sid)
    with lab.preserved("b"):
        lab.env_set("b", "CTX_HANDOVER_PEER_TOKEN", "definitely-not-the-token")
        res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=300)
    ev.cli("take-wrong-token", res)
    assert res.rc == 4 and res.error_code == "gate_rejected", res.brief()
    assert "rejected the access credential" in res.text, res.text
    assert "HERMES_DASHBOARD_SESSION_TOKEN" in res.text, res.text
    assert lab.marks("a", src.sid) == marks


@pytest.mark.e2e_timeout(420)
def test_e18_listener_is_the_gateway_listener(lab, ev):
    """E18: B pairs with A's **gateway** listener (``api_origin`` of gw-a, fixed token, no native ticket) instead of
    ``hermes serve``, and a pull through it succeeds; the source participant runs in the gateway process."""
    desc = lab.ctl.identify("a") or {}
    url = str(desc.get("api_origin") or "").rstrip("/")
    assert url.startswith("http://127.0.0.1:"), desc
    try:
        with lab.preserved("a"):
            lab.setting("a", "listen_url", url)
            made, redeemed = lab.pair("a", "b", hint="laptop18", name="servergw")
        ev.cli("pair-code", made)
        ev.cli("pair-redeem", redeemed)
        peers = {r["name"]: r for r in lab.ledger_rows("b", "SELECT name, url, direction FROM peers")}
        assert peers.get("servergw", {}).get("url", "").rstrip("/") == url, peers
        src = lab.make_source("a", "e18proj", repo=False, turns=("via the gateway",))
        mark = lab.llm_mark()
        res = lab.handover("b", "take", "servergw", src.sid, "--yes", timeout=600)
        ev.cli("take", res)
    finally:
        _drop_peers(lab, ev, dial="servergw", inbound="laptop18")
    assert res.rc == 0, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write("job-b.json", status)
    assert status["job"].get("state") == "done", status
    source = lab.job_row("a", res.job_id, "source") or {}
    ev.write("source-job-a.json", source)
    owner_pid = str(source.get("owner") or "").split(":", 1)[0].removeprefix("pid=")
    callers = {str((r.get("caller") or {}).get("pid")) for r in lab.llm_since(mark) if r.get("kind") == "aux"}
    ev.write(
        "pids.json",
        {"owner": owner_pid, "aux_callers": sorted(callers), "gw_a": desc.get("pid"), "serve_a": lab.pid("serve-a")},
    )
    assert source, "A has no source participant row for the job"
    seen = {owner_pid} | callers
    assert str(desc.get("pid")) in seen and str(lab.pid("serve-a")) not in seen, seen


@pytest.mark.e2e_timeout(600)
def test_e22_basic_login_gate(lab, ev):
    """E22 (R7): A's ``hermes serve`` runs with the Basic login provider (as ``spike/r7/basic_login.py``: user,
    password, secret and a non-loopback public URL so the gate engages on a loopback bind); B pairs with gate
    ``basic_login`` (user name in the settings, password in ``.env``) and a pull succeeds; with a wrong password
    the pull ends ``gate_rejected``."""
    password = f"e22-{_lab.uid(16)}"
    try:
        with lab.preserved("b"):
            lab.stop("serve-a")
            env = lab.ctl.env_for(
                "a",
                HERMES_DASHBOARD_BASIC_AUTH_USERNAME="lab",
                HERMES_DASHBOARD_BASIC_AUTH_PASSWORD=password,
                HERMES_DASHBOARD_BASIC_AUTH_SECRET=_lab.uid(32),
                HERMES_DASHBOARD_PUBLIC_URL="http://lab-a.invalid:47111",
            )
            pid = lab.ctl.spawn(
                "serve-a",
                [str(lab.ctl.HERMES), "serve", "--host", "127.0.0.1", "--port", "47111"],
                _lab._child_env(env),
                lab.work("a"),
            )
            ready = _lab.wait_for(lambda: lab.ctl.http_ok(_lab.SERVE_URL["a"] + "/api/auth/providers") == 200, 180)
            ev.write("serve-a-basic.json", {"pid": pid, "ready": ready})
            assert ready, "scaffold: serve-a with the Basic provider did not come up"
            no_cred = lab.ctl.http_ok(_lab.SERVE_URL["a"] + "/api/plugins/ctx_handover/v1/hello")
            assert no_cred == 401, no_cred
            lab.setting("b", "peer_username", "lab")
            lab.env_set("b", "CTX_HANDOVER_PEER_PASSWORD", password)
            made, redeemed = lab.pair("a", "b", hint="laptop22", name="serverbasic", gate="basic_login")
            ev.cli("pair-code", made)
            ev.cli("pair-redeem", redeemed)
            src = lab.make_source("a", "e22proj", repo=False, turns=("basic login",))
            res = lab.handover("b", "take", "serverbasic", src.sid, "--yes", timeout=600)
            ev.cli("take", res)
            lab.env_set("b", "CTX_HANDOVER_PEER_PASSWORD", "wrong-password")
            src2 = lab.make_source("a", "e22bproj", repo=False, turns=("wrong password",))
            bad = lab.handover("b", "take", "serverbasic", src2.sid, "--yes", timeout=300)
            ev.cli("take-wrong-password", bad)
    finally:
        lab.stop("serve-a")
        time.sleep(0.5)
        lab.start("serve-a")
        _drop_peers(lab, ev, dial="serverbasic", inbound="laptop22")
    assert res.rc == 0, res.brief()
    assert lab.job_status("b", res.job_id)["job"].get("state") == "done"
    assert bad.rc == 4 and bad.error_code == "gate_rejected", bad.brief()
