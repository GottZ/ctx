"""Cluster F, layer 2 — the surfaces against the integrated contracts of clusters A, C and D.

X03 (reason codes), X05/W126 (suspected-secret marker), W125 (pre-send summary), X07 (local ``--include-secret``),
X20 (status form, end states), W066/X24 (doctor from ``probe_gate``/``gate_warnings``), W065/W119 (identity
rotation), W067/W047 (revoke, reclaim through ``jobs``), the profile runtime (one ledger, store, veto index and
runner per profile). Where the engine module exists it is used for real (``plan``, ``ledger``, ``crypto``,
``jobs``); otherwise the stand-ins copy the real signatures.
"""

from __future__ import annotations

import itertools
import threading
import types
from dataclasses import replace

import pytest

from ctx_handover.engine import commands, plan, surfaces
from ctx_handover.engine.commands import EngineBackend, Plugin
from ctx_handover.engine.model import Include, Mode, Options, PlanHints, Progress, Role, SessionRef
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer, sample_final_plan

from .test_commands import SID, _plugin, _slash
from .test_engine_backend import _stub_module

MIB = 1024**2
ACK = {
    "session_id": "20261109_141003_a1b2c3",
    "root_id": "20261109_141003_a1b2c3",
    "title": "My Work",
    "dests": [["w0", "/home/u/app", "fresh"]],
    "path_map": [{"source": "/srv/app", "target": "/home/u/app", "kind": "workspace"}],
    "model": "qwen3",
    "offline_capable": True,
    "warnings": [],
    "committed_at": 1.0,
}


# ---------------------------------------------------------------------------------------------------------
# X03, X05/W126 — reason codes and the suspected-secret marker are cluster D's
# ---------------------------------------------------------------------------------------------------------


def test_reason_codes_are_cluster_d_codes():
    """X03: the confirmation reasons are D's codes (``plan.CONFIRM_*``); letter (d) is ``include_secret``; every
    code has a text; ``suspected_secret`` is no reason code; the timeout rule equals the conductor's
    (``set(reasons) <= {"proposed"}``) for every non-empty set of D's codes.

    Red proof: mutant = ``REASONS`` with F's layer-1 letter (``"d": "secret"``) →
    ``AssertionError: assert {'a': 'propos..._secret', ...} == {'a': 'propos...'secret', ...}``.
    """
    by_letter = {letter: code for code, letter in plan.CONFIRM_LETTERS.items()}
    assert by_letter == surfaces.REASONS
    assert surfaces.reason_names(["d", "include_secret", "a"]) == ["include_secret", "proposed"]
    assert set(surfaces.REASON_TEXT) == set(plan.CONFIRM_LETTERS)
    assert plan.SUSPECTED_SECRET not in surfaces.REASON_TEXT
    assert plan.SUSPECTED_SECRET not in surfaces.REASONS.values()
    codes = sorted(plan.CONFIRM_LETTERS)
    for n in range(1, len(codes) + 1):
        for combo in itertools.combinations(codes, n):
            conductor = "no_proposed" if set(combo) <= {plan.CONFIRM_PROPOSED} else "cancel"
            assert surfaces.choice_on_timeout(list(combo)) == conductor, combo


def test_confirmation_explains_d_codes_in_words():
    """X03: the question explains (d) ``include_secret`` in words instead of printing the raw code."""
    plugin, backend, ctx = _plugin()
    backend.confirm_reasons = [plan.CONFIRM_SECRET]
    ctx.choice_answers = ["cancel"]
    _slash(plugin, "to server --include-secret /srv/app/.env")
    question = ctx.choices_asked[0][0]
    assert "--include-secret sends a file from the secrets list" in question
    assert "include_secret" not in question.replace("--include-secret", "")


def _with_reasons(final, item_id: str, reasons: tuple[str, ...]):
    items = tuple(replace(i, reasons=reasons) if i.item_id == item_id else i for i in final.plan.items)
    return replace(final, plan=replace(final.plan, items=items))


