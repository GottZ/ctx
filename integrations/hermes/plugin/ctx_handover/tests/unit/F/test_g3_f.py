"""Round G‴ of cluster F (``reports/L2/pkg/G3.md`` package g3-df, lab findings of the E2E suite):

* L3-F17 / R32 — a session larger than the target model's context window: the surfaces show it (pre-send line,
  question, plan) and ``--yes`` does not confirm it; without a person nothing is locked;
* L3-F15 — the printed short job id (``/handover status HRMT9Z``) finds the job; unique starts and ends of an id
  too; several matches ⇒ a clear message;
* L3-F14 — refusals are machine-readable: a refused ``reclaim`` exits ≠ 0 with ``ok: false`` and ``error.code``; a
  job the runner ended (``session_busy``) carries its code into ``--json``;
* L17 — the final message names title, session id and the exact way to open the received session (g3-b: it is
  stored, not open — Desktop sidebar, ``hermes sessions list``, ``hermes --resume <id>``);
* L15 / R33 — under ``plugins.isolation: host`` a gateway cannot receive or provide handovers over HTTP: ``doctor``
  says so (FAIL when paired devices dial it), and so does the ``listener_missing`` text.

Every test reproduces its finding without Hermes through the real surfaces (``Plugin.slash``/``Plugin.cli``) and
``FakeBackend``; the engine-backed parts run against a real ``Ledger``.
"""

from __future__ import annotations

import argparse
import json
import threading
import types

import pytest

from ctx_handover.engine import commands, surfaces
from ctx_handover.engine.commands import EngineBackend, Plugin
from ctx_handover.engine.errors import BadRequest, ConflictError, ListenerMissing
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Role
from ctx_handover.tests.conftest import PLUGIN_ROOT
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer

from .test_commands import SID, _plugin, _slash

OVER = {"tokens": 40_000, "context_length": 4000, "model": "fake-model"}
CONTEXT = "context_over"  # D's reason (i), ``plan.CONFIRM_CONTEXT`` (a literal here: the red run predates the name)
JOB_A = "01M6ZYWJJ1M8MMDP2XJJHRMT9Z"  # ends in the lab's printed short id HRMT9Z (L3-F15)
JOB_B = "01M700X3SKS483VVTZ8Y4PXCW5"
JOB_C = "01M700X3SKS483VVTZ8Y4QQQQQ"


def _cli(plugin: Plugin, *argv: str) -> int:
    return plugin.cli(argparse.Namespace(verb=argv[0], rest=list(argv[1:])))


def _json(capsys) -> dict:
    return json.loads(capsys.readouterr().out.strip().splitlines()[-1])


# ---------------------------------------------------------------------------------------------------------
# L3-F17 / R32: larger than the target model's window
# ---------------------------------------------------------------------------------------------------------


def _over_backend() -> FakeBackend:
    backend = FakeBackend(peers=[make_peer("server"), make_peer("laptop", direction="inbound")])
    backend.confirm_reasons = [CONTEXT]
    backend.confirm_even_with_yes = True  # the conductor asks despite --yes for D's YES_EXEMPT (conductor._confirm)
    backend.summary_extra = {"context_over": dict(OVER)}
    return backend


def test_yes_does_not_confirm_a_session_larger_than_the_target_window(capsys):
    """★ L3-F17 / R32 (lab L24: `take`/`to` without a person): ``--yes`` does not confirm ``context_over``; without
    an answer the handover is cancelled before the lock (exit 1), the text names size, window and model and offers
    ``/compress``; ``--json`` carries ``error.code == "cancelled"`` with the refused reason.

    Before G‴ the reason did not exist: ``--yes`` answered "yes" and the handover went on (exit 0)."""
    plugin, backend, _ = _plugin(_over_backend())
    code = _cli(plugin, "to", "server", "--session", SID, "--yes", "--json")
    payload = _json(capsys)
    assert code == commands.EXIT_FAILED and payload["ok"] is False, payload
    assert "run_foreground" not in backend.names() and "continue_async" not in backend.names()
    text = payload["text"]
    assert (
        "--yes does not confirm that the session (about 40,000 tokens) is larger than the context window of " in (text)
        and "fake-model there (4,000 tokens)" in text
        and "/compress" in text
    ), text
    assert payload["error"]["code"] == "cancelled"
    assert payload["error"]["details"]["yes_refused"] == [CONTEXT]


