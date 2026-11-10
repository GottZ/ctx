"""Cluster F — ``/handover`` and ``hermes handover`` (DESIGN §14.1, §14.2, R24, BX L5; TESTPLAN §2.7)."""

from __future__ import annotations

import argparse
import json

import pytest

from ctx_handover.engine import commands, surfaces
from ctx_handover.engine.commands import LOCAL_ONLY_VERBS, Plugin, allowed_on
from ctx_handover.engine.errors import GateRejected, InDoubt, SessionBusy, WaitTimeout
from ctx_handover.engine.model import GateMode, Mode, Right, SessionRef
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer

SID = "20261109_101500_aaaaaa"
KEY = f"agent:main:local:dm:{SID}"

# one valid command line per verb (all of LOCAL_ONLY_VERBS and the rest)
SAMPLE_ARGS = {
    "to": "to server",
    "take": "take server",
    "export": "export",
    "import": "import /x.ctxh.tar",
    "status": "status",
    "peers": "peers",
    "pair": "pair",
    "reclaim": "reclaim",
    "cancel": "cancel 01JCJOB",
    "resume": "resume 01JCJOB",
    "setup": "setup",
    "doctor": "doctor",
    "identity": "identity",
    "gc": "gc",
    "logs": "logs 01JCJOB",
    "help": "help",
}


class Ctx(FakeCtx):
    """FakeCtx whose ``session_cwd`` must never be consulted (R11)."""

    def session_cwd(self) -> str:
        raise AssertionError("ctx.session_cwd() must not be used (R11)")


def _plugin(backend: FakeBackend | None = None, ctx: FakeCtx | None = None) -> tuple[Plugin, FakeBackend, FakeCtx]:
    backend = backend or FakeBackend(peers=[make_peer("server"), make_peer("laptop", direction="inbound")])
    ctx = ctx or Ctx()
    return Plugin(ctx, backend_factory=lambda c, p: backend), backend, ctx


def _slash(plugin: Plugin, raw: str, platform: str = "local", sid: str | None = SID) -> str:
    return plugin.slash(raw, session_id=sid, session_key=KEY if sid else None, platform=platform, source=object())


def test_admin_verbs_refused_on_messaging_platform():
    """★ R24, SEC 6.4, I12: on a messaging platform every verb of ``LOCAL_ONLY_VERBS`` answers with the refusal and
    reaches no backend method.

    Red proof: mutant = ``allowed_on`` returns True unconditionally (no check) → ``AssertionError: ('telegram',
    'cancel', 'Cancel requested for 1JCJOB.')`` / ``assert 'Cancel requested for 1JCJOB.' == 'This command…'``.
    """
    assert {
        "pair",
        "setup",
        "peers",
        "identity",
        "take",
        "to",
        "export",
        "import",
        "reclaim",
        "resume",
        "gc",
        "cancel",
    } == LOCAL_ONLY_VERBS
    for platform in ("telegram", "discord", "slack", "whatsapp", "api_server", "webhook", "signal", None):
        for verb in sorted(LOCAL_ONLY_VERBS):
            plugin, backend, _ctx = _plugin()
            out = _slash(plugin, SAMPLE_ARGS[verb], platform=platform)
            assert out == commands.REFUSAL, (platform, verb, out)
            touched = [n for n in backend.names() if n not in {"flush_outbox"}]
            assert touched == [], (platform, verb, touched)
            assert not allowed_on(platform, verb)
    for platform in ("local", "cli", "tui", "desktop", "LOCAL"):
        assert all(allowed_on(platform, v) for v in LOCAL_ONLY_VERBS)