def test_suspected_secret_is_the_exact_marker():
    """X05/W126: a suspected secret is a proposal whose ``reasons`` contain the exact element
    ``plan.SUSPECTED_SECRET``; free text that merely contains the word is not one.

    Red proof: mutant = substring search over the joined reasons (F's layer-1 rule) →
    ``AssertionError: assert ['/home/u/notes0.md'] == []`` (``not_a_suspected_secret_hint``).
    """
    final = sample_final_plan(proposed=1, suspected=1)
    assert surfaces.suspected_secrets({"final_plan": final}) == ["/home/u/token0.txt"]
    near = _with_reasons(sample_final_plan(proposed=1), "x0", ("model", "not_a_suspected_secret_hint"))
    assert surfaces.suspected_secrets({"final_plan": near}) == []
    spaced = _with_reasons(sample_final_plan(proposed=1), "x0", ("suspected secret: proposed only",))
    assert surfaces.suspected_secrets({"final_plan": spaced}) == []
    included = sample_final_plan(suspected=1)
    items = tuple(replace(i, include=Include.YES) if i.item_id == "s0" else i for i in included.plan.items)
    included = replace(included, plan=replace(included.plan, items=items))
    assert surfaces.suspected_secrets({"final_plan": included}) == []  # only proposals count
    # the conductor's own list is merged (it computes it from the same marker)
    assert surfaces.suspected_secrets({"final_plan": final, "suspected_secrets": ["/x/key.pem"]}) == [
        "/home/u/token0.txt",
        "/x/key.pem",
    ]


def test_timeout_with_suspected_secret_drops_all_proposals():
    """R25 with X03: a suspected secret adds no reason of its own; with (a) as the only reason an unanswered
    question continues without any proposal (the suspected one included), and the question names it."""
    plugin, backend, ctx = _plugin()
    backend.final = sample_final_plan(proposed=1, suspected=1)
    backend.confirm_reasons = [plan.CONFIRM_PROPOSED]
    ctx.choice_answers = [None]
    _slash(plugin, "to server")
    assert backend.confirm_answers == ["no_proposed"]
    question = ctx.choices_asked[0][0]
    assert surfaces.SUSPECTED_TEXT in question and "Suspected secrets: /home/u/token0.txt" in question


# ---------------------------------------------------------------------------------------------------------
# W125 — the summary is cluster D's
# ---------------------------------------------------------------------------------------------------------


def test_summary_renders_cluster_d_presend():
    """W125: counts and lists come from ``plan.presend_summary`` (the conductor's ``info["summary"]`` wins over a
    recomputation), rows/proposals/conflicts from ``plan.summarize``; the model is named from the
    ``model_change`` form ``LocalTarget`` writes.

    Red proof: mutant = ``plan_summary`` ignores ``presend`` →
    ``assert [] == ['stash@{0}']``.
    """
    final = sample_final_plan(proposed=1, secrets=2, extras=1)
    pre = plan.presend_summary(final)
    view = surfaces.plan_summary(final)
    assert view["items_count"] == pre["items"] == 2
    assert view["total_bytes"] == pre["total_bytes"] == 3 * MIB + 2048
    assert view["excluded_secrets"] == pre["excluded_secrets"] == 2
    assert view["outside_workspace"] == pre["extras_outside_workspace"] == ["/home/u/extra0.txt"]
    assert view["proposed"] == 1 and view["conflicts"] == 0 and view["need_bytes"] == 10 * MIB
    assert view["model"] == "qwen3 (local)" and view["offline"] is True
    assert {r["source_path"] for r in view["items"]} == {"/srv/app", "/home/u/notes0.md", "/home/u/extra0.txt"}
    later = surfaces.plan_summary(final, {**pre, "withheld_stash": ["stash@{0}"]})
    assert later["withheld_stash"] == ["stash@{0}"]
    assert surfaces.render_presend(view, "server", Mode.PUSH) == (
        "Handover → server: 2 items, 3.0 MiB; 2 secrets left out; 1 items outside the project."
    )
    text = surfaces.render_plan(view)
    assert "Plan: 2 items, 3.0 MiB, 1 proposed, 0 conflicts; model qwen3 (local) (offline: yes)." in text
    assert "Secrets left out: 2" in text and "/srv/app/.env (secret)" in text


