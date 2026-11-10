"""R36 / L3-F19, cluster F part: the summary before sending names the workspace against the session's
folder, and a workspace that holds more than that folder needs a confirmation ``--yes`` does not give (reason (j)
``workspace_differs``); the README says how the workspace is chosen.

D's ``presend_summary["workspace"]`` has the form ``{"path", "bytes", "cwd", "cwd_bytes", "differs"}`` (None when the
session's folder does not travel); F shows it as it is (V10) and invents nothing for a malformed value."""

from __future__ import annotations

import re
from dataclasses import replace

import pytest

from ctx_handover.engine import plan as planning
from ctx_handover.engine import surfaces
from ctx_handover.engine.commands import Call, _ConfirmState, _yes_refused_text
from ctx_handover.engine.model import Mode, Options
from ctx_handover.tests.fakes.surfaces_fake import sample_final_plan

from ._l2r_helpers import FLAT
from .test_commands import _plugin, _slash

MIB = 1024**2
DIFFERS = {
    "path": "/home/u/work/foreign",
    "bytes": 102 * MIB,
    "cwd": "/home/u/work/foreign/src",
    "cwd_bytes": 3 * 1024,
    "differs": True,
}
"""The lab's case after R36 in the form of ``plan.presend_summary["workspace"]``: a cwd git does not ignore, so the
workspace is the whole checkout around it."""


def _presend(**extra) -> dict:
    return {**planning.presend_summary(sample_final_plan()), **extra}


def _plan_from_a_subfolder(tracked: bool | None = None):
    """The sample plan as D plans a session started in ``/srv/app/services/api`` (a folder of the repository git does
    not ignore): the primary is the repository, ``rel_cwd`` the folder, ``cwd_bytes`` its size, ``cwd_tracked``
    whether the folder holds tracked files (R36a; None = unknown)."""
    final = sample_final_plan()
    primary = replace(final.plan.items[0], rel_cwd="services/api", cwd_bytes=40 * 1024, cwd_tracked=tracked)
    return replace(final, plan=replace(final.plan, items=(primary, *final.plan.items[1:])))


def test_presend_question_and_plan_name_the_workspace_against_the_cwd():
    """★ R36: the first progress message, the confirmation question and the plan name the workspace with its size and,
    when it holds more than the session's folder, that folder with its size.

    Red on ``ae0749a5``: ``KeyError: 'workspace'`` — ``plan_summary`` had no such key, nothing named the workspace."""
    summary = surfaces.plan_summary(None, _presend(workspace=DIFFERS))
    assert summary["workspace"] == DIFFERS
    presend = surfaces.render_presend(summary, "laptop", Mode.PUSH)
    assert "project folder /home/u/work/foreign (102.0 MiB)" in presend, presend
    assert "session's working folder /home/u/work/foreign/src (3.0 KiB)" in presend, presend
    question = surfaces.confirm_question(summary, "laptop")
    assert "/home/u/work/foreign (102.0 MiB)" in question and "/home/u/work/foreign/src (3.0 KiB)" in question
    assert question.endswith("Continue?"), question
    plan_text = surfaces.render_plan(summary)
    assert "Project folder: /home/u/work/foreign (102.0 MiB)" in plan_text, plan_text
    assert "/home/u/work/foreign/src (3.0 KiB)" in plan_text, plan_text


def test_a_workspace_that_is_the_cwd_is_named_without_a_second_folder():
    """R36 ("in every case"): a workspace that is the session's folder is named with its size, without the comparison
    — over D's real summary of the sample plan."""
    summary = surfaces.plan_summary(sample_final_plan())
    assert summary["workspace"] == {
        "path": "/srv/app",
        "bytes": 3 * MIB,
        "cwd": "/srv/app",
        "cwd_bytes": 3 * MIB,
        "differs": False,
    }
    presend = surfaces.render_presend(summary, "server", Mode.PUSH)
    assert "project folder /srv/app (3.0 MiB)" in presend and "session's working folder" not in presend, presend
    assert "session's working folder" not in surfaces.confirm_question(summary, "server")