def test_status_allowed_on_messaging_platform():
    """status (own session only), doctor, logs of own jobs and help still answer on a messaging platform."""
    backend = FakeBackend(peers=[make_peer("server")])
    backend.jobs = [
        {
            "job_id": "01JCAAAAAAAAAAAAAAAAAMINE1",
            "mode": "push",
            "state": "packing",
            "peer": "server",
            "calling_session": SID,
            "active": True,
        },
        {
            "job_id": "01JCAAAAAAAAAAAAAAAAOTHER1",
            "mode": "pull",
            "state": "packing",
            "peer": "server",
            "calling_session": "someone-else",
            "active": True,
        },
    ]
    backend.session_states[SID] = {"source": {"state": "retired", "peer": "server", "job_id": "01JCX"}}
    backend.statuses["01JCAAAAAAAAAAAAAAAAOTHER1"] = {"calling_session": "someone-else", "state": "packing"}
    plugin, backend, _ = _plugin(backend)
    out = _slash(plugin, "status", platform="telegram")
    assert "MINE1" in out and "OTHER1" not in out and "handed over and read-only here" in out
    assert _slash(plugin, "status 01JCAAAAAAAAAAAAAAAAOTHER1", platform="telegram") == commands.REFUSAL
    assert _slash(plugin, "logs 01JCAAAAAAAAAAAAAAAAOTHER1", platform="telegram") == commands.REFUSAL
    assert "[" in _slash(plugin, "doctor", platform="telegram")
    assert "ctx_handover hands" in _slash(plugin, "help", platform="telegram")
    full = _slash(plugin, "status", platform="local")
    assert "MINE1" in full and "OTHER1" in full


def test_slash_handler_returns_after_negotiation():
    """The handler plans synchronously (session from the handler keyword, R11), posts the pre-send summary as the
    first progress message, hands the job to the runner thread and returns at once."""
    plugin, backend, ctx = _plugin()
    out = _slash(plugin, "to server --wait")
    start = backend.last("start_job")
    assert start["mode"] is Mode.PUSH and start["peer"].name == "server"
    assert start["session_ref"] == SessionRef(id=SID) and start["calling_session"] == SID
    assert start["session_key"] == KEY
    assert start["options"].wait is True and start["options"].wait_timeout_s == 1800
    assert backend.names()[-1] == "continue_async"
    assert "is running" in out and "JOB01" in out
    assert ctx.messages and ctx.messages[0].startswith("Handover → server: 1 items, 3.0 MiB")


@pytest.mark.parametrize(
    ("reasons", "answer", "expected", "started"),
    [
        (["a"], None, "no_proposed", True),
        (["proposed"], None, "no_proposed", True),
        (["b"], None, "cancel", False),
        (["c"], None, "cancel", False),
        (["d"], None, "cancel", False),
        (["e"], None, "cancel", False),
        (["f"], None, "cancel", False),
        (["g"], None, "cancel", False),
        (["a", "b"], None, "cancel", False),
        (["include_secret"], None, "cancel", False),
        (["not_a_reason_code"], None, "cancel", False),
        (["b"], "yes", "yes", True),
        (["a"], "no_proposed", "no_proposed", True),
        (["c"], "show_plan", "show_plan", False),
        (["c"], "cancel", "cancel", False),
        (["c"], "something else", "cancel", False),
    ],
)
def test_slash_confirmation_choices(reasons, answer, expected, started):
    """★ BX L5, SEC 5.8: the confirmation offers yes / without proposed items / show plan / cancel; an
    unanswered question (``ask_choice`` → None after 90 s) continues without proposed items only when (a) is the
    sole reason and cancels with the plan printed for (b)–(g); nothing is locked when it ends there.

    Red proof: mutant = ``choice_on_timeout`` returns ``"yes"`` (continue on None) → 12 of 16 cases red, first
    ``AssertionError: assert ['yes'] == ['no_proposed']`` (reasons ['a']); for ['b'] ``['yes'] == ['cancel']``.
    Reasons are cluster D's codes or letters (X03); unknown codes fail closed. The info has the conductor's form
    (D's presend summary in ``summary``); mutant = that dict used as the rendering summary (layer 1, W125) →
    ``assert False`` on ``'Hand over to server: 0 items, 3.0 MiB, 0 proposed, 0 conflicts, model unchanged …'
    .startswith('Hand over to server: 1 items, 3.0 MiB, 0 proposed, 0 conflicts, model qwen3')``.
    """
    plugin, backend, ctx = _plugin()
    backend.confirm_reasons = reasons
    ctx.choice_answers = [answer]
    out = _slash(plugin, "to server")
    assert backend.confirm_answers == [expected]
    question, choices = ctx.choices_asked[0]
    assert question.startswith("Hand over to server: 1 items, 3.0 MiB, 0 proposed, 0 conflicts, model qwen3")
    assert [c["value"] for c in choices] == ["yes", "no_proposed", "show_plan", "cancel"]
    assert (backend.names()[-1] == "continue_async") is started
    if expected == "show_plan" or (answer is None and expected == "cancel"):
        assert "Plan: 1 items" in out and "Nothing was locked" in out
    if not started:
        assert "continue_async" not in backend.names()