def test_engine_presend_summary_renders(fake_host):
    """W125: ``EngineBackend.presend_summary`` gives the rendering view; F's layer-1 merge put D's integer
    ``excluded_secrets``/``items`` over F's lists.

    Red proof: mutant = the layer-1 merge ``{**surfaces.plan_summary(final), **plan.presend_summary(final)}`` →
    ``TypeError: 'int' object is not iterable`` in ``render_plan`` (D's ``items`` count over F's rows).
    """
    eb = EngineBackend(FakeCtx(host=fake_host), Plugin(FakeCtx(host=fake_host)))
    final = sample_final_plan(secrets=1)
    text = surfaces.render_presend(eb.presend_summary(final), "server", Mode.PUSH)
    assert text == "Handover → server: 1 items, 3.0 MiB; 1 secrets left out."
    assert "Plan: 1 items" in surfaces.render_plan(eb.presend_summary(final))


def test_confirmation_question_from_conductor_info():
    """W125 + X03 end to end through the slash handler: the conductor's info (D's reason codes, D's presend
    summary, the plan, the suspected list, instruction files) renders as counts, model and the instruction
    files that arrive."""
    plugin, backend, ctx = _plugin()
    backend.final = sample_final_plan(proposed=2, extras=1)
    backend.confirm_reasons = [plan.CONFIRM_PROPOSED, plan.CONFIRM_OUTSIDE, plan.CONFIRM_INSTRUCTIONS]
    backend.instruction_files = [{"item_id": "w0", "rel": "AGENTS.md", "status": "changed", "sha256": "0" * 64}]
    ctx.choice_answers = ["show_plan"]
    out = _slash(plugin, "take server --latest")
    question = ctx.choices_asked[0][0]
    assert question.startswith(
        "Hand over to server: 2 items, 3.0 MiB, 2 proposed, 0 conflicts, model qwen3 (local) (offline: yes)."
    )
    assert surfaces.REASON_TEXT[plan.CONFIRM_INSTRUCTIONS] in question
    assert "AGENTS.md (changed)" in out and "Nothing was locked" in out


# ---------------------------------------------------------------------------------------------------------
# X07 — the local --include-secret reaches plan_sync
# ---------------------------------------------------------------------------------------------------------


def test_start_job_passes_local_include_secret(fake_host):
    """X07: ``EngineBackend.start_job`` hands the flags typed on this (local) surface to ``plan_sync`` as the
    explicit ``include_secret`` parameter — cluster A builds ``PolicyConfig.include_secret`` only from that
    parameter — and never takes it from ``Options.hints`` (which travel to the other side; R18).

    Red proof: mutant = ``start_job`` passes ``tuple(options.hints.include_secret)`` (the checkpoint ``c32a862d``)
    → ``AssertionError: assert (() == ('/srv/app/.env',)``.
    """
    seen: dict = {}

    class Runner:
        def plan_sync(self, mode, peer, options, *, include_secret=(), **kw):
            seen.update(include_secret=include_secret, bundle_path=kw.get("bundle_path"))
            return "01JOB", sample_final_plan()

    eb = EngineBackend(FakeCtx(host=fake_host), Plugin(FakeCtx(host=fake_host)))
    eb._parts.update({"runner": Runner(), "runtime": types.SimpleNamespace()})
    kwargs = dict(
        session_ref=SessionRef(id=SID),
        calling_session=SID,
        session_key=None,
        bundle_path=None,
        confirm=None,
        side_question=None,
        reporter=None,
    )
    eb.start_job(Mode.PUSH, make_peer("server"), Options(), include_secret=("/srv/app/.env",), **kwargs)
    assert seen["include_secret"] == ("/srv/app/.env",) and seen["bundle_path"] is None
    smuggled = Options(hints=PlanHints(include_secret=("/home/u/.ssh/id_ed25519",)))
    eb.start_job(Mode.PUSH, make_peer("server"), smuggled, **kwargs)
    assert seen["include_secret"] == ()  # hints are never read for it
    parsed = surfaces.parse("to server --include-secret /srv/app/.env --include-secret ~/.ssh/config")
    assert surfaces.include_secret_from(parsed) == ("/srv/app/.env", "~/.ssh/config")
    assert surfaces.options_from(parsed, {}).hints.include_secret == ()


# ---------------------------------------------------------------------------------------------------------
# X20 — status form and end states of cluster A
# ---------------------------------------------------------------------------------------------------------

MINE = "01JC0000000000000000MINE01"
OTHER = "01JC000000000000000OTHER1"


