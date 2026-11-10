"""Round G′ of cluster F: the excluded working folder before sending and in the
confirmation (V41, DECISIONS R30, L3-F02), the copy of ``export --keep`` in status and closing text (O3, V36), the
troubleshooting note of a source-checkout Hermes (V45, R31) and the success sweep over the F files."""

from __future__ import annotations

import re
from dataclasses import replace

import pytest

from ctx_handover.engine import commands, surfaces
from ctx_handover.engine import plan as planning
from ctx_handover.engine.commands import Call, Plugin, _ConfirmState
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Include, Mode, NotTransferred, Options, Peer, Right
from ctx_handover.tests.conftest import PLUGIN_ROOT, require_upstream
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer, sample_final_plan

from ._l2r_helpers import FLAT, backend_with_runtime, near
from .test_commands import _plugin, _slash
from .test_l2_contracts import ACK

EXCLUDED = {
    "path": "/opt/data/work/proj",
    "reason": "hermes",
    "why": "hermes",
}
"""``presend_summary["workspace_excluded"]`` in the form cluster D defines: the session's working
folder (role ``primary``, origin ``cwd``) the policy left out because it lies inside the Hermes home (L3-F02)."""


def _presend(**extra) -> dict:
    """D's presend summary of the sample plan plus ``extra`` keys (the conductor hands it over as it is, V10)."""
    return {**planning.presend_summary(sample_final_plan()), **extra}


# ---------------------------------------------------------------------------------------------------------
# V41 / R30 — the excluded working folder is named before sending and needs a confirmation --yes does not give
# ---------------------------------------------------------------------------------------------------------


def test_presend_names_the_excluded_working_folder():
    """★ V41 (R30, L3-F02): the first progress message, the plan and the confirmation question name the working
    folder the policy left out — with its path and why — instead of a bare "0 items, 0 B; 0 secrets left out"; the
    rendering summary carries D's ``workspace_excluded`` as it is (V10). Without the key nothing is added.

    Red (before G′, run on the tree of 94cf9fc1): ``KeyError: 'workspace_excluded'`` — ``plan_summary`` dropped the
    key, so neither ``render_presend`` nor ``confirm_question`` could say it.
    """
    summary = surfaces.plan_summary(None, _presend(workspace_excluded=EXCLUDED))
    assert summary["workspace_excluded"] == EXCLUDED
    presend = surfaces.render_presend(summary, "gottz", Mode.PUSH)
    assert "working folder /opt/data/work/proj stays here" in presend, presend
    assert "inside the Hermes home" in presend, presend
    question = surfaces.confirm_question(summary, "gottz")
    assert "/opt/data/work/proj stays here" in question and question.endswith("Continue?"), question
    plan_text = surfaces.render_plan(summary)
    assert "Working folder left out: /opt/data/work/proj" in plan_text, plan_text

    plain = surfaces.plan_summary(None, _presend())
    assert plain["workspace_excluded"] is None
    assert "working folder" not in surfaces.render_presend(plain, "gottz", Mode.PUSH)
    assert "working folder" not in surfaces.confirm_question(plain, "gottz")


@pytest.mark.parametrize(
    "value",
    [None, "", "/opt/data/work/proj", {"reason": "hermes"}, {"path": ""}, {"path": 7}, ["/x"]],
    ids=["none", "empty", "bare-string", "no-path", "empty-path", "int-path", "list"],
)
def test_presend_ignores_a_malformed_workspace_excluded(value):
    """Only D's documented form (a mapping with a non-empty ``path``) is shown; anything else counts as absent —
    the surface invents no folder."""
    summary = surfaces.plan_summary(None, _presend(workspace_excluded=value))
    assert summary["workspace_excluded"] is None
    assert "working folder" not in surfaces.render_presend(summary, "gottz", Mode.PUSH)


@pytest.mark.parametrize(
    ("reason", "why", "needle"),
    [
        ("hermes", "hermes", "inside the Hermes home"),
        ("home", "home", "the home folder itself"),
        ("other", "denied by a rule", "denied by a rule"),
        ("unheard-of", "", "the plugin's rules leave it out"),
    ],
)
def test_excluded_folder_reason_text(reason, why, needle):
    """The reason codes of ``plan.workspace_excluded`` get a sentence; an unknown code shows D's ``why`` text, else a
    neutral one."""
    text = surfaces.workspace_excluded_text({"path": "/p", "reason": reason, "why": why})
    assert needle in text, text