def test_slash_yes_skips_confirmation():
    plugin, backend, ctx = _plugin()
    backend.confirm_reasons = ["c"]
    _slash(plugin, "to server --yes")
    assert ctx.choices_asked == [] and backend.names()[-1] == "continue_async"


def test_slash_dial_only_peer_creates_offer_with_refinement():
    """BX H4: ``to <peer that dials in>`` stores an offer, refined right away with the session's side question
    (token captured by post_llm_call, ≤ 60 s), and tells the person what to run on the other device."""
    plugin, backend, ctx = _plugin()
    ctx.capabilities.add("llm.side_question")
    ctx.side_answers = ['{"add": [], "notes_for_target": "run make test"}']
    plugin.hooks.post_llm_call(session_id=SID, turn_id="t1", side_question_token="tok-1")
    out = _slash(plugin, "to laptop")
    offer = backend.last("create_offer")
    assert offer["peer"].name == "laptop" and offer["session_ref"] == SessionRef(id=SID)
    assert offer["refinement"] == '{"add": [], "notes_for_target": "run make test"}'
    assert ctx.side_calls[0][0] == "tok-1"
    assert "Offer created for laptop (valid" in out and "On laptop run: /handover take server" in out
    assert "start_job" not in backend.names()


def test_take_defaults_to_single_peer():
    """BX P3: exactly one paired device to dial ⇒ ``take`` needs no name; a single positional that is not a peer
    is the session id; ``--latest``/``--offer`` select the remote session."""
    plugin, backend, _ = _plugin(FakeBackend(peers=[make_peer("server"), make_peer("laptop", direction="inbound")]))
    _slash(plugin, "take --latest")
    start = backend.last("start_job")
    assert (
        start["mode"] is Mode.PULL
        and start["peer"].name == "server"
        and start["session_ref"] == SessionRef(latest=True)
    )
    _slash(plugin, "take 20261109_090000_bbbbbb")
    assert backend.last("start_job")["session_ref"] == SessionRef(id="20261109_090000_bbbbbb")
    _slash(plugin, "take server --offer 01OFFER")
    assert backend.last("start_job")["session_ref"] == SessionRef(offer_id="01OFFER")

    two = FakeBackend(peers=[make_peer("server"), make_peer("nas")])
    plugin, two, ctx = _plugin(two)
    ctx.choice_answers = ["nas"]
    two.remote = {
        "offers": [{"offer_id": "01OF", "session": {"title": "My Work"}, "expires_at": 1e18}],
        "sessions": [{"id": "S1", "title": "Other", "last_active_at": 0, "message_count": 4, "cwd": "/x"}],
    }
    ctx.choice_answers = ["nas", "offer:01OF"]
    _slash(plugin, "take")
    start = two.last("start_job")
    assert start["peer"].name == "nas" and start["session_ref"] == SessionRef(offer_id="01OF")
    labels = [c["label"] for c in ctx.choices_asked[1][1]]
    assert labels[0].startswith("Offer: My Work") and labels[1].startswith("Other ·")