@pytest.fixture
def real_status(fake_host, tmp_path):
    """An ``EngineBackend`` whose runtime is a real ``Ledger`` and whose runner is a real ``JobRunner``."""
    from ctx_handover.engine.jobs import JobRunner
    from ctx_handover.engine.ledger import Ledger

    ledger = Ledger(tmp_path / "ledger.sqlite3")
    laptop = make_peer("laptop", direction="inbound")
    ledger.upsert_peer(laptop)
    ledger.create_job(MINE, Role.CONDUCTOR, Mode.PUSH, None, {"calling_session": SID, "mode": "push"})
    ledger.create_job(OTHER, Role.CONDUCTOR, Mode.EXPORT, None, {"calling_session": "someone-else"})
    ledger.pull_log_put("01JC00000000000000000PULL1", laptop.peer_id, ["/srv/app"])
    ctx = FakeCtx(host=fake_host)
    holder: dict = {}
    plugin = Plugin(ctx, backend_factory=lambda c, p: holder.setdefault("eb", EngineBackend(c, p)))
    eb = plugin.backend
    runtime = types.SimpleNamespace(ledger=ledger, store=None, veto=None, lock=threading.RLock())
    eb._parts.update({"runtime": runtime, "runner": JobRunner(ledger)})
    try:
        yield plugin, ledger
    finally:
        ledger.close()


def test_status_reads_cluster_a_form(real_status):
    """X20: ``JobRunner.status`` → ``{"jobs": [...], "pull_log": [...]}`` / ``{"job": {...}}``; on a messaging
    platform only jobs of the caller's session (``calling_session``) and no pull log; locally everything, the
    pull log under the peer's name.

    Red proof: mutant = ``_own_jobs`` keeps ``pulled_by`` → ``assert 'pulled by laptop' not in '…pulled by laptop
    at …'``.
    """
    plugin, _ledger = real_status
    local = _slash(plugin, "status")
    assert MINE[-6:] in local and OTHER[-6:] in local and "pulled by laptop" in local and "/srv/app" in local
    remote = _slash(plugin, "status", platform="telegram")
    assert MINE[-6:] in remote and OTHER[-6:] not in remote
    assert "pulled by laptop" not in remote
    assert _slash(plugin, f"status {OTHER}", platform="telegram") == commands.REFUSAL
    assert MINE[-6:] in _slash(plugin, f"status {MINE}", platform="telegram")
    assert "Cancel requested" in _slash(plugin, "cancel")  # one active job of this session is found


def test_final_text_follows_end_states():
    """X20/W111: the last progress is the end state; ``done``/``aborted``/``suspended`` close the notice,
    ``in_doubt``/``retiring`` show their text but stay open (the runner passes through them on the way to
    ``done``); a cancel is ``aborted`` with ``last_error.code == "cancelled"``.

    Red proof: mutant = ``PostMessageReporter`` closes on every terminal step (layer-1 rule) → after ``done``
    ``assert ('"My Work" continues there' in '…\nNext: the other side is released …' and 'Next:' not in …)``:
    the notice still shows the ``retiring`` text.
    """
    plugin, backend, ctx = _plugin()
    _slash(plugin, "to server")
    start = backend.last("start_job")
    job_id, reporter = start["job_id"], start["reporter"]
    assert ctx.messages[0].startswith("Handover → server: 1 items")
    nxt = "the other side is released on the next contact (the janitor retries)"
    backend.statuses[job_id] = {"state": "retiring", "ack": ACK, "next_action": nxt}
    reporter.progress(Progress(step="retiring"))
    assert "Handed over to server" in ctx.messages[0] and f"Next: {nxt}" in ctx.messages[0]
    backend.statuses[job_id] = {"state": "done", "ack": ACK}
    reporter.progress(Progress(step="retired"))
    reporter.progress(Progress(step="done"))
    assert '"My Work" continues there' in ctx.messages[0] and "Next:" not in ctx.messages[0]
    reporter.progress(Progress(step="transfer"))  # nothing after a closing step
    assert '"My Work" continues there' in ctx.messages[0]

    for status, needle in (
        ({"state": "aborted", "last_error": {"code": "cancelled", "message": "x"}}, "Cancelled. Nothing was changed."),
        ({"state": "in_doubt", "last_error": {"code": "peer_unreachable", "message": "x"}}, "may already have"),
        (
            {"state": "suspended", "last_error": {"code": "peer_unreachable", "message": "timeout"}},
            "/handover resume ",
        ),
        ({"state": "suspended"}, "/handover resume "),
    ):
        plugin, backend, ctx = _plugin()
        _slash(plugin, "to server")
        job_id, reporter = backend.last("start_job")["job_id"], backend.last("start_job")["reporter"]
        backend.statuses[job_id] = status
        reporter.progress(Progress(step=status["state"]))
        assert needle in ctx.messages[0], (status, ctx.messages[0])
        if status["state"] == "suspended":
            assert f"/handover resume {job_id[-6:]}" in ctx.messages[0]


