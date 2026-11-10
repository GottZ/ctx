"""Round G″ of cluster F:

1. ``reclaim --session <id>`` is accepted by the one grammar (slash and ``hermes handover``) — the remedy the README
   names for a session the CLI cannot point at otherwise (L3-F06);
2. a ``policy_denied`` with ``details.reason == "workspace_root"`` (R29, V40: the receiving gateway's write root is
   unusable) reads as such — it names the setting ``workspace_root`` and the folder, not "items are not allowed …
   use --dest";
3. the confirmation callback honours the conductor's ``info["yes_exempt"]``: a reason it names is never confirmed by
   ``--yes`` — a person answers, or the job ends before the lock (R30);
4. the status line of a job another gateway conducts names this gateway's role (``source``/``target``) and the
   other side (V43, L3-F04).
"""

from __future__ import annotations

import argparse

import pytest

from ctx_handover.engine import commands, surfaces
from ctx_handover.engine.commands import Call, _ConfirmState
from ctx_handover.engine.errors import InsufficientStorage, PolicyDenied
from ctx_handover.engine.model import Options
from ctx_handover.tests.conftest import PLUGIN_ROOT
from ctx_handover.tests.fakes.surfaces_fake import sample_final_plan

from .test_commands import SID, _plugin, _slash

ROOT = "/opt/data/hermes-handover"
JOB = "01JC0000000000000000000JOB"


# ---------------------------------------------------------------------------------------------------------
# 1. reclaim --session <id> (slash and CLI)
# ---------------------------------------------------------------------------------------------------------


def test_reclaim_accepts_the_session_as_an_option():
    """★ (L3-F06): ``reclaim --session <id>`` parses like ``reclaim <id>``; ``--force`` still works with
    it; the same id given both ways is fine, two different ids are a usage error.

    Red (on 16698398): ``assert ("--session is not an option of 'reclaim'. Usage: …",) == ()``.
    """
    parsed = surfaces.parse("reclaim --session S1 --force")
    assert parsed.errors == ()
    assert parsed.args["session"] == "S1" and parsed.args["force"] is True
    assert surfaces.parse("reclaim S1").args["session"] == "S1"
    assert surfaces.parse("reclaim S1 --session S1").errors == ()
    clash = surfaces.parse("reclaim S1 --session S2")
    assert clash.errors and "S1" in clash.errors[0] and "S2" in clash.errors[0], clash.errors
    assert "--session" in surfaces.USAGE["reclaim"]


def test_reclaim_session_option_reaches_the_backend_from_slash_and_cli(capsys):
    """★ the slash form and ``hermes handover reclaim --session <id>`` (argparse in front, as Hermes
    builds it with ``commands.cli_setup``) reclaim exactly the named session; a clash reclaims nothing (exit 2).

    Red (on 16698398): the slash answered the usage text (no ``reclaim`` call) — ``assert 'reclaim' in [...]``.
    """
    plugin, backend, _ctx = _plugin()
    _slash(plugin, "reclaim --session 20261109_090000_bbbbbb")
    assert backend.last("reclaim") == {"session_id": "20261109_090000_bbbbbb", "force": False}

    parser = argparse.ArgumentParser(prog="hermes handover")
    commands.cli_setup(parser)
    ns = parser.parse_args(["reclaim", "--session", "20261109_090000_cccccc", "--force"])
    assert plugin.cli(ns) == commands.EXIT_OK
    assert backend.last("reclaim") == {"session_id": "20261109_090000_cccccc", "force": True}

    calls = len(backend.calls)
    ns = parser.parse_args(["reclaim", "20261109_090000_cccccc", "--session", "20261109_090000_dddddd"])
    assert plugin.cli(ns) == commands.EXIT_USAGE
    assert len(backend.calls) == calls
    capsys.readouterr()


# ---------------------------------------------------------------------------------------------------------
# 2. policy_denied for the receiving gateway's workspace root (R29)
# ---------------------------------------------------------------------------------------------------------


def _root_denied(message: str) -> PolicyDenied:
    """The error ``Conductor._check_workspace_root`` raises in FACTS (``conductor.py`` ``PolicyDenied(message,
    reason="workspace_root", setting="workspace_root", problem, path, side, host)``)."""
    return PolicyDenied(
        message,
        reason="workspace_root",
        setting="workspace_root",
        problem="denied",
        path=ROOT,
        side="target",
        host="gottz",
    )