@pytest.mark.parametrize(
    "value",
    [None, "", "/srv/app", {"bytes": 3}, {"path": ""}, {"path": 7}, ["/x"]],
    ids=["none", "empty", "bare-string", "no-path", "empty-path", "int-path", "list"],
)
def test_a_malformed_workspace_is_not_shown(value):
    """Only D's form (a mapping with a non-empty string ``path``) is shown; anything else counts as absent."""
    summary = surfaces.plan_summary(None, _presend(workspace=value))
    assert summary["workspace"] is None
    assert "workspace" not in surfaces.render_presend(summary, "x", Mode.PUSH)


def test_workspace_entry_reads_differs_from_the_paths_when_the_flag_is_odd():
    """A summary whose ``differs`` is not a bool, or False while the paths differ, is shown as differing (the surface
    rather says too much than hides the larger workspace); a missing ``cwd_bytes`` says the size is unknown."""
    odd = surfaces.workspace_entry({**DIFFERS, "differs": "no", "cwd_bytes": None})
    assert odd is not None and odd["differs"] is True and odd["cwd_bytes"] is None
    assert "size unknown" in surfaces.workspace_text(odd)
    assert surfaces.workspace_entry({**DIFFERS, "differs": False})["differs"] is True


def test_reason_workspace_differs_has_text_letter_and_is_yes_exempt():
    """★ R36: reason (j) ``workspace_differs`` has a text, the letter ``j``, a blanket yes does not confirm it and an
    unanswered question about it cancels.

    Red on ``ae0749a5``: ``KeyError: 'workspace_differs'`` in ``REASON_TEXT``."""
    assert surfaces.REASON_TEXT["workspace_differs"]
    assert surfaces.reason_names(["j"]) == ["workspace_differs"]
    assert surfaces.yes_exempt(["a", "j", "c"]) == ["workspace_differs"]
    assert surfaces.choice_on_timeout(["proposed", "workspace_differs"]) == "cancel"
    for code in planning.REASON_CODES:
        assert surfaces.REASON_TEXT.get(code), code


def test_d_reasons_of_a_plan_from_a_subfolder_reach_the_question():
    """R36 end to end over D's real functions: D's summary of the plan names the repository and the subfolder, D's
    reasons under ``--yes`` hold (j) and F treats it as not confirmed by ``--yes``."""
    final = _plan_from_a_subfolder()
    summary = surfaces.plan_summary(final)
    assert summary["workspace"] == {
        "path": "/srv/app",
        "bytes": 3 * MIB,
        "cwd": "/srv/app/services/api",
        "cwd_bytes": 40 * 1024,
        "differs": True,
    }
    reasons = planning.confirmation_reasons(final, Options(yes=True), True)
    assert surfaces.yes_exempt(reasons) == ["workspace_differs"], reasons


@pytest.mark.parametrize(("tracked", "asked"), [(True, False), (False, True), (None, True)])
def test_r36a_a_tracked_subfolder_is_named_but_not_asked_about(tracked, asked):
    """★ R36a over D's real functions: a session folder that holds tracked files of the repository around it is still
    named in the summary (workspace against folder, both sizes), but D raises no (j) for it, so ``--yes`` goes
    through; an untracked folder or an unknown answer is asked about.

    Red on ``08cfe0f0``: ``TypeError`` — ``PlanItem`` had no ``cwd_tracked``."""
    final = _plan_from_a_subfolder(tracked)
    summary = surfaces.plan_summary(final)
    assert summary["workspace"]["differs"] is True
    presend = surfaces.render_presend(summary, "server", Mode.PUSH)
    assert (
        "project folder /srv/app (3.0 MiB), more than the session's working folder /srv/app/services/api (40.0 KiB)"
        in presend
    )
    reasons = planning.confirmation_reasons(final, Options(yes=True), True)
    assert (surfaces.yes_exempt(reasons) == ["workspace_differs"]) is asked, reasons