def test_reason_workspace_excluded_has_text_and_letter():
    """★ V41: the confirmation reason ``workspace_excluded`` (letter ``h`` of D's ``CONFIRM_LETTERS``) has a text; it
    is exempt from a blanket yes; an unanswered question about it cancels (it is not reason (a) alone).

    Red (before G′): ``KeyError: 'workspace_excluded'`` in ``REASON_TEXT``."""
    assert surfaces.REASON_TEXT["workspace_excluded"]
    assert surfaces.reason_names(["h"]) == ["workspace_excluded"]
    assert surfaces.yes_exempt(["a", "h", "c"]) == ["workspace_excluded"]
    assert surfaces.yes_exempt(["a", "b", "c", "d", "e", "f", "g"]) == []
    assert surfaces.choice_on_timeout(["workspace_excluded"]) == "cancel"
    assert surfaces.choice_on_timeout(["proposed", "workspace_excluded"]) == "cancel"
    for code in planning.REASON_CODES:  # every reason D can give has a text here
        assert surfaces.REASON_TEXT.get(code), code


def _plan_without_its_folder():
    """The sample plan as D plans a session whose working folder lies inside the Hermes home (L3-F02): the primary
    ``cwd`` item is ``no`` and named in ``not_transferred`` with why ``hermes``."""
    final = sample_final_plan()
    primary = replace(final.plan.items[0], include=Include.NO)
    plan = replace(final.plan, items=(primary,), not_transferred=(NotTransferred(path="/srv/app", why="hermes"),))
    return replace(final, plan=plan)


def test_d_summary_of_a_plan_without_its_folder_reaches_the_notice():
    """V41 end to end over cluster D's real functions: ``plan.presend_summary`` of such a plan gives
    ``workspace_excluded``, ``plan_summary`` keeps it, the notice names the folder, D's ``confirmation_reasons``
    give reason (h) also under ``--yes`` and F treats it as not confirmed by ``--yes``; ``--no-workspace`` is the
    explicit answer (no reason)."""
    final = _plan_without_its_folder()
    summary = surfaces.plan_summary(final)
    assert summary["workspace_excluded"] == {"path": "/srv/app", "reason": "hermes", "why": "hermes"}
    text = surfaces.render_presend(summary, "gottz", Mode.PUSH)
    assert "the working folder /srv/app stays here (it lies inside the Hermes home" in text, text
    reasons = planning.confirmation_reasons(final, Options(yes=True), True)
    assert surfaces.yes_exempt(reasons) == ["workspace_excluded"], reasons
    explicit = Options(yes=True, hints=replace(Options().hints, no_workspace=True))
    assert "workspace_excluded" not in planning.confirmation_reasons(final, explicit, True)


def _confirm(options: Options, info: dict, asker) -> tuple[str, _ConfirmState]:
    plugin, _backend, _ctx = _plugin()
    state = _ConfirmState()
    call = Call(platform="local", session_id="s", session_key=None, asker=asker)
    confirm = plugin._confirm_fn(call, "gottz", options, state)
    return confirm(info), state


def _info(*reasons: str) -> dict:
    return {
        "reasons": list(reasons),
        "summary": _presend(workspace_excluded=EXCLUDED),
        "final_plan": sample_final_plan(),
        "suspected_secrets": [],
        "instruction_files": [],
    }


def test_blanket_yes_does_not_confirm_the_excluded_working_folder():
    """★ V41 (R30): with ``--yes`` the confirmation callback still asks a person when ``workspace_excluded`` is a
    reason, and the question says which folder stays here; the person's ``yes`` goes ahead. Other reasons under
    ``--yes`` are still answered without a question.

    Red (before G′): the question was never asked — ``assert [] == [...]`` on the asked questions, answer
    ``"yes"`` straight from ``--yes``.
    """
    asked: list[str] = []

    def asker(question, choices):
        asked.append(question)
        return "yes"

    answer, state = _confirm(Options(yes=True), _info("large", "workspace_excluded"), asker)
    assert len(asked) == 1, asked
    assert "/opt/data/work/proj stays here" in asked[0], asked[0]
    assert surfaces.REASON_TEXT["workspace_excluded"] in asked[0]
    assert answer == "yes" and state.yes_refused == ["workspace_excluded"]

    asked.clear()
    answer, state = _confirm(Options(yes=True), _info("large"), asker)
    assert asked == [] and answer == "yes" and state.yes_refused == []