@pytest.mark.parametrize(
    "message",
    [
        f"the folder {ROOT} on gottz, where handed-over project folders go (setting workspace_root), lies in a "
        "protected place (such as the Hermes home) where the plugin never writes; choose another folder with "
        "workspace_root (and allow_roots) in the ctx_handover settings on gottz",
        "",
        "refused",
    ],
    ids=["d-text", "empty", "bare"],
)
def test_workspace_root_denial_names_the_setting_and_the_folder(message):
    """★ (R29/V40): the text names ``workspace_root``, the folder and the gateway, says that nothing was
    locked or sent, and does not send the user to ``--dest``/"items" (a root problem is not an item problem) — with
    D's sentence, without any message, and with a message that names neither.

    Red (on 16698398): ``AssertionError: assert '1 items are not allowed on gottz: … Adjust allow_roots there or use
    --dest.'`` (``'items are not allowed' not in text``).
    """
    text = surfaces.render_error(_root_denied(message), "gottz")
    assert "items are not allowed" not in text and "--dest" not in text, text
    assert "workspace_root" in text and ROOT in text and "gottz" in text, text
    assert "Nothing was locked or sent" in text, text
    assert text[0].isupper(), text


def test_workspace_root_denial_reads_the_same_from_the_jobs_record():
    """★ The same text when it comes from the job's ``last_error`` (closing text, ``/handover status``) — the route
    ``_job_error_text`` rebuilds the error class from ``{code, message, details}``; other ``policy_denied`` errors
    keep their item text.

    Red (on 16698398): the item text ``… are not allowed on gottz …``.
    """
    err = _root_denied("")
    record = {"code": err.code, "message": err.message, "details": dict(err.details)}
    text = commands._job_error_text(JOB, record, "gottz")
    assert "workspace_root" in text and ROOT in text and "--dest" not in text, text
    items = PolicyDenied("denied", items=["/etc/x"], count=1)
    assert "items may not be written on gottz: /etc/x" in surfaces.render_error(items, "gottz")


def test_workspace_root_denial_from_the_planning_step_reaches_the_slash():
    """★ ``/handover to`` whose job ends in FACTS with the R29 error: the person reads the root text; exit 1."""
    plugin, backend, _ctx = _plugin()
    backend.start_error = _root_denied("")
    out = _slash(plugin, "to server --yes")
    assert "workspace_root" in out and ROOT in out and "items are not allowed" not in out, out
    assert "continue_async" not in backend.names()


def test_full_disk_of_this_machine_without_a_known_need_reads_as_such():
    """★ Success sweep (``render_error`` ``insufficient_storage``): the error ``bundle.write_error`` raises for a full
    disk while an export is written may not know how much more it needs (``need_bytes`` 0 before a plan exists) — the
    text then says the folder ran out of space instead of "needs 0 B free"; with a need the text is unchanged.

    Red (mutant M17, branch removed): ``assert 'ran out of space' in 'This machine needs 0 B free in /srv/out …'``.
    """
    full = InsufficientStorage("x", need_bytes=0, free_bytes=0, path="/srv/out", side="This machine")
    text = surfaces.render_error(full, None)
    assert "ran out of disk space in /srv/out" in text and "needs 0" not in text and text.startswith("This machine"), (
        text
    )
    need = InsufficientStorage("x", need_bytes=2048, free_bytes=1024, path="/srv/out", side="This machine")
    assert surfaces.render_error(need, None).startswith("This machine needs 2.0 KiB of free disk space in /srv/out")


# ---------------------------------------------------------------------------------------------------------
# 3. info["yes_exempt"] — --yes never answers what the conductor names
# ---------------------------------------------------------------------------------------------------------


def _confirm(options: Options, info: dict, asker) -> tuple[str, _ConfirmState]:
    plugin, _backend, _ctx = _plugin()
    state = _ConfirmState()
    call = Call(platform="local", session_id=SID, session_key=None, asker=asker)
    return plugin._confirm_fn(call, "gottz", options, state)(info), state


def _info(reasons, yes_exempt) -> dict:
    info = {
        "reasons": list(reasons),
        "summary": None,
        "final_plan": sample_final_plan(),
        "suspected_secrets": [],
        "instruction_files": [],
    }
    if yes_exempt is not ...:
        info["yes_exempt"] = yes_exempt
    return info


def test_yes_does_not_answer_a_reason_the_conductor_marks_yes_exempt():
    """★ ``conductor._confirm`` calls the callback despite ``--yes`` exactly for the reasons in
    ``info["yes_exempt"]``; the surface asks the person for them even when its own table (``plan.YES_EXEMPT``) does
    not list the code — the conductor's list wins. Without a person: ``cancel`` (never ``yes``).

    Red (on 16698398): ``assert 'yes' == 'cancel'`` / no question asked — ``--yes`` answered a reason the conductor
    had exempted (the callback read only its own table).
    """
    asked: list[str] = []

    def asker(question, choices):
        asked.append(question)
        return "no_proposed"

    answer, state = _confirm(Options(yes=True), _info(["large"], ["large"]), asker)
    assert asked and answer == "no_proposed", (asked, answer)
    assert state.yes_refused == ["large"]

    answer, state = _confirm(Options(yes=True), _info(["large"], ["large"]), None)
    assert answer == "cancel" and state.timed_out is True and state.yes_refused == ["large"], (answer, state)