def test_the_question_and_the_presend_line_name_the_window():
    """★ R32: a person is asked (slash, ``ask_choice``) with the window in the question and the reason text; a
    ``yes`` goes ahead (no abort, R32) and the pre-send notice names the window too — read from the conductor's
    journaled ``presend`` (computed with both sides' facts)."""
    backend = _over_backend()
    plugin, backend, ctx = _plugin(backend)
    ctx.choice_answers = ["yes"]
    backend.presend_by_job["01JC00000000000000000JOB01"] = {"context_over": dict(OVER)}
    out = _slash(plugin, "to server --yes")
    question = ctx.choices_asked[-1][0] if ctx.choices_asked else ""
    assert "larger than the context window of fake-model there (4,000 tokens)" in question, question
    assert surfaces.REASON_TEXT[CONTEXT] in question
    assert backend.confirm_answers == ["yes"] and backend.names()[-1] == "continue_async", out
    assert backend.last("presend_summary") == {"job_id": "01JC00000000000000000JOB01"}
    summary = backend.presend_summary(backend.final, job_id="01JC00000000000000000JOB01")
    presend = surfaces.render_presend(summary, "x", "push")
    assert "larger than the context window of fake-model there (4,000 tokens)" in presend, presend


@pytest.mark.parametrize(
    "value",
    [
        None,
        {"tokens": 4000, "context_length": 4000, "model": "m"},  # not larger
        {"tokens": True, "context_length": 1, "model": "m"},
        {"tokens": 10, "context_length": 0},
        {"tokens": "40000", "context_length": 4000},
        "40000 tokens",
    ],
)
def test_the_surface_invents_no_window(value):
    """R32: only D's form (positive integers, size > window) is shown; anything else shows nothing."""
    summary = surfaces.plan_summary(None, {"context_over": value})
    assert summary["context_over"] is None
    assert "context window" not in surfaces.render_presend(summary, "server", "push")
    assert "context window" not in surfaces.render_plan(summary)
    assert "context window" not in surfaces.confirm_question(summary, "server")


def test_plan_and_question_texts_name_the_window():
    summary = surfaces.plan_summary(None, {"context_over": dict(OVER)})
    assert "Context: the session (about 40,000 tokens) is larger than the context window of fake-model there" in (
        surfaces.render_plan(summary)
    )
    assert "The session (about 40,000 tokens) is larger" in surfaces.confirm_question(summary, "server")
    assert surfaces.CONFIRM_CONTEXT in surfaces.YES_EXEMPT and surfaces.REASONS["i"] == surfaces.CONFIRM_CONTEXT
    # reason (e) no longer speaks of the window
    assert "context" not in surfaces.REASON_TEXT["model"]


def test_engine_backend_reads_the_journaled_presend(fake_host, tmp_path):
    """R32: ``EngineBackend.presend_summary(final, job_id=…)`` renders the conductor's journaled ``presend`` (the one
    computed with the target facts), not a recomputation from the plan alone."""
    eb = _engine_backend(fake_host, tmp_path)
    ledger = eb._parts["runtime"].ledger
    ledger.create_job(JOB_A, Role.CONDUCTOR, Mode.PUSH, None, {})
    ledger.event(JOB_A, Role.CONDUCTOR, "transition", {"ctx": {"presend": {"context_over": dict(OVER)}}}, epoch=0)
    from ctx_handover.tests.fakes.surfaces_fake import sample_final_plan

    final = sample_final_plan()
    assert eb.presend_summary(final, job_id=JOB_A)["context_over"] == OVER
    assert eb.presend_summary(final)["context_over"] is None
    assert eb.presend_summary(final, job_id=JOB_B)["context_over"] is None


# ---------------------------------------------------------------------------------------------------------
# L3-F15: the printed short id finds the job
# ---------------------------------------------------------------------------------------------------------


def _with_jobs() -> tuple[Plugin, FakeBackend, FakeCtx]:
    plugin, backend, ctx = _plugin()
    for jid in (JOB_A, JOB_B, JOB_C):
        backend.statuses[jid] = {"job_id": jid, "mode": "pull", "state": "done", "peer": "server"}
    return plugin, backend, ctx