def test_planning_progress_posts_nothing_before_the_notice():
    """The planning part runs inside the handler (it answers itself): progress before ``start`` — including the
    ``aborted`` of a ``show_plan``/``cancel`` — posts no second notice."""
    plugin, backend, ctx = _plugin()
    backend.confirm_reasons = ["c"]
    ctx.choice_answers = ["cancel"]

    def start_job(*a, **kw):
        kw["reporter"].progress(Progress(step="planning"))
        kw["reporter"].progress(Progress(step="aborted"))
        return original(*a, **kw)

    original, backend.start_job = backend.start_job, start_job
    out = _slash(plugin, "to server")
    assert ctx.messages == [] and "Cancelled. Nothing was locked." in out


def test_dry_run_shows_the_plan_the_job_ended_with(fake_host, tmp_path):
    """W113: a dry run ends after NEGOTIATED with ``plan_sync`` → ``(job_id, None)``; the backend reads the plan
    from the conductor row (``result.plan``) so the person and the agent tool see it."""
    from ctx_handover.engine.jsonutil import to_wire
    from ctx_handover.engine.ledger import Ledger

    ledger = Ledger(tmp_path / "ledger.sqlite3")
    try:
        final = sample_final_plan(job_id="01JC0000000000000000DRYRUN")
        ledger.create_job("01JC0000000000000000DRYRUN", Role.CONDUCTOR, Mode.EXPORT, None, {})
        ledger.job_update("01JC0000000000000000DRYRUN", Role.CONDUCTOR, result={"plan": to_wire(final)})

        class Runner:
            def plan_sync(self, *a, **kw):
                return "01JC0000000000000000DRYRUN", None

        eb = EngineBackend(FakeCtx(host=fake_host), Plugin(FakeCtx(host=fake_host)))
        eb._parts.update({"runner": Runner(), "runtime": types.SimpleNamespace(ledger=ledger)})
        _job, got = eb.start_job(
            Mode.EXPORT,
            None,
            Options(dry_run=True),
            session_ref=SessionRef(id=SID),
            calling_session=SID,
            session_key=None,
            bundle_path=None,
            confirm=None,
            side_question=None,
            reporter=None,
        )
        assert got == final
        _job, none = eb.start_job(
            Mode.EXPORT,
            None,
            Options(),
            session_ref=SessionRef(id=SID),
            calling_session=SID,
            session_key=None,
            bundle_path=None,
            confirm=None,
            side_question=None,
            reporter=None,
        )
        assert none is None  # a cancelled real run stays cancelled
    finally:
        ledger.close()