def test_yes_without_a_person_cancels_and_says_how_to_go_on():
    """★ V41 (R30): ``--yes`` where nobody can answer (no ``ask_choice``) ends with nothing locked; the message says
    that ``--yes`` does not confirm the excluded working folder, names it and offers ``--no-workspace``.

    Red (before G′): the handover went ahead (``continue_async`` called) — ``--yes`` confirmed it.
    """
    plugin, backend, ctx = _plugin()
    backend.confirm_reasons = ["workspace_excluded"]
    backend.confirm_even_with_yes = True  # the conductor asks despite --yes (plan.yes_exempt)
    backend.summary_extra = {"workspace_excluded": EXCLUDED}
    ctx.ask_choice = None  # no person on this surface
    out = _slash(plugin, "to server --yes")
    assert backend.confirm_answers == ["cancel"]
    assert "continue_async" not in backend.names()
    assert "--yes does not confirm" in out and "/opt/data/work/proj" in out and "--no-workspace" in out, out
    assert "Nothing was locked" in out


def test_presend_notice_names_the_excluded_folder_after_a_person_said_yes():
    """★ V41: after the person confirmed, the first progress message (what the notice shows while the job runs)
    still names the folder that stays here (L3-F02: the only notice said "0 items, 0 B").

    Red (before G′): ``Handover → server: 1 items, 3.0 MiB; 0 secrets left out.``
    """
    plugin, backend, ctx = _plugin()
    backend.confirm_reasons = ["workspace_excluded"]
    backend.summary_extra = {"workspace_excluded": EXCLUDED}
    ctx.choice_answers = ["yes"]
    _slash(plugin, "to server")
    assert backend.confirm_answers == ["yes"] and "continue_async" in backend.names()
    reporter = backend.last("start_job")["reporter"]
    assert "working folder /opt/data/work/proj stays here" in reporter._line, reporter._line


def test_readme_names_the_excluded_working_folder_and_yes():
    """V41: the README's confirmation paragraph names the excluded working folder, that ``--yes`` does not confirm
    it and ``--no-workspace``."""
    use = FLAT.split("## Use", 1)[1].split("## Settings", 1)[0]
    assert re.search(r"working folder[^.]*left out", use), "no sentence on the excluded working folder"
    assert re.search(r"`--yes`[^.]*(does not|never)[^.]*working folder", use) or re.search(
        r"working folder[^.]*`--yes`[^.]*(does not|never)", use
    ), "README does not say that --yes leaves the working-folder question open"
    assert "--no-workspace" in use


# ---------------------------------------------------------------------------------------------------------
# O3 / V36 — export --keep is shown as a copy, from the job's own record
# ---------------------------------------------------------------------------------------------------------

EXPORT_JOB = "01JCAAAAAAAAAAAAAAAAEXPRT1"


@pytest.mark.parametrize(("kept", "options_keep"), [(True, False), (False, True), (True, True), (False, False)])
def test_export_closing_text_follows_the_jobs_source_kept(kept, options_keep):
    """★ O3/V36: the closing text of an export says "stays active here; the bundle is a copy" exactly when the job
    result says ``source_kept`` — also when the text is built from other options than the starting command's (a
    resumed job, the gateway notice) — and "read-only here" otherwise, whatever ``--keep`` the caller holds.

    Red (before G′): the text followed only ``options.keep_source`` — case (True, False) said "read-only", case
    (False, True) said "stays active".
    """
    plugin, backend, _ctx = _plugin()
    backend.statuses[EXPORT_JOB] = {
        "state": "done",
        "source_kept": kept,
        "bundle": {"path": "/b/x.ctxh.tar", "size": 1024},
        "title": "My Work",
    }
    text = plugin._final_text(EXPORT_JOB, Mode.EXPORT, None, Options(keep_source=options_keep))
    assert ("the handover file is a copy" in text) is kept, text
    assert ("read-only here" in text) is (not kept), text


def test_export_closing_text_without_the_key_keeps_the_command_option():
    """A status without ``source_kept`` (a backend that does not record it) falls back to the command's
    ``--keep``; ``source_kept`` other than ``True`` is not a copy."""
    plugin, backend, _ctx = _plugin()
    backend.statuses[EXPORT_JOB] = {"state": "done", "bundle": {"path": "/b/x.ctxh.tar"}}
    assert "the handover file is a copy" in plugin._final_text(EXPORT_JOB, Mode.EXPORT, None, Options(keep_source=True))
    backend.statuses[EXPORT_JOB] = {"state": "done", "source_kept": "yes", "bundle": {"path": "/b/x.ctxh.tar"}}
    assert "read-only here" in plugin._final_text(EXPORT_JOB, Mode.EXPORT, None, Options(keep_source=True))


