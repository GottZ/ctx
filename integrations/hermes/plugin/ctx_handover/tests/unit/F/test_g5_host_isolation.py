"""L3-F21 (R33 corrected): what ``doctor`` and the ``listener_missing`` text say under
``plugins.isolation: host``.

The lab (L15; lab finding L3-F21) pulled a session from a gateway running under
``plugins.isolation: host`` through its ``hermes serve`` four times out of five (``done``); ``doctor`` on that gateway
said FAIL "cannot receive or provide handovers over HTTP". Upstream forwards a user plugin's dashboard routes to the
profile's plugin host under ``host`` (``hermes_cli/web_server_dashboard.py`` ``_mount_one_plugin_api`` →
``_mount_hosted_plugin_api``; buffered, ``HOSTED_REQUEST_TIMEOUT_SECS``). Not run in the lab under ``host``: the
gateway's own listener as the address, pushes and offers to such a gateway, ``--continue`` there, and
``to``/``export``/``import`` started there. The texts say exactly that: no FAIL for what works, no promise for what
was not run.

Through the real surfaces (``Plugin.cli``/``Plugin.slash``) and ``FakeBackend``, like ``test_g3_f.py``."""

from __future__ import annotations

import json

import pytest

from ctx_handover.engine import surfaces
from ctx_handover.engine.errors import ListenerMissing
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer

from .test_commands import _plugin, _slash
from .test_g3_f import _cli

NOT_RUN = (
    "this gateway's own listener as the address",
    "pushes and offers to this gateway",
    "--continue",
    "`to`, `export` or `import` started here",
)


def _doctor(capsys, settings: dict, peers: list) -> tuple[dict, str]:
    """``hermes handover doctor --json`` under ``plugins.isolation: host``: the ``plugin isolation`` check and its
    rendered line."""
    config = {"plugins": {"isolation": "host", "entries": {"ctx_handover": {"settings": dict(settings)}}}}
    plugin, _backend, _ = _plugin(FakeBackend(peers=peers, config=config))
    _cli(plugin, "doctor", "--json")
    payload = json.loads(capsys.readouterr().out.strip().splitlines()[-1])
    (check,) = [c for c in payload["checks"] if c.get("name") == "plugin isolation"]
    (line,) = [x for x in payload["text"].splitlines() if "plugin isolation" in x]
    return check, line


@pytest.mark.parametrize(
    ("settings", "peers", "dialers"),
    [
        ({"inbound": "auto"}, [make_peer("laptop", direction="inbound")], "laptop"),  # lab L15: a device pulls
        ({"inbound": "on"}, [], "paired devices"),
        ({"inbound": "auto"}, [make_peer("server")], None),  # only dials out
        ({"inbound": "off"}, [make_peer("laptop", direction="inbound")], None),
    ],
    ids=["dialed", "inbound_on", "dials_out", "inbound_off"],
)
def test_g5_doctor_under_host_isolation_says_what_works_and_what_was_not_run(capsys, settings, peers, dialers):
    """★ L3-F21 / R33: under ``plugins.isolation: host`` the ``plugin isolation`` line is a warning, never a FAIL, in
    every inbound situation: it says the routes go through ``hermes serve`` and the plugin host, names what was run
    end to end (a pull from here through ``hermes serve``, ``take`` started here) and lists as not verified what the
    lab did not run; it keeps the CLI/progress/buffer notes (E14 checks "CLI" and "buffers request bodies"). A paired
    device that dials in is named with the advice to use the ``hermes serve`` address.

    Red on ``08cfe0f0``: ``AssertionError`` — the line was ``[FAIL] … cannot receive or provide handovers over HTTP
    (its ctx_handover routes need in_process, R33) …`` with ``ok: false`` for ``dialed`` and ``inbound_on`` (the lab
    pulled through exactly such a gateway), and ``[warn]`` with the same claim otherwise.

    Mutants: ``"ok": False if expected else None`` back (``dialed``/``inbound_on`` red); the old
    ``HOST_ISOLATION_DETAIL`` (every case red)."""
    check, line = _doctor(capsys, settings, peers)
    assert check["ok"] is None and line.startswith("[warn] plugin isolation: "), line
    assert "cannot receive" not in line and "cannot reach" not in line, line
    assert "`hermes serve` forwards" in line and "plugin host" in line, line
    assert "Verified end to end under host: a paired device pulling a session from here through `hermes serve`" in line
    assert "`take` started here" in line, line
    not_run = line.split("Not verified under host: ", 1)[1]
    for what in NOT_RUN:
        assert what in not_run, (what, line)
    assert "CLI" in line and "buffers request bodies" in line and "no progress notices" in line, line
    if dialers:
        assert f"Paired devices that dial this gateway: {dialers}" in line, line
        assert "`hermes serve` address" in line, line
    else:
        assert "Paired devices that dial this gateway" not in line, line
    if settings["inbound"] == "off":
        assert "inbound: off" in line, line
    assert "in_process" in check["remedy"] and "verified setup" in check["remedy"], check