# ---------------------------------------------------------------------------------------------------------
# W066/X24 — doctor from cluster C's probe report
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("probe", "skew", "ok", "detail", "remedy"),
    [
        ({"gate": "unreachable", "plugin": False, "signed": False, "error": "peer_unreachable"}, None, False,
         "did not answer (peer_unreachable)", "network or VPN"),
        ({"gate": "rejected", "plugin": False, "signed": False, "error": "gate_rejected"}, None, False,
         "refused the stored credential (session_token", "rejected the access credential"),
        ({"gate": "ok", "plugin": False, "signed": False, "error": "listener_missing"}, None, False,
         "ctx_handover did not (listener_missing)", "has no ctx_handover routes"),
        ({"gate": "ok", "plugin": True, "signed": False, "error": "unauthorized"}, None, False,
         "without a valid signed hello (unauthorized)", "did not accept this device's key"),
        ({"gate": "ok", "plugin": True, "signed": True, "body_limit": 8 * MIB}, 2.0, True,
         "valid signature; request bodies of 8 MiB pass", None),
        ({"gate": "ok", "plugin": True, "signed": True, "body_limit": 2 * MIB}, None, None,
         "limits request bodies to 2.0 MiB", "raise the body limit"),
        ({"gate": "ok", "plugin": True, "signed": True, "body_limit": 0}, None, None,
         "below 1 MiB", "raise the body limit"),
        ({"gate": "ok", "plugin": True, "signed": True, "body_limit": 8 * MIB}, -420.0, None,
         "clock differs by -420 s", "synchronise the clocks"),
    ],
)  # fmt: skip
def test_doctor_peer_check_from_probe_report(probe, skew, ok, detail, remedy):
    """W066/X24, DESIGN §14.1 doctor: ``WireClient.probe_gate`` reports ``gate``/``plugin``/``signed``/
    ``error``/``body_limit`` (cluster C); the doctor line says which layer answered and what to do.

    Red proof: mutant = ``peer_check`` reads ``probe["ok"]`` like layer 1 (``ok = bool(probe.get("ok"))`` for a
    signed answer) → ``AssertionError: {'name': 'peer server', 'ok': False, 'detail': 'dashboard gate passed, …;
    request bodies of 8 MiB pass', 'remedy': None}``.
    """
    check = commands.peer_check(make_peer("server"), probe, skew)
    assert check["name"] == "peer server" and check["ok"] is ok, check
    assert detail in check["detail"], check
    if remedy is None:
        assert not check.get("remedy")
    else:
        assert remedy in (check.get("remedy") or ""), check


def test_doctor_uses_c_gate_warnings(fake_host, monkeypatch):
    """W066: the credential and ``.env`` warnings are cluster C's ``wire.gate_warnings`` lines."""
    from ctx_handover.engine import wire

    calls = []
    real = wire.gate_warnings

    def spy(gate, *, env_mode):
        calls.append((getattr(gate, "mode", None), env_mode))
        return real(gate, env_mode=env_mode)

    monkeypatch.setattr(wire, "gate_warnings", spy)
    fake_host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "tok"
    checks = {c["name"]: c for c in commands.local_checks(FakeCtx(host=fake_host), FakeBackend())}
    assert "the stored dashboard token opens the whole dashboard" in checks["stored dashboard token"]["detail"]
    assert [c[0].value for c in calls if c[0] is not None] == ["session_token"]


# ---------------------------------------------------------------------------------------------------------
# W065/W119 — identity rotation through Identity.create(rotate=True)
# ---------------------------------------------------------------------------------------------------------


def test_identity_rotate_creates_a_new_key(fake_host, tmp_path):
    """W065/W119: ``/handover identity rotate`` calls ``crypto.Identity.create(…, rotate=True)``: a new key in the
    profile ``.env`` and the ledger, and the profile's cached identity is the new one (the runner and the pull
    client sign with it from now on).

    Red proof: mutant = ``rotate=False`` → ``assert '6f917c8554002e29' != '6f917c8554002e29'`` (the old key stays;
    the fingerprint differs per run).
    """
    from ctx_handover.engine import crypto, hostcompat
    from ctx_handover.engine.ledger import Ledger

    ledger = Ledger(tmp_path / "ledger.sqlite3")
    try:
        old = crypto.Identity.create(
            ledger,
            env_writable=hostcompat.env_writable,
            save_env=hostcompat.save_env,
            read_env_fresh=hostcompat.read_env_fresh,
        )
        runtime = types.SimpleNamespace(ledger=ledger, identity=old, identity_error=None, lock=threading.RLock())
        holder: dict = {}
        plugin = Plugin(
            FakeCtx(host=fake_host), backend_factory=lambda c, p: holder.setdefault("eb", EngineBackend(c, p))
        )
        plugin.backend._parts["runtime"] = runtime
        out = _slash(plugin, "identity rotate")
        new_fp = ledger.meta_get(crypto.META_FP)
        assert old.fingerprint() != new_fp
        assert f"New key {new_fp}" in out and runtime.identity.fingerprint() == new_fp
        assert fake_host.env_file[crypto.IDENTITY_ENV]
        loaded = crypto.Identity.load(
            ledger, read_secret=hostcompat.read_secret, read_env_fresh=hostcompat.read_env_fresh
        )
        assert loaded is not None and loaded.fingerprint() == new_fp
    finally:
        ledger.close()