def test_wait_flag_and_busy_texts():
    """R10, BX M6: ``--wait`` and the ``wait_timeout_min`` setting reach the job; ``session_busy`` and
    ``wait_timeout`` texts say what to do."""
    backend = FakeBackend(
        peers=[make_peer("server")],
        config={"plugins": {"entries": {"ctx_handover": {"settings": {"wait_timeout_min": 7}}}}},
    )
    plugin, backend, _ = _plugin(backend)
    _slash(plugin, "to server --wait")
    opts = backend.last("start_job")["options"]
    assert opts.wait is True and opts.wait_timeout_s == 420
    backend.start_error = SessionBusy("busy")
    assert "add --wait" in _slash(plugin, "to server")
    backend.start_error = WaitTimeout("late", minutes=7)
    assert "did not finish within 7 min" in _slash(plugin, "to server --wait")


def test_cli_exit_codes(capsys):
    """DESIGN §14.2: 0 ok, 1 failed, 2 usage, 3 busy/conflict, 4 auth/gate, 5 in-doubt/suspended; ``--json``."""
    plugin, backend, _ = _plugin()

    def run(*argv: str) -> int:
        return plugin.cli(argparse.Namespace(verb=argv[0] if argv else None, rest=list(argv[1:])))

    assert run("help") == 0
    assert run("bogus") == 2
    assert run("to") == 2
    assert run("to", "server") == 2  # no session in the CLI without --session
    assert run("to", "server", "--session", SID, "--yes") == 0
    assert backend.last("start_job")["session_ref"] == SessionRef(id=SID)
    assert backend.names()[-2:] == ["run_foreground", "job_status"]
    backend.foreground_code = 1
    assert run("take", "--latest", "--yes") == 1
    backend.start_error = SessionBusy("x")
    assert run("take", "--latest") == 3
    backend.start_error = GateRejected("x")
    assert run("take", "--latest") == 4
    backend.start_error = InDoubt("x")
    assert run("take", "--latest") == 5
    backend.start_error = None
    capsys.readouterr()
    assert run("status", "--json") == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload["ok"] is True and payload["exit_code"] == 0 and "status" in payload


def test_pair_listener_and_dialer_texts(fake_host):
    """R7/R17: the listener prints one paste string (never a gate credential) and names the setting to fill when
    no address is known; the dialer refuses before any network call when the gate secret is missing."""
    backend = FakeBackend(peers=[])
    plugin, backend, _ = _plugin(backend)
    out = _slash(plugin, "pair --name laptop")
    assert "listen_url" in out and "create_pair_code" not in backend.names()
    backend.config_data = {"plugins": {"entries": {"ctx_handover": {"settings": {"listen_url": "https://s.example/"}}}}}
    fake_host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "dashboard-token-value"
    out = _slash(plugin, "pair --name laptop --rights pull --ttl 15m")
    call = backend.last("create_pair_code")
    assert call["rights"] == frozenset({Right.PULL}) and call["ttl_s"] == 900 and call["url"] == "https://s.example"
    assert "/handover pair ctxh-pair:" in out and "never contains it" in out
    assert "dashboard-token-value" not in out

    fake_host.secrets.clear()
    out = _slash(plugin, "pair ctxh-pair:xyz --name server")
    assert "Dashboard token of the other gateway" in out and "redeem_pair" not in backend.names()
    fake_host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "t"
    out = _slash(plugin, "pair ctxh-pair:xyz --name server --allow-insecure")
    redeem = backend.last("redeem_pair")
    assert redeem["gate"].mode is GateMode.SESSION_TOKEN and redeem["allow_insecure"] is True
    assert "Paired with server" in out and "/handover take server" in out
    out = _slash(plugin, "pair ctxh-pair:xyz --name nas --gate basic_login")
    assert "Dashboard user name" in out