def test_the_printed_short_id_finds_the_job():
    """★ L3-F15: ``/handover status HRMT9Z`` — the short id every progress, final and error text prints
    (``surfaces.short_job``) — shows that job; lower case too; so do ``cancel``, ``resume``, ``logs`` and
    ``gc --job``. Before G‴: ``job HRMT9Z not found`` (exact lookup only)."""
    plugin, backend, _ = _with_jobs()
    short = surfaces.short_job(JOB_A)
    assert short == "HRMT9Z"
    out = _slash(plugin, f"status {short}")
    assert backend.last("job_status") == {"job_id": JOB_A} and "HRMT9Z · pull · server · done" in out, out
    _slash(plugin, "status hrmt9z")
    assert backend.last("job_status") == {"job_id": JOB_A}
    _slash(plugin, f"cancel {short}")
    assert backend.last("cancel") == {"job_id": JOB_A}
    _slash(plugin, f"resume {short}")
    assert backend.last("resume") == {"job_id": JOB_A}
    _slash(plugin, f"logs {short}")
    assert backend.last("logs") == {"job_id": JOB_A}
    _slash(plugin, f"gc --job {short}")
    assert backend.last("gc") == {"job_id": JOB_A, "all_jobs": False}


def test_unique_starts_and_ambiguity():
    """L3-F15: a unique start of an id resolves as well; a token several jobs start or end with is refused with
    the candidates named (a messaging platform gets their count only); a token no job matches, or one shorter than
    four characters, reaches the engine unchanged ("not found" as before)."""
    plugin, backend, _ = _with_jobs()
    _slash(plugin, "status 01M6ZY")
    assert backend.last("job_status") == {"job_id": JOB_A}
    out = _slash(plugin, "status 01M700")
    assert JOB_B in out and JOB_C in out and "several handovers" in out, out
    with pytest.raises(BadRequest) as info:
        surfaces.match_job("01M700", [JOB_A, JOB_B, JOB_C], listed=False)
    assert JOB_B not in info.value.message and "2 handovers" in info.value.message
    assert surfaces.match_job("NOPE42", [JOB_A, JOB_B]) == "NOPE42"
    assert surfaces.match_job("9Z", [JOB_A]) == "9Z"
    assert surfaces.match_job(JOB_A.lower(), [JOB_A]) == JOB_A
    assert surfaces.match_job("HRM-9Z", [JOB_A]) == "HRM-9Z"


def test_engine_backend_lists_every_job_of_every_role(fake_host, tmp_path):
    """L3-F15: ``EngineBackend.job_ids`` names the jobs of every role (a push that arrived here is a ``target`` row),
    so a short id printed on either side resolves."""
    eb = _engine_backend(fake_host, tmp_path)
    ledger = eb._parts["runtime"].ledger
    ledger.create_job(JOB_A, Role.CONDUCTOR, Mode.PULL, None, {})
    ledger.create_job(JOB_B, Role.TARGET, Mode.PUSH, None, {})
    ledger.create_job(JOB_B, Role.SOURCE, Mode.PUSH, None, {})
    assert sorted(eb.job_ids()) == sorted([JOB_A, JOB_B])


# ---------------------------------------------------------------------------------------------------------
# L3-F14: refusals are machine-readable
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("result", "code", "error"),
    [
        (
            {
                "state": None,
                "ok": False,
                "message": "A handover is being committed. Wait, or use --force.",
                "error": {"code": "invalid_state", "message": "committing", "details": {"reason": "committing"}},
            },
            commands.EXIT_BUSY,
            "invalid_state",
        ),
        (
            {"state": None, "ok": False, "message": "Nothing to reclaim.", "error": {"code": "not_found"}},
            commands.EXIT_USAGE,
            "not_found",
        ),
        ({"state": None, "message": "A handover is being committed."}, commands.EXIT_FAILED, "not_reclaimed"),
        ({"state": "reclaimed", "ok": "yes"}, commands.EXIT_FAILED, "not_reclaimed"),  # only True is a success
        ({"state": "reclaimed", "ok": True, "message": "The session is active here again."}, commands.EXIT_OK, None),
        ({"state": "released"}, commands.EXIT_OK, None),
    ],
)
def test_refused_reclaim_exits_non_zero_with_a_code(capsys, result, code, error):
    """★ L3-F14: ``hermes handover reclaim`` without ``--force`` during a commit answered exit 0 / ``ok: true``.
    Now success only on cluster A's ``ok is True`` (or, without ``ok``, a session active here again); otherwise the
    exit code of the error code (DESIGN §14.2) and ``error`` in ``--json``."""
    plugin, backend, _ = _plugin()
    backend.reclaim_result = dict(result)
    assert _cli(plugin, "reclaim", SID, "--json") == code
    payload = _json(capsys)
    assert payload["ok"] is (code == commands.EXIT_OK) and payload["exit_code"] == code
    assert (payload.get("error") or {}).get("code") == error, payload
    if result.get("message"):
        assert result["message"] in payload["text"]