def test_status_marks_a_kept_export_as_a_copy():
    """★ O3: ``/handover status`` marks an export with ``source_kept`` as a copy whose session stays active here;
    other jobs (and ``source_kept`` other than ``True``) get no such line.

    Red (before G′): ``assert 'copy' in '…EXPRT1 · export · bundle · done'``.
    """
    job = {"job_id": EXPORT_JOB, "mode": "export", "state": "done", "source_kept": True}
    text = surfaces.render_status({"jobs": [job]})
    assert "copy (--keep): this session stays active here" in text, text
    for other in ({**job, "source_kept": False}, {**job, "source_kept": "yes"}, {**job, "mode": "push"}):
        if other["mode"] == "push":
            other = {**other, "source_kept": False}
        assert "copy" not in surfaces.render_status({"jobs": [other]})


def test_readme_names_export_keep_as_a_copy():
    """O3: the README's use table says that ``export --keep`` leaves the session active and that the bundle is a
    copy, and that status and the final message say so."""
    assert near(FLAT, "`export --keep`", "copy") and near(FLAT, "`export --keep`", "status and the final message")


# ---------------------------------------------------------------------------------------------------------
# V45 / R31 — troubleshooting: a source-checkout Hermes under load skips the plugin
# ---------------------------------------------------------------------------------------------------------