def test_peers_reclaim_cancel_gc_identity_logs():
    plugin, backend, _ = _plugin()
    assert "server" in _slash(plugin, "peers") and "laptop" in _slash(plugin, "peers")
    out = _slash(plugin, "peers revoke laptop")
    assert "Revoked laptop" in out and "HERMES_DASHBOARD_SESSION_TOKEN" in out
    assert "peers remove server" in _slash(plugin, "peers revoke server")
    assert "Removed server" in _slash(plugin, "peers remove server")
    assert "Reclaimed" in _slash(plugin, "reclaim --force") and backend.last("reclaim") == {
        "session_id": SID,
        "force": True,
    }
    assert "Cancel requested" in _slash(plugin, "cancel 01JCJOBXYZ")
    assert "Freed 5.0 MiB" in _slash(plugin, "gc --job 01JCJOBXYZ")
    assert "fedcba9876543210" in _slash(plugin, "identity rotate")
    assert "intent lock" in _slash(plugin, "logs 01JCJOB")


def test_setup_asks_and_writes_settings():
    backend = FakeBackend(config={"memory": {"provider": "ctx_checkpoint"}})
    plugin, backend, ctx = _plugin(backend)
    ctx.choice_answers = ["on", "yes"]
    out = _slash(plugin, "setup")
    assert backend.settings_written == {"inbound": "on", "ctx_persist": "auto"}
    assert backend.granted == ["ctx"]
    assert "listen_url" in out


def test_doctor_warns_deployment_token_and_readable_env(fake_host, hermes_home):
    """R17, SEC 1.1, W066: a stored dashboard token or password (it opens the peer's whole dashboard) and a group-
    or world-readable profile ``.env`` are reported as warnings with the remedy, worded by cluster C's
    ``wire.gate_warnings``.

    Red proof: mutant = ``_gate_warnings`` returns None (F's own fallback text) →
    ``assert '[warn] stored dashboard token: CTX_HANDOVER_PEER_TOKEN (for server): the stored dashboard token opens
    the whole dashboard of this paired machine' in '…'``.
    """
    env = hermes_home / ".env"
    env.write_text("CTX_HANDOVER_IDENTITY_KEY=x\n", encoding="utf-8")
    env.chmod(0o644)
    fake_host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "tok"
    fake_host.secrets["CTX_HANDOVER_PEER_PASSWORD"] = "pw"
    plugin, _backend, _ = _plugin()
    out = _slash(plugin, "doctor")
    assert (
        "[warn] stored dashboard token: CTX_HANDOVER_PEER_TOKEN (for server): the stored dashboard token opens the "
        "whole dashboard of this paired machine" in out
    )
    assert "[warn] stored dashboard password: CTX_HANDOVER_PEER_PASSWORD (for server): a dashboard password" in out
    assert "[warn] profile .env permissions" in out and "group- or world-readable (mode 644)" in out
    assert "chmod 600" in out
    env.chmod(0o600)
    fake_host.secrets.clear()
    out = _slash(plugin, "doctor")
    assert "stored dashboard" not in out and "[ok] profile .env permissions" in out


def test_doctor_reports_approvals_off_with_remote_continue(fake_host):
    fake_host.config = {
        "approvals": {"mode": False},
        "plugins": {"entries": {"ctx_handover": {"settings": {"allow_remote_continue": True}}}},
    }
    backend = FakeBackend(config=fake_host.config)
    plugin, _backend, _ = _plugin(backend)
    out = _slash(plugin, "doctor")
    assert "approvals are off on this gateway, so a --continue from a paired device never starts the agent here" in out


def test_handler_never_raises():
    plugin, backend, _ = _plugin()
    backend.start_error = RuntimeError("boom")
    out = _slash(plugin, "to server")
    assert "failed unexpectedly (RuntimeError)" in out
    assert surfaces.USAGE["to"] in _slash(plugin, "to server --bogus")