@pytest.mark.parametrize(
    ("yes_exempt", "asks"),
    [
        (..., False),
        ([], False),
        (None, False),
        ("large", True),
        (["c"], True),
        (True, True),
        ({"bogus": 1}, True),
    ],
    ids=["absent", "empty", "none", "bare-string", "letter", "true", "mapping"],
)
def test_yes_exempt_counts_only_an_explicitly_empty_value_as_none(yes_exempt, asks):
    """★ Success sweep: ``--yes`` answers on its own only when the conductor's ``yes_exempt`` is absent,
    ``None`` or an empty list (and the surface's own table names nothing); a bare string or a letter names a reason;
    any other value present counts as "ask" (fail-closed), never as "nothing exempt".

    Red (on 16698398): ``"large"``/``True``/mapping were ignored ⇒ ``'yes'``.
    """
    answer, _state = _confirm(Options(yes=True), _info(["large"], yes_exempt), None)
    assert (answer != "yes") is asks, answer
    if asks:
        assert answer == "cancel"


def test_silence_after_a_refused_yes_cancels_even_when_only_proposals_are_asked():
    """★ R30: when ``--yes`` was refused for a reason the conductor names and nobody answers, the answer is
    ``cancel`` (nothing is locked) — also when that reason is (a) alone, where silence without ``--yes`` would mean
    "without the proposed items" (BX L5); without ``--yes`` that rule is unchanged.

    Red (mutant M05, ``choice_on_timeout`` for every silence): ``assert 'no_proposed' == 'cancel'``.
    """
    answer, state = _confirm(Options(yes=True), _info(["proposed"], ["proposed"]), None)
    assert answer == "cancel" and state.yes_refused == ["proposed"], (answer, state)
    answer, state = _confirm(Options(), _info(["proposed"], []), None)
    assert answer == "no_proposed" and state.yes_refused == [], (answer, state)


def test_slash_with_yes_asks_for_a_conductor_exempt_reason():
    """★ End to end through ``/handover to … --yes`` with the backend fake passing ``yes_exempt`` like the conductor:
    with ``ask_choice`` the person is asked and their ``cancel`` locks nothing; without it the job is cancelled."""
    plugin, backend, ctx = _plugin()
    backend.confirm_reasons = ["large"]
    backend.confirm_even_with_yes = True
    backend.yes_exempt = ["large"]
    asked: list[str] = []
    ctx.ask_choice = lambda question, choices, **_kw: asked.append(question) or "cancel"
    out = _slash(plugin, "to server --yes")
    assert asked and backend.confirm_answers == ["cancel"], (asked, backend.confirm_answers)
    assert "continue_async" not in backend.names() and "Nothing was locked" in out, out


# ---------------------------------------------------------------------------------------------------------
# 4. the role of a participant job in the status line (V43)
# ---------------------------------------------------------------------------------------------------------


def _participant(role: str, mode: str, state: str = "done", **extra) -> dict:
    return {
        "job_id": JOB,
        "mode": mode,
        "role": role,
        "state": state,
        "peer": "laptop",
        "progress": {"step": state},
        "calling_session": "20261109_120000_eeeeee",
        "title": "Flight work",
        **extra,
    }


def test_status_line_names_this_gateways_role_and_the_other_side():
    """★ (V43, L3-F04): a push that arrived here reads ``target`` and "from laptop" and names the session
    it became; the source side of a pull reads ``source`` and "to laptop"; this gateway's own (conductor) jobs keep
    their line.

    Red (on 16698398): ``assert 'target' in '…JOB · push · laptop · done'``.
    """
    arrived = surfaces.render_status({"jobs": [_participant("target", "push")]})
    head = arrived.splitlines()[0]
    assert "arriving here from laptop" in head and "done" in head, arrived
    assert "session here: 20261109_120000_eeeeee (Flight work)" in arrived, arrived

    taken = surfaces.render_status({"jobs": [_participant("source", "pull", "handed_over")]})
    head = taken.splitlines()[0]
    assert "leaving here for laptop" in head and "handed_over" in head, taken

    own = surfaces.render_status({"jobs": [{**_participant("conductor", "push"), "calling_session": SID}]})
    assert own.splitlines()[0].endswith("· handover to laptop · done"), own
    assert "session here" not in own and "arriving here" not in own and "leaving here" not in own, own


@pytest.mark.parametrize("role", ["", None, "bogus", 7])
def test_status_line_invents_no_role(role):
    """A missing or unknown ``role`` is shown as before (no "source"/"target" claimed)."""
    shown = surfaces.render_status({"jobs": [_participant(role, "push")]})
    assert "target" not in shown and "source" not in shown and "session here" not in shown, shown


def test_readme_names_reclaim_session_and_the_role_in_status():
    """README (G-F: README against the code): ``reclaim --session`` in a terminal and the role of jobs another
    gateway runs in ``/handover status``."""
    text = (PLUGIN_ROOT / "README.md").read_text(encoding="utf-8")
    assert "hermes handover reclaim --session <id>" in text
    assert "listed with this gateway's role" in text