def _confirm(options: Options, info: dict, asker) -> tuple[str, _ConfirmState]:
    plugin, _backend, _ctx = _plugin()
    state = _ConfirmState()
    call = Call(platform="local", session_id="s", session_key=None, asker=asker)
    return plugin._confirm_fn(call, "laptop", options, state)(info), state


def test_blanket_yes_asks_about_a_workspace_that_differs():
    """★ R36: with ``--yes`` the callback still asks a person when ``workspace_differs`` is a reason; the question names
    both folders and sizes; the person's ``yes`` goes ahead.

    Red on ``ae0749a5``: no question — ``--yes`` answered ``yes`` (D's ``yes_exempt`` did not know the reason)."""
    asked: list[str] = []

    def asker(question, choices):
        asked.append(question)
        return "yes"

    info = {
        "reasons": ["large", "workspace_differs"],
        "summary": _presend(workspace=DIFFERS),
        "final_plan": sample_final_plan(),
        "suspected_secrets": [],
        "instruction_files": [],
    }
    answer, state = _confirm(Options(yes=True), info, asker)
    assert len(asked) == 1, asked
    assert "/home/u/work/foreign (102.0 MiB)" in asked[0] and "/home/u/work/foreign/src (3.0 KiB)" in asked[0]
    assert surfaces.REASON_TEXT["workspace_differs"] in asked[0]
    assert answer == "yes" and state.yes_refused == ["workspace_differs"]


def test_yes_without_a_person_cancels_and_names_both_folders():
    """★ R36: ``--yes`` where nobody can answer ends with nothing locked; the message says that ``--yes`` does not
    confirm the larger workspace and names both folders with their sizes.

    Red on ``ae0749a5``: the handover went ahead (``continue_async`` called).

    Mutant: ``_yes_refused_text`` without the (j) sentence (the generic "--yes does not confirm this handover")."""
    plugin, backend, ctx = _plugin()
    backend.confirm_reasons = ["workspace_differs"]
    backend.confirm_even_with_yes = True  # the conductor asks despite --yes (plan.yes_exempt)
    backend.summary_extra = {"workspace": DIFFERS}
    ctx.ask_choice = None
    out = _slash(plugin, "to server --yes")
    assert backend.confirm_answers == ["cancel"]
    assert "continue_async" not in backend.names()
    assert (
        "--yes does not confirm the project folder /home/u/work/foreign (102.0 MiB), more than the session's "
        "working folder "
        "/home/u/work/foreign/src (3.0 KiB)." in out
    ), out
    assert "Nothing was locked" in out


def test_yes_refused_text_for_workspace_differs():
    text = _yes_refused_text({"workspace": DIFFERS}, ["workspace_differs"])
    assert text.startswith("--yes does not confirm the project folder /home/u/work/foreign (102.0 MiB), more than"), (
        text
    )
    plain = _yes_refused_text({}, ["workspace_differs"])
    assert "--yes does not confirm" in plain and "session's folder" in plain, plain


def test_readme_says_how_the_workspace_is_chosen():
    """★ R36: the README says that the workspace is the repository around the session's folder unless git ignores the
    folder there (then the folder alone travels as a file tree), that the summary names the workspace and its size
    next to the folder's, and that ``--yes`` does not confirm a workspace larger than the folder — bound to the
    constants behind it (D's reason table and the collector's reason text).

    Red on ``ae0749a5``: no such sentence."""
    use = FLAT.split("## Use", 1)[1].split("## Settings", 1)[0]
    assert re.search(r"git ignores[^.]*working folder|working folder[^.]*git ignores", use), "no sentence on R36"
    assert re.search(r"file tree", use)
    assert re.search(r"names the workspace[^.]*size", use), "the summary sentence is missing"
    assert re.search(r"`--yes` does not confirm[^.]*workspace", use) or re.search(
        r"Nor does it confirm[^.]*workspace", use
    ), "README does not say that --yes leaves the workspace question open"
    assert planning.CONFIRM_WORKSPACE_DIFFERS in planning.YES_EXEMPT
    from ctx_handover.engine import collector

    assert "file tree" in collector.IGNORED_REASON and "ignored" in collector.IGNORED_REASON