def test_g5_doctor_without_host_isolation_has_no_isolation_line(capsys):
    """``plugins.isolation`` absent or ``in_process``: no ``plugin isolation`` line (unchanged)."""
    for config in ({}, {"plugins": {"isolation": "in_process"}}):
        plugin, _b, _ = _plugin(FakeBackend(peers=[make_peer("laptop", direction="inbound")], config=config))
        assert "plugin isolation" not in _slash(plugin, "doctor")


def test_g5_doctor_does_not_fail_because_of_host_isolation(capsys):
    """★ L3-F21: the lab's case (``inbound: auto``, the laptop dials in) — ``doctor`` exits 0 when nothing else fails.

    Red on ``08cfe0f0``: exit 1 (``EXIT_FAILED`` from the isolation FAIL alone).
    Mutant: ``"ok": False if expected else None`` back."""
    config = {"plugins": {"isolation": "host", "entries": {"ctx_handover": {"settings": {"inbound": "auto"}}}}}
    plugin, _backend, _ = _plugin(FakeBackend(peers=[make_peer("laptop", direction="inbound")], config=config))
    code = _cli(plugin, "doctor", "--json")
    payload = json.loads(capsys.readouterr().out.strip().splitlines()[-1])
    failed = [c["name"] for c in payload["checks"] if c.get("ok") is False]
    assert "plugin isolation" not in failed, failed
    assert code == (1 if failed else 0), (code, failed)


def test_g5_missing_routes_text_does_not_claim_host_isolation_blocks_http():
    """★ L3-F21: the ``listener_missing`` text (a dialer got an unsigned 404) named host isolation as a cause that
    blocks HTTP altogether. Now: the causes the listener has for that answer (not installed, not enabled,
    ``inbound: off``, plugin not started), the restart after enabling or changing ``plugins.isolation``, that under
    ``host`` pulls through ``hermes serve`` are verified and ``in_process`` is the verified setup for the rest, and
    the bundle path.

    Red on ``08cfe0f0``: ``AssertionError`` — the text said "Under plugins.isolation: host a gateway cannot receive or
    provide handovers over HTTP (R33)".
    Mutant: the old text."""
    text = surfaces.render_error(ListenerMissing("no routes"), "server")
    assert "cannot receive or provide" not in text, text
    assert "server has no ctx_handover routes" in text, text
    assert "inbound: off" in text and "restart `hermes serve` and the gateway on server" in text, text
    assert "changed plugins.isolation" in text, text
    assert "Under plugins.isolation: host on server, pulls through its `hermes serve` address are verified" in text
    assert "in_process" in text and "/handover export" in text, text


@pytest.mark.parametrize("hosted", [True, False], ids=["plugin_host", "in_process"])
def test_g5_running_text_promises_a_notice_only_where_one_is_posted(monkeypatch, hosted):
    """★ L3-F21, same family: in the plugin host there is no ``post_message`` (no progress notice), yet ``/handover to``
    answered "progress shows in the notice above" (lab E14 ``p3/test_e14_host_isolation/03-take.json``). Now the text
    points to ``/handover status`` there; with a notice it is unchanged.

    Red on ``08cfe0f0`` (``plugin_host``): the text said "progress shows in the notice above".
    Mutant: the ``_can_post()`` branch removed (old text always)."""
    from ctx_handover.engine import hostcompat

    from .test_commands import Ctx

    if hosted:
        monkeypatch.setenv(hostcompat.HOST_PROCESS_ENV, "1")
    else:
        monkeypatch.delenv(hostcompat.HOST_PROCESS_ENV, raising=False)

    class PostCtx(Ctx):
        def post_message(self, text):
            if hosted:
                raise AssertionError("post_message must not be called in the plugin host")
            return None

    plugin, backend, _ctx = _plugin(ctx=PostCtx())
    out = _slash(plugin, "to server")
    assert backend.names()[-1] == "continue_async" and "is running" in out, out
    if hosted:
        assert "notice above" not in out and "shows its progress" in out and "/handover status" in out, out
    else:
        assert "progress shows in the notice above" in out, out