def test_final_text_rendered_when_job_ends():
    """The sticky notice turns into the final message when the runner reports ``done`` (§11.6)."""
    from ctx_handover.engine.model import Progress

    plugin, backend, ctx = _plugin()
    _slash(plugin, "take server --latest")
    job_id = backend.last("start_job")["job_id"]
    backend.statuses[job_id] = {
        "state": "done",
        "title": "My Work",
        "ack": {
            "session_id": "20261109_141003_a1b2c3",
            "root_id": "20261109_141003_a1b2c3",
            "title": "My Work",
            "dests": [["w0", "/home/u/app", "fresh"]],
            "path_map": [{"source": "/srv/app", "target": "/home/u/app", "kind": "workspace"}],
            "model": "qwen3",
            "offline_capable": True,
            "warnings": [],
            "committed_at": 1.0,
        },
        "note": "Moved from: server",
        "origin": "verified peer server",
    }
    reporter = backend.last("start_job")["reporter"]
    reporter.progress(Progress(step="done"))
    final = ctx.messages[0]
    assert '"My Work" is here now' in final and "works offline: yes" in final and "Moved from: server" in final


def test_host_isolation_slash_path_without_post_message(monkeypatch):
    """§15.3: in the plugin host the slash path works; no progress notice is attempted (the host's
    ``post_message`` stub raises), ``/handover status`` stays authoritative."""
    from ctx_handover.engine import hostcompat

    monkeypatch.setenv(hostcompat.HOST_PROCESS_ENV, "1")

    class HostCtx(Ctx):
        def post_message(self, text):
            raise AssertionError("post_message must not be called in the plugin host")

    plugin, backend, _ctx = _plugin(ctx=HostCtx())
    out = _slash(plugin, "to server")
    assert backend.names()[-1] == "continue_async" and "is running" in out


def test_status_of_job_shows_note_origin_and_exclusions():
    """SEC 4.6: ``/handover status <job>`` on the receiving side shows origin, exclusions, instruction files and
    the full note text."""
    plugin, backend, _ = _plugin()
    backend.statuses["01JCAAAAAAAAAAAAAAAAARRIV1"] = {
        "job_id": "01JCAAAAAAAAAAAAAAAAARRIV1",
        "mode": "pull",
        "state": "done",
        "peer": "server",
        "note": "<ctx-handover-note>Moved from: server</ctx-handover-note>",
        "origin": "verified peer server",
        "excluded_secrets": ["/srv/app/.env"],
        "instruction_files": ["AGENTS.md (new)"],
    }
    out = _slash(plugin, "status 01JCAAAAAAAAAAAAAAAAARRIV1")
    for needle in (
        "RRIV1",
        "Origin: verified peer server",
        "/srv/app/.env",
        "AGENTS.md (new)",
        "Moved from: server",
        "the chat does not show it",
    ):
        assert needle in out, needle


def test_yes_never_confirms_suspected_secrets():
    """R25: ``--yes`` confirms only proposals the policy found unsuspicious; with suspected secrets among the
    proposals the answer is "without the proposed items", and the interactive question names them together with
    ``--include-secret``."""
    from ctx_handover.tests.fakes.surfaces_fake import sample_final_plan

    plugin, backend, ctx = _plugin()
    backend.final = sample_final_plan(proposed=1, suspected=1)
    backend.confirm_reasons = ["a"]
    backend.confirm_even_with_yes = True
    _slash(plugin, "to server --yes")
    assert backend.confirm_answers == ["no_proposed"]
    backend.final = sample_final_plan(proposed=1)
    _slash(plugin, "to server --yes")
    assert backend.confirm_answers[-1] == "yes"
    backend.final = sample_final_plan(proposed=1, suspected=1)
    backend.confirm_even_with_yes = False
    ctx.choice_answers = ["no_proposed"]
    _slash(plugin, "to server")
    question = ctx.choices_asked[-1][0]
    assert "Files that look like secrets: /home/u/token0.txt" in question and "--include-secret" in question