def test_a_job_the_runner_ended_carries_its_error_code(capsys):
    """★ L3-F14 (lab E12): ``take`` on a busy source ended with exit 3 and the right text, but ``--json`` had no
    ``error.code``: the runner, not the command, ended the job. Now the job's ``last_error`` is the ``error``, and
    the state is reported."""
    plugin, backend, _ = _plugin()
    job = "01JC00000000000000000JOB01"
    backend.statuses[job] = {
        "job_id": job,
        "mode": "pull",
        "state": "aborted",
        "last_error": {"code": "session_busy", "message": "a turn is running", "details": {"session": "s"}},
    }
    backend.foreground_code = commands.EXIT_BUSY
    assert _cli(plugin, "take", "server", "--latest", "--yes", "--json") == commands.EXIT_BUSY
    payload = _json(capsys)
    assert payload["ok"] is False and payload["error"]["code"] == "session_busy", payload
    assert payload["state"] == "aborted"
    # a finished job keeps exit 0 and carries no error
    backend.statuses["01JC00000000000000000JOB02"] = {"job_id": "x", "mode": "pull", "state": "done"}
    backend.foreground_code = 0
    assert _cli(plugin, "take", "server", "--latest", "--yes", "--json") == commands.EXIT_OK
    payload = _json(capsys)
    assert payload["ok"] is True and "error" not in payload and payload["state"] == "done"
    # exit 0 for a job whose record is not finished is not a success
    backend.statuses["01JC00000000000000000JOB03"] = {"job_id": "y", "mode": "pull", "state": "packing"}
    assert _cli(plugin, "take", "server", "--latest", "--yes", "--json") == commands.EXIT_FAILED


# ---------------------------------------------------------------------------------------------------------
# L17: how to open the received session
# ---------------------------------------------------------------------------------------------------------


def test_final_message_names_title_id_and_how_to_open_it(capsys):
    """★ L17: a received session is stored, not open — a gateway's list of open sessions (``session.list``
    without ``scope: stored``) misses it until the first open (g3-b, upstream ``gateway/session_controls.py:584``).
    The final message names title and session id and says exactly how to open it: Desktop sidebar,
    ``hermes sessions list``, ``hermes --resume <id>``. A push says it for the other gateway. Before G‴: "Open it
    from the session list and send a message"."""
    plugin, backend, _ = _plugin()
    job = "01JC00000000000000000JOB01"
    backend.statuses[job] = {
        "job_id": job,
        "mode": "pull",
        "state": "done",
        "target_session": {"id": "20261109_112434_29a74a", "title": "Fix the login bug"},
    }
    assert _cli(plugin, "take", "server", "--latest", "--yes") == commands.EXIT_OK
    out = capsys.readouterr().out
    for needle in (
        '"Fix the login bug"',
        "20261109_112434_29a74a",
        "Desktop sidebar",
        "`hermes sessions list`",
        "`hermes --resume 20261109_112434_29a74a`",
    ):
        assert needle in out, (needle, out)
    push = surfaces.render_final(
        Mode.PUSH, None, "done", "laptop", None, target_session={"id": "20261109_1_x", "title": "T"}
    )
    assert 'On laptop: The session "T" (20261109_1_x) is stored but not open yet' in push
    assert "`hermes --resume 20261109_1_x`" in push
    bare = surfaces.render_final(Mode.PULL, None, "done", "server", None, target_session={"id": "", "title": "T"})
    assert "--resume" not in bare and "session ?" not in bare


def test_status_names_the_session_the_job_created():
    """★ L17 (g3-a's ``target_session`` in every status entry): ``/handover status`` names the session a pull made
    here, or a push made on the other gateway, and how to open it; an entry without a usable id names nothing.
    Before G‴ the status line of a conductor job named no session."""
    plugin, backend, _ = _plugin()
    backend.jobs = [
        {
            "job_id": JOB_A,
            "mode": "pull",
            "peer": "server",
            "state": "done",
            "role": "conductor",
            "target_session": {"id": "20261109_112434_29a74a", "title": "Fix the login bug"},
        },
        {
            "job_id": JOB_B,
            "mode": "push",
            "peer": "laptop",
            "state": "done",
            "role": "conductor",
            "target_session": {"id": "20261109_120000_bbbbbb", "title": None},
        },
        {
            "job_id": JOB_C,
            "mode": "push",
            "peer": "laptop",
            "state": "aborted",
            "role": "conductor",
            "target_session": {"id": "", "title": "x"},
        },
    ]
    out = _slash(plugin, "status")
    assert (
        "session here: 20261109_112434_29a74a (Fix the login bug) (open it with "
        "`hermes --resume 20261109_112434_29a74a`)" in out
    ), out
    assert "session on laptop: 20261109_120000_bbbbbb (open it with" in out, out
    assert out.count("hermes --resume") == 2


