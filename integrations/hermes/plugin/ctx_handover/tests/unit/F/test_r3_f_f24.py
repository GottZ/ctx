"""Lab finding L3-F24 (F part, the D form ``plan.include_refused`` /
``presend_summary["include_refused"]`` comes from cluster D): a folder named with ``--include`` that this machine's
policy refuses (outside the home and ``export_roots``) left ``to … --yes`` without a word in the sender's output — the
first message ("Handover → …: 1 items …") and the final message did not name it; only ``status <job>``
(``exclusions``) and the note on the other side did.

Now the first message, the confirmation and the plan name each refused ``--include`` path with its reason, and so does
the final message (from the job's ``exclusions``)."""

from __future__ import annotations

from dataclasses import replace

from ctx_handover.engine import commands, plan, surfaces
from ctx_handover.engine.model import Mode, NotTransferred
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer, sample_final_plan
from ctx_handover.tests.unit.F._docs import docs_text

SID = "20261109_141003_a1b2c3"
TREE = "/srv/lab/ws-e2e/a/rvh9tree"
WHY = "outside home"
JOB = "01JC00000000000000000JOB01"


def _final():
    final = sample_final_plan()
    left = (NotTransferred(path=TREE, why=WHY), NotTransferred(path="/srv/other", why=WHY))
    return replace(final, plan=replace(final.plan, not_transferred=final.plan.not_transferred + left))


def _run(line: str, backend: FakeBackend, capsys) -> str:
    """``hermes handover <line>`` in a terminal: the first message goes to the terminal reporter, the rest to
    ``out``."""
    plugin = commands.Plugin(commands_ctx(), backend_factory=lambda c, p: backend)
    call = commands.Call(platform="cli", session_id=SID, session_key=None, asker=None, cli=True)
    plugin.run(line, call)
    captured = capsys.readouterr()
    return "\n".join([captured.out, captured.err, *call.out])


def commands_ctx():
    from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx

    return FakeCtx()


def test_r3_f_f24_to_yes_names_a_refused_include_first_and_last(capsys):
    """★ The lab scenario: ``hermes handover to server --include <tree> --yes`` where this machine's plan refuses the
    tree (``outside home``) — the first message and the final message name it with its reason; a path that was not
    named with ``--include`` is not listed as such.

    Red before the fix: "Handover → server: 1 items, 3.0 MiB; 0 secrets left out." and a final text without the
    tree."""
    backend = FakeBackend(peers=[make_peer("server")])
    backend.final = _final()
    backend.statuses[JOB] = {
        "job_id": JOB,
        "state": "done",
        "mode": "push",
        "exclusions": [{"path": TREE, "why": WHY}, {"path": "/srv/other", "why": WHY}],
    }
    out = _run(f"to server --include {TREE} --yes", backend, capsys)
    first = next(line for line in out.splitlines() if line.startswith("Handover →"))
    assert f"named with --include but left out: {TREE} ({WHY})" in first, first
    assert "/srv/other" not in first, first
    assert f"Named with --include but left out:\n  - {TREE} ({WHY})" in out, out
    assert "  - /srv/other" not in out, out


def test_r3_f_f24_d_form_passes_through_and_renders_everywhere():
    """D's form (``include_refused``/``include_refused_more``) reaches the three pre-send texts; a summary that does
    not carry it is completed from the final plan and the local ``--include`` paths."""
    final = _final()
    view = surfaces.plan_summary(final, plan.presend_summary(final, include=[TREE]))
    assert view["include_refused"] == [{"path": TREE, "why": WHY}] and view["include_refused_more"] == 0
    bare = surfaces.with_include_refused(surfaces.plan_summary(final), final, [TREE])
    assert bare["include_refused"] == view["include_refused"]
    assert surfaces.with_include_refused(surfaces.plan_summary(final), final, [])["include_refused"] == []
    for text in (
        surfaces.render_presend(view, "server", Mode.PUSH),
        surfaces.confirm_question(view, "server"),
        surfaces.render_plan(view),
    ):
        assert TREE in text and WHY in text, text
    more = surfaces.render_presend({**view, "include_refused_more": 2}, "server", Mode.PUSH)
    assert "and 2 more" in more, more
    assert surfaces.include_refused_entries([{"path": 3}, "x", {"path": TREE}]) == [{"path": TREE, "why": ""}]


def test_r3_f_f24_final_matches_the_include_as_typed_or_resolved(tmp_path):
    """The final message matches the job's exclusions against ``--include`` as typed, normalised and resolved."""
    link = tmp_path / "link"
    real = tmp_path / "real"
    real.mkdir()
    link.symlink_to(real)
    exclusions = [{"path": str(real), "why": WHY}, {"path": "/srv/x", "why": WHY}]
    assert surfaces.refused_includes(exclusions, [str(link) + "/"]) == [f"{real} ({WHY})"]
    assert surfaces.refused_includes(exclusions, []) == []


def test_r3_f_f24_readme_names_it():
    """README §Use says both places (first message, also with ``--yes``; final message)."""

    text = " ".join(docs_text().split())
    named = "the paths you named with `--include` that this machine's policy leaves out (with the reason, also with"
    assert f"{named} `--yes`)" in text
    assert "after a handover from here it names the `--include` paths this machine left out" in text