# ---------------------------------------------------------------------------------------------------------
# W067/W047 — revoke and reclaim through jobs
# ---------------------------------------------------------------------------------------------------------


def test_revoke_and_reclaim_go_through_jobs(fake_host, monkeypatch):
    """W067: ``peers revoke`` is ``jobs.revoke_peer`` (ledger cascade + abort of the peer's participant jobs);
    W047: ``reclaim`` is ``jobs.reclaim`` (lock.reclaim + ``source_reclaimed`` to the target), and the person is
    told when the target was reached.

    Red proof: mutant = ``EngineBackend.revoke_peer`` calls ``ledger.revoke_peer`` (layer 1) →
    ``assert [] == ['laptop']``.
    """
    revoked, reclaimed = [], []
    _stub_module(
        monkeypatch,
        "jobs",
        revoke_peer=lambda name: revoked.append(name) or ["01JOBA", "01JOBB"],
        reclaim=lambda sid, *, force: (
            reclaimed.append((sid, force))
            or {
                "state": "reclaimed",
                "job_id": "01JOBA",
                "message": "Reclaimed.",
                "warnings": [],
                "target_notified": True,
            }
        ),
    )

    class Ledger:
        def peers(self, *, include_revoked=False):
            return [make_peer("server"), make_peer("laptop", direction="inbound")]

        def peer(self, name):
            return next((p for p in self.peers() if p.name == name), None)

        def revoke_peer(self, name):  # must not be used directly
            raise AssertionError("ledger.revoke_peer called directly")

    holder: dict = {}
    plugin = Plugin(FakeCtx(host=fake_host), backend_factory=lambda c, p: holder.setdefault("eb", EngineBackend(c, p)))
    plugin.backend._parts["runtime"] = types.SimpleNamespace(ledger=Ledger(), store=None, veto=None)
    out = _slash(plugin, "peers revoke laptop")
    assert revoked == ["laptop"] and "2 unfinished handovers" in out
    out = _slash(plugin, "reclaim --force")
    assert reclaimed == [(SID, True)]
    assert "Reclaimed." in out and "The other gateway was told" in out


# ---------------------------------------------------------------------------------------------------------
# The profile runtime: one ledger, store, veto index and runner per profile (item 9, W026, W109)
# ---------------------------------------------------------------------------------------------------------


def test_backend_uses_the_profile_runtime(fake_host):
    """W109/W026: the backend uses ``jobs.ensure_started``'s profile — its ledger, block store and the one
    ``VetoIndex`` (no second one) — and ``jobs.runner_for``'s runner, which reports to the surfaces' broker even
    when a hook started the profile first (in Hermes a hook fires before any command).

    Red proof: mutant = ``_started`` without the ``jobs.runner_for(…, broker.factory)`` line →
    ``assert <…_NullReporter object> is <…Bound object>`` (ensure_started's janitor built the runner without a
    reporter factory, so no progress notice and no final message would ever appear).
    """
    from ctx_handover.engine import jobs, lock

    built = []
    real_init = lock.VetoIndex.__init__

    def counting(self, *a, **kw):
        built.append(self)
        real_init(self, *a, **kw)

    jobs._reset_for_tests()
    prof = None
    try:
        with pytest.MonkeyPatch.context() as m:
            m.setattr(lock.VetoIndex, "__init__", counting)
            plugin = Plugin(FakeCtx(host=fake_host))
            assert plugin.hooks.pre_llm_call(session_id="20261109_000000_000000") is None  # starts the profile
            prof = jobs._profile()
            backend = plugin.backend
            assert backend._started() is prof
            assert backend._ledger() is prof.ledger and backend._store() is prof.store
            assert backend.hook_runtime()._veto is prof.veto
            assert backend._runner() is prof.runner
            assert len(built) == 1

            class Bound:
                def progress(self, p):
                    pass

            bound = Bound()
            with plugin.broker.bind(bound):
                assert prof.runner._reporter_factory("01JC0000000000000000BOUND1") is bound
    finally:
        if prof is not None and prof.veto is not None:
            prof.veto.stop()
        jobs._reset_for_tests()