# ---------------------------------------------------------------------------------------------------------
# L15 / R33: host isolation
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("settings", "peers", "ok"),
    [
        ({"inbound": "auto"}, [make_peer("laptop", direction="inbound")], False),  # a device dials in: unreachable
        ({"inbound": "on"}, [], False),
        ({"inbound": "auto"}, [make_peer("server")], None),  # only dials out: works, warn
        ({"inbound": "off"}, [make_peer("laptop", direction="inbound")], None),
    ],
)
def test_doctor_names_the_host_isolation_limit(settings, peers, ok):
    """★ L15 / R33: under ``plugins.isolation: host`` (config, or this process is the plugin host) ``doctor`` says
    that this gateway cannot receive or provide handovers over HTTP (``in_process`` needed) and that dialing out and
    bundles work; FAIL when paired devices are expected to reach it. Before G‴ the line said only "no CLI, no
    progress notices" — and only inside the plugin host."""
    config = {"plugins": {"isolation": "host", "entries": {"ctx_handover": {"settings": dict(settings)}}}}
    plugin, _backend, _ = _plugin(FakeBackend(peers=peers, config=config))
    out = _slash(plugin, "doctor")
    line = next((x for x in out.splitlines() if "plugin isolation" in x), "")
    assert "cannot receive or provide handovers over HTTP" in line and "in_process" in line, out
    assert "bundles" in line and "dialing" in line
    assert line.startswith("[FAIL]" if ok is False else "[warn]"), line
    quiet = FakeBackend(peers=peers, config={"plugins": {"isolation": "in_process"}})
    plugin, _b, _ = _plugin(quiet)
    assert "plugin isolation" not in _slash(plugin, "doctor")


def test_missing_routes_text_names_host_isolation():
    """L15 / R33: the ``listener_missing`` text (the lab's L15 cause) names host isolation and the bundle path."""
    text = surfaces.render_error(ListenerMissing("no routes"), "server")
    assert "plugins.isolation: host" in text and "in_process" in text and "/handover export" in text


def test_partial_in_the_way_names_the_path():
    """Integrator wish (g3-e's ``ConflictError(kind="partial_exists")``, L3-F08): the text names the path."""
    err = ConflictError("x", kind="partial_exists", path="/home/u/hermes-handover/.ctxh-abc123")
    text = surfaces.render_error(err, "laptop")
    assert "/home/u/hermes-handover/.ctxh-abc123" in text and "Move or remove it" in text


# ---------------------------------------------------------------------------------------------------------
# README
# ---------------------------------------------------------------------------------------------------------


def test_readme_names_the_g3_limits_and_answers():
    """The README says what the surfaces say: the window question ``--yes`` does not answer (R32), how to open a
    received session (L17), host isolation (R33), short job ids (L3-F15) and the reclaim exit code (L3-F14)."""
    text = " ".join((PLUGIN_ROOT / "README.md").read_text(encoding="utf-8").split())
    for needle in (
        "larger than the context window",
        "/compress",
        "`hermes --resume <session-id>`",
        "Desktop sidebar",
        "`hermes sessions list`",
        "receive or provide handovers over HTTP",
        "`in_process`",
        "bundles",
        "six-character job id",
        "exits 3",
        "keeps the answer it already got",
    ):
        assert needle in text, needle


# ---------------------------------------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------------------------------------


def _engine_backend(fake_host, tmp_path) -> EngineBackend:
    ctx = FakeCtx(host=fake_host)
    eb = EngineBackend(ctx, Plugin(ctx))
    runtime = types.SimpleNamespace(
        ledger=Ledger(tmp_path / "ledger.sqlite3"),
        store=None,
        veto=None,
        identity=None,
        identity_error=None,
        lock=threading.RLock(),
    )
    eb._parts.update({"runtime": runtime})
    return eb