def test_readme_troubleshooting_names_the_version_skip(monkeypatch):
    """V45 (R31): the README section "Troubleshooting" quotes the log line Hermes writes when it skips this plugin
    (built from upstream's own ``requires_hermes_error`` and the loader's warning format), the floor version of
    ``plugin.yaml``, the version probe (``git tag --merged HEAD`` with upstream's timeout) and what to do.

    Red (before G, README of abcbd8a7): ``IndexError`` — no "## Troubleshooting" section.
    """
    import hermes_yaml
    from hermes_cli import plugins_manifest

    root = require_upstream()
    loader = (root / "hermes_cli" / "plugins_loader.py").read_text(encoding="utf-8")
    fmt = re.search(r"logger\.warning\(\"(Plugin '%s' skipped: %s)\", plugin_key, reason\)", loader)
    assert fmt, "upstream loader no longer logs the skipped plugin this way"
    manifest = hermes_yaml.safe_load((PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8"))
    monkeypatch.setattr(plugins_manifest, "running_hermes_version", lambda: "0.21.5")
    reason = plugins_manifest.requires_hermes_error(manifest)
    logged = fmt.group(1) % (manifest["name"], reason)
    assert logged == f"Plugin 'ctx_handover' skipped: requires hermes {manifest['requires_hermes']}, running 0.21.5"

    section = re.sub(r"\s+", " ", (PLUGIN_ROOT / "README.md").read_text(encoding="utf-8"))
    section = section.split("### Troubleshooting", 1)[1].split(" ### ", 1)[0]
    assert logged.split(", running", 1)[0] in section, section
    version_info = (root / "hermes_cli" / "version_info.py").read_text(encoding="utf-8")
    assert '"tag", "--merged", "HEAD"' in version_info and "`git tag --merged HEAD`" in section
    timeout = re.search(r"def _run_git\(.*?timeout=(\d+)", version_info, re.S)
    assert timeout and f"{timeout.group(1)}-second" in section
    floor = manifest["requires_hermes"].removeprefix(">=")
    assert f"{floor} or newer" in section
    assert "hermes plugins enable ctx_handover" in section
    assert "source checkout" in section and "restart the gateway" in section


# ---------------------------------------------------------------------------------------------------------
# Success sweep: success only on an explicitly positive value
# ---------------------------------------------------------------------------------------------------------

PUSH_JOB = "01JCAAAAAAAAAAAAAAAAPUSH01"


@pytest.mark.parametrize("state", ["", "locking", "transferring", "target_preparing", "committing", "weird"])
def test_closing_text_is_no_success_text_before_the_commit_point(state):
    """★ Sweep (``commands.py`` ``_final_text``): a success text ("Handed over …", "is here now", "Exported …")
    only for the states after the commit point (``committed``, ``retiring``, ``retired``, ``continuing``, ``done``);
    a missing, running or unknown state gets "not finished" with the status command.

    Red (before G′): every state outside in_doubt/aborting/aborted/suspended fell through to ``render_final`` ⇒
    ``Handed over to server: "this session" continues there (session ?, model default; …)``.
    """
    plugin, backend, _ctx = _plugin()
    backend.statuses[PUSH_JOB] = {"state": state, "ack": ACK}
    for mode in (Mode.PUSH, Mode.PULL, Mode.EXPORT, Mode.IMPORT):
        text = plugin._final_text(PUSH_JOB, mode, "server", Options())
        for success in ("Handed over", "is here now", "Exported", "is active here"):
            assert success not in text, (mode, text)
        assert "has not finished" in text and f"/handover status {PUSH_JOB[-6:]}" in text, text


@pytest.mark.parametrize("state", ["committed", "retiring", "retired", "continuing", "done"])
def test_closing_text_after_the_commit_point_is_a_success_text(state):
    """The states after the commit point keep their success text (push and pull)."""
    plugin, backend, _ctx = _plugin()
    backend.statuses[PUSH_JOB] = {"state": state, "ack": ACK}
    assert "Handed over to server" in plugin._final_text(PUSH_JOB, Mode.PUSH, "server", Options())
    pulled = plugin._final_text(PUSH_JOB, Mode.PULL, "server", Options())
    assert '"My Work" is' in pulled and "session 20261109_141003_a1b2c3" in pulled, pulled


@pytest.mark.parametrize("mode", [Mode.PUSH, Mode.PULL, Mode.IMPORT])
def test_closing_text_without_an_ack_invents_no_session_or_model(mode):
    """★ Sweep (L3-F03 seen from F): a finished job whose result carries no target acknowledgement (the gateway
    notice read it before the conductor wrote it) does not claim "session ?, model default; works offline:
    unknown" — it says the details are not recorded yet and names the status command.

    Red (before G′): ``Handed over to server: "this session" continues there (session ?, model default; works
    offline: unknown)``.
    """
    plugin, backend, _ctx = _plugin()
    backend.statuses[PUSH_JOB] = {"state": "done"}
    text = plugin._final_text(PUSH_JOB, mode, "server", Options())
    assert "session ?" not in text and "model default" not in text and "unknown" not in text, text
    assert f"/handover status {PUSH_JOB[-6:]}" in text, text


@pytest.mark.parametrize("started", ["false", 1, "yes", None])
def test_continuation_counts_as_started_only_when_it_says_true(started):
    """★ Sweep (``commands.py`` ``_final_text``, ``surfaces._job_line``): "The agent is continuing" and the
    status line ``continuation: pid …`` only for ``started is True``.

    Red (before G′): ``started: "false"`` ⇒ ``The agent is continuing on server.``
    """
    plugin, backend, _ctx = _plugin()
    cont = {"started": started, "pid": 42, "log": "/l", "running": True}
    backend.statuses[PUSH_JOB] = {"state": "done", "ack": ACK, "continuation": cont}
    text = plugin._final_text(PUSH_JOB, Mode.PUSH, "server", Options())
    assert "continuing" not in text, text
    line = surfaces.render_status(
        {"jobs": [{"job_id": PUSH_JOB, "mode": "push", "state": "done", "continuation": cont}]}
    )
    assert "pid 42" not in line, line
    backend.statuses[PUSH_JOB] = {"state": "done", "ack": ACK, "continuation": {**cont, "started": True}}
    assert "The agent is continuing on server." in plugin._final_text(PUSH_JOB, Mode.PUSH, "server", Options())


def test_status_continuation_running_only_when_true():
    """Sweep (``surfaces._job_line``): ``running`` other than ``True`` is not shown as running."""
    job = {"job_id": PUSH_JOB, "mode": "push", "state": "done"}
    shown = surfaces.render_status({"jobs": [{**job, "continuation": {"started": True, "pid": 7, "running": "no"}}]})
    assert "(running)" not in shown and "pid 7 (started)" in shown, shown


@pytest.mark.parametrize("notified", ["no", 1, None])
def test_reclaim_says_the_target_was_told_only_when_true(notified):
    """★ Sweep (``commands.py`` ``_verb_reclaim``): "The other gateway was told …" only for ``target_notified is
    True``.

    Red (before G′): ``target_notified: "no"`` ⇒ the sentence was shown.
    """
    plugin, backend, _ctx = _plugin()
    backend.reclaim = lambda sid, force: {"state": "reclaimed", "target_notified": notified}
    assert "The other machine was told" not in _slash(plugin, "reclaim")
    backend.reclaim = lambda sid, force: {"state": "reclaimed", "target_notified": True}
    assert "The other machine was told" in _slash(plugin, "reclaim")


def _inbound(name: str) -> Peer:
    return Peer(
        name=name,
        peer_id=ulid(),
        direction="inbound",
        url=None,
        pubkey="B" * 43,
        rights=frozenset({Right.PUSH, Right.PULL}),
    )


def test_peers_remove_of_an_inbound_device_claims_nothing(fake_host, tmp_path):
    """★ Sweep (``commands.py`` ``_verb_peers`` remove): the ledger removes dial entries only
    (``Ledger.remove_peer`` → False for an inbound peer); the answer must not say "Removed" then — it points to
    ``revoke`` and fails. Against the real ledger.

    Red (before G′): ``Removed laptop from this device. Pair again to use it.`` while the entry stayed.
    """
    backend, runtime = backend_with_runtime(fake_host, tmp_path)
    runtime.ledger.upsert_peer(_inbound("laptop"))
    ctx = FakeCtx(host=fake_host)
    plugin = Plugin(ctx, backend_factory=lambda c, p: backend)
    call = Call(platform="cli", session_id=None, session_key=None, asker=None, cli=True)
    plugin.run("peers remove laptop", call)
    out = "\n".join(call.out)
    assert "Removed" not in out, out
    assert "revoke laptop" in out and call.exit_code != commands.EXIT_OK, out
    assert runtime.ledger.peer("laptop") is not None


def test_peers_remove_reports_a_failed_removal(fake_host, tmp_path):
    """Sweep: a dial entry the ledger did not remove (gone meanwhile) is reported as not removed."""
    backend = FakeBackend(peers=[make_peer("server")])
    backend.remove_peer = lambda name: False
    plugin = Plugin(FakeCtx(host=fake_host), backend_factory=lambda c, p: backend)
    call = Call(platform="cli", session_id=None, session_key=None, asker=None, cli=True)
    plugin.run("peers remove server", call)
    assert "Removed" not in "\n".join(call.out) and call.exit_code != commands.EXIT_OK, call.out


def test_doctor_does_not_pass_continuation_when_the_approval_check_fails(fake_host, monkeypatch):
    """★ Sweep (``commands.local_checks`` ``allow_remote_continue``): when Hermes' approval setting cannot be read,
    the line is a warning ("not checked"), not ``ok True``.

    Red (before G′): ``assert True is None`` — the exception counted as "approvals apply".
    """
    from ctx_handover.engine import hostcompat

    def broken(config):
        raise RuntimeError("unreadable")

    monkeypatch.setattr(hostcompat, "approvals_off", broken)
    raw = {"allow_remote_continue": True}
    backend = FakeBackend(config={"plugins": {"entries": {"ctx_handover": {"settings": raw}}}})
    checks = {c["name"]: c for c in commands.local_checks(FakeCtx(host=fake_host), backend)}
    line = checks["allow_remote_continue"]
    assert line["ok"] is None and "not checked" in line["detail"], line
    monkeypatch.setattr(hostcompat, "approvals_off", lambda config: False)
    checks = {c["name"]: c for c in commands.local_checks(FakeCtx(host=fake_host), backend)}
    assert checks["allow_remote_continue"]["ok"] is True
    monkeypatch.setattr(hostcompat, "approvals_off", lambda config: True)
    checks = {c["name"]: c for c in commands.local_checks(FakeCtx(host=fake_host), backend)}
    assert checks["allow_remote_continue"]["ok"] is None


@pytest.mark.parametrize("result", [{"ok": False}, "queued", 1, None])
def test_inject_counts_only_an_explicit_true(result):
    """★ Sweep (``continuation.inject_after_adoption``): the prompt counts as queued only when
    ``ctx.inject_message`` answers ``True`` (its declared ``-> bool``); a dict, a word or a number means "use the
    subprocess".

    Red (before G′): ``assert True is False`` for ``{"ok": False}`` — ``bool(result)`` took any truthy answer.
    """
    from ctx_handover.engine.continuation import inject_after_adoption

    class Ctx:
        def inject_message(self, content, role="user", *, session_key=None, origin=None):
            return result

    assert inject_after_adoption(Ctx(), "agent:main:local:dm:x", "go") is False


# ---------------------------------------------------------------------------------------------------------
# V40 / R29 seen from F — setup names the root the default really resolves to
# ---------------------------------------------------------------------------------------------------------


def test_setup_names_the_reserved_root_when_home_is_the_hermes_home(fake_host, hermes_home, user_home, monkeypatch):
    """★ README against the code after R29: with HOME = HERMES_HOME (official Docker image) the default
    ``~/hermes-handover`` is ``<HERMES_HOME>/handover-workspaces`` (``paths.expand_handover_root``); ``/handover
    setup`` names that folder, not the raw setting. With a separate home it stays ``~/hermes-handover`` expanded.

    Red (before the fix): ``Received workspaces go to ~/hermes-handover (setting workspace_root; writes only under
    ~/hermes-handover).``
    """
    plugin, _backend, _ctx = _plugin()
    out = _slash(plugin, "setup")
    assert f"Project folders handed to this machine go to {user_home}/hermes-handover " in out, out
    monkeypatch.setenv("HOME", str(hermes_home))
    out = _slash(plugin, "setup")
    reserved = f"{hermes_home}/handover-workspaces"
    assert f"Project folders handed to this machine go to {reserved} " in out, out
    assert f"writes files only under {reserved} (setting allow_roots)" in out, out


def test_readme_names_the_reserved_default_root():
    """R29: the settings table says where the default root goes when the home folder is the Hermes home."""
    table = FLAT.split("## Settings", 1)[1].split("## Disclosure", 1)[0]
    assert "handover-workspaces" in table and near(table, "handover-workspaces", "Hermes home")


# ---------------------------------------------------------------------------------------------------------
# Integrator list N39, N28 — success only on an explicitly positive value
# ---------------------------------------------------------------------------------------------------------


def _runtime_backend(fake_host):
    from ctx_handover.engine import jobs

    jobs._reset_for_tests()
    backend = Plugin(FakeCtx(host=fake_host)).backend
    return backend, backend._started()


def _stop(rt) -> None:
    from ctx_handover.engine import jobs

    if rt is not None and getattr(rt, "veto", None) is not None:
        rt.veto.stop()
    jobs._reset_for_tests()


def test_gc_of_an_unknown_job_says_no_such_job(fake_host):
    """★ N39: ``gc --job <ULID>`` of a job this gateway has never seen (no ledger row in any role, no staging,
    report or continuation file, no object reference) answers "no such job" and fails — it does not report "removed
    everything of 1 jobs". Against the real profile runtime.

    Red (before the fix): ``backend.gc`` returned ``{'freed_bytes': 0, 'jobs': 1, 'skipped': 0}`` (no ``NotFound``).
    """
    from ctx_handover.engine.errors import NotFound

    backend, rt = _runtime_backend(fake_host)
    try:
        with pytest.raises(NotFound, match="no such job"):
            backend.gc(job_id=ulid(), all_jobs=False)
        plugin = Plugin(FakeCtx(host=fake_host), backend_factory=lambda c, p: backend)
        call = Call(platform="cli", session_id=None, session_key=None, asker=None, cli=True)
        plugin.run(f"gc --job {ulid()}", call)
        out = "\n".join(call.out)
        assert "removed everything" not in out and "no such job" in out and call.exit_code != commands.EXIT_OK, out
    finally:
        _stop(rt)


def test_gc_of_a_job_known_only_by_its_leftovers_still_cleans_them(fake_host):
    """N39: a job whose ledger rows are gone but whose staging folder or continuation log is still on disk counts
    as known — ``gc --job`` removes the leftovers and says so."""
    from ctx_handover.engine.continuation import job_files

    backend, rt = _runtime_backend(fake_host)
    try:
        data = fake_host.plugin_data
        staged = ulid()
        (data / "staging" / staged / "git").mkdir(parents=True)
        assert backend.gc(job_id=staged, all_jobs=False)["jobs"] == 1
        assert not (data / "staging" / staged).exists()
        logged = ulid()
        log, _sidecar = job_files(data / "logs", logged)
        log.parent.mkdir(parents=True, exist_ok=True)
        log.write_text("x", encoding="utf-8")
        assert backend.gc(job_id=logged, all_jobs=False)["jobs"] == 1 and not log.exists()
    finally:
        _stop(rt)


@pytest.mark.parametrize(
    ("entry", "shown"),
    [
        ({"rel": "AGENTS.md"}, "AGENTS.md (status unknown)"),
        ({"rel": "AGENTS.md", "status": ""}, "AGENTS.md (status unknown)"),
        ({"rel": "AGENTS.md", "status": "new"}, "AGENTS.md (new)"),
        ({"path": "/w/.hermes.md", "status": "changed"}, "/w/.hermes.md (changed)"),
    ],
)
def test_instruction_file_without_a_status_is_not_new(entry, shown):
    """★ N28: an instruction-file entry without ``status`` is shown as "status unknown", not as "new" — "new" is
    D's positive statement (``plan.instruction_files``), never a default.

    Red (before the fix): ``assert ['AGENTS.md (new)'] == ['AGENTS.md (status unknown)']``.
    """
    summary = surfaces.plan_summary(None, _presend(), instruction_files=[entry])
    assert summary["instruction_files"] == [shown]


# ---------------------------------------------------------------------------------------------------------
# V44 (L3-F05), F side: pairing without --name leaves the choice of a free name to cluster C
# ---------------------------------------------------------------------------------------------------------


class _InProcessListener:
    """An unpaired listener with an open pairing code (real A ``Ledger``, real C ``Server``, in process)."""

    def __init__(self, tmp_path) -> None:
        from ctx_handover.engine.crypto import Identity
        from ctx_handover.engine.ledger import Ledger
        from ctx_handover.engine.participant import ParticipantRegistry
        from ctx_handover.engine.server import PeerSignatureAuth, Server
        from ctx_handover.tests.unit.C._l2r_helpers import InProcessTransport

        self.ledger = Ledger(tmp_path / "listener" / "ledger.sqlite3")
        env: dict = {}
        self.identity = Identity.create(
            self.ledger, env_writable=lambda name: None, save_env=env.__setitem__, read_env_fresh=env.get
        )
        registry = ParticipantRegistry(self.ledger, lambda role, peer: None)
        auth = [PeerSignatureAuth(self.ledger, self.identity)]
        self.transport = InProcessTransport(Server(self.ledger, self.identity, registry, {"name": "server"}, auth))

    def paste(self) -> str:
        from ctx_handover.engine import pairing

        code = pairing.create_code(self.ledger, rights=frozenset(Right), name_hint="laptop", ttl_s=600)
        return pairing.pair_string(
            code, url="http://127.0.0.1:9", fingerprint=self.identity.fingerprint(), name="server", profile=None
        )


@pytest.fixture
def dialing_backend(fake_host, tmp_path, monkeypatch):
    """F's ``EngineBackend`` (real ledger) whose ``WireClient`` reaches an in-process listener."""
    from ctx_handover.engine import wire

    listener = _InProcessListener(tmp_path)
    backend, runtime = backend_with_runtime(fake_host, tmp_path)
    real = wire.WireClient

    def client(peer, ident, read_secret=None, **_kw):
        return real(peer, ident, read_secret=lambda n: "gate-token", transport=listener.transport, max_attempts=1)

    monkeypatch.setattr(wire, "WireClient", client)
    # H1: the pairing copies the form's credential into the peer's own variable, so the form slot holds one
    fake_host.secrets["CTX_HANDOVER_PEER_TOKEN"] = "gate-token"
    return backend, runtime, listener


def test_pair_without_a_name_takes_a_free_one_when_the_listener_name_is_taken(dialing_backend):
    """★ V44 (F side): ``/handover pair <code>`` without ``--name`` passes no name to
    ``pairing.redeem_dialer`` — C then names the listener after its paste string or, when that name is taken here
    (the usual two-sided pairing: the listener already dials in as ``server``), the next free one (``server-2``).
    Both sides end paired.

    Red (before the fix): F passed the paste-string name as an explicit ``name="server"`` ⇒ ``ConflictError: a peer
    named 'server' already exists on this device; the pairing code was not used. …``
    """
    from ctx_handover.engine.model import GateAuth, GateMode

    backend, runtime, listener = dialing_backend
    runtime.ledger.upsert_peer(
        Peer(
            name="server",
            peer_id=ulid(),
            direction="inbound",
            url=None,
            pubkey=listener.identity.public_b64(),
            rights=frozenset(Right),
        )
    )
    gate = GateAuth(mode=GateMode.SESSION_TOKEN)
    peer = backend.redeem_pair(listener.paste(), name=None, gate=gate, allow_insecure=False)
    assert peer.name == "server-2" and peer.direction == "dial", peer
    assert runtime.ledger.peer("server").direction == "inbound"
    mine = [p for p in listener.ledger.peers() if p.direction == "inbound"]
    assert len(mine) == 1 and mine[0].revoked_at is None, mine


def test_pair_with_an_explicit_taken_name_is_still_refused(dialing_backend):
    """V44: an explicit ``--name`` stays explicit — a taken one is refused before the code is used."""
    from ctx_handover.engine.errors import ConflictError
    from ctx_handover.engine.model import GateAuth, GateMode

    backend, runtime, listener = dialing_backend
    runtime.ledger.upsert_peer(
        Peer(name="b", peer_id=ulid(), direction="inbound", url=None, pubkey="C" * 43, rights=frozenset(Right))
    )
    with pytest.raises(ConflictError):
        backend.redeem_pair(
            listener.paste(), name="b", gate=GateAuth(mode=GateMode.SESSION_TOKEN), allow_insecure=False
        )
    assert [p for p in listener.ledger.peers() if p.direction == "inbound"] == []


def test_readme_names_how_the_dialer_names_the_listener():
    """V44: the pairing step says that without ``--name`` the next free name is taken and that a taken explicit
    name is refused before the code is used."""
    assert near(FLAT, "`server-2`", "next free name") and near(FLAT, "`--name`", "refused before the code is used")
