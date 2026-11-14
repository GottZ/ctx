"""First review round — M37 (the first external review §2, High per orchestrator), H15 and M12 display: the summary
before sending named items outside the project only as a count ("N items outside the project") and said nothing of
repositories that stay here or of an unknown context window there. Cluster D's ``plan.presend_summary`` now carries
``outside_items``/``outside_more``, ``repos_left``/``repos_left_more`` and ``context_unknown``; ``plan_summary``
passes them through and ``render_presend``, ``confirm_question`` and ``render_plan`` name paths and sizes."""

from __future__ import annotations

from dataclasses import replace

from ctx_handover.engine import plan, surfaces
from ctx_handover.engine.model import Mode, NotTransferred
from ctx_handover.tests.fakes.surfaces_fake import sample_final_plan

SUB, NESTED = "/srv/app/vendor/lib", "/srv/app/tools/x"


def _final(extras: int = 1, repos: tuple[tuple[str, str], ...] = ()):
    final = sample_final_plan(extras=extras)
    left = tuple(NotTransferred(path=p, why=w) for p, w in repos)
    return replace(final, plan=replace(final.plan, not_transferred=final.plan.not_transferred + left))


def _view(final, **extra):
    return surfaces.plan_summary(final, {**plan.presend_summary(final), **extra})


def test_rv_f_m37_presend_names_outside_paths_with_size():
    """★ M37: an included file outside the project ⇒ the first progress line names its path and size, not a count.

    Red before the fix: "…; 1 items outside the project." (no path)."""
    final = _final(extras=1)
    text = surfaces.render_presend(_view(final), "server", Mode.PUSH)
    assert f"1 items outside the project folder: /home/u/extra0.txt ({surfaces.human_bytes(2048)})" in text, text


def test_rv_f_m37_presend_caps_the_list_and_counts_the_rest():
    """★ M37: more than ``plan.PRESEND_PATHS_MAX`` outside items ⇒ that many paths, then "and N more"."""
    final = _final(extras=plan.PRESEND_PATHS_MAX + 3)
    view = _view(final)
    assert len(view["outside_items"]) == plan.PRESEND_PATHS_MAX and view["outside_more"] == 3
    text = surfaces.render_presend(view, "server", Mode.PUSH)
    assert text.count("/home/u/extra") == plan.PRESEND_PATHS_MAX, text
    assert "and 3 more" in text, text


def test_rv_f_h15_presend_names_repositories_that_stay_here():
    """★ H15/(k): a submodule and a clone inside the transferred repository ⇒ "stays here: submodule …, repository …
    (their files do not travel)".

    Red before the fix: nothing about them."""
    final = _final(repos=((SUB, plan.WHY_SUBMODULE), (NESTED, plan.WHY_NESTED_REPO)))
    view = _view(final)
    assert [e["kind"] for e in view["repos_left"]] == ["nested", "submodule"], view["repos_left"]
    text = surfaces.render_presend(view, "server", Mode.PUSH)
    assert f"stays here: git repository {NESTED}, submodule {SUB} (their files do not travel)" in text, text
    more = surfaces.render_presend({**view, "repos_left_more": 4}, "server", Mode.PUSH)
    assert "and 4 more (their files do not travel)" in more, more


def test_rv_f_m12_presend_names_an_unknown_context_window():
    """★ M12 display: the model there is known, its window is not ⇒ "context window of <model> there unknown"; without
    a model name "of the model there".

    Red before the fix: nothing."""
    final = _final()
    view = _view(final, context_unknown={"tokens": 5000, "model": "qwen3"})
    assert "context window of qwen3 there unknown" in surfaces.render_presend(view, "server", Mode.PUSH)
    nameless = _view(final, context_unknown={"tokens": 5000, "model": None})
    assert "context window of the model there unknown" in surfaces.render_presend(nameless, "server", Mode.PUSH)
    none = _view(final, context_unknown=None)
    assert "context window" not in surfaces.render_presend(none, "server", Mode.PUSH)


def test_rv_f_m37_confirm_question_names_paths():
    """★ M37: the confirmation question names the outside paths with sizes, the repositories that stay and the unknown
    window — the person answers knowing what goes and what stays.

    Red before the fix: none of the paths in the question."""
    final = _final(extras=2, repos=((SUB, plan.WHY_SUBMODULE),))
    view = _view(final, context_unknown={"tokens": 5000, "model": "qwen3"})
    q = surfaces.confirm_question(view, "server")
    assert "Outside the project folder: /home/u/extra0.txt (" in q and "/home/u/extra1.txt (" in q, q
    assert f"Stays here: submodule {SUB}" in q and "do not travel" in q, q
    assert "The context window of qwen3 there is unknown" in q, q
    assert q.endswith("Continue?"), q


def test_rv_f_m37_render_plan_lists_sections():
    """★ M37: ``render_plan`` lists "Outside the project" (path · size) and "Stays here" entries and the unknown
    window."""
    final = _final(extras=2, repos=((SUB, plan.WHY_SUBMODULE), (NESTED, plan.WHY_NESTED_REPO)))
    text = surfaces.render_plan(_view(final, context_unknown={"tokens": 5000, "model": "qwen3"}))
    assert "Outside the project folder:" in text and f"  - /home/u/extra0.txt ({surfaces.human_bytes(2048)})" in text
    assert "Stays here (their files and changes do not travel):" in text, text
    assert f"  - submodule {SUB}" in text and f"  - git repository {NESTED}" in text, text
    assert "Context: the context window of qwen3 there is unknown" in text, text


def test_rv_f_m37_plan_summary_passes_the_keys_and_drops_bad_forms():
    """M37: ``plan_summary`` passes D's five keys through; an entry without a string path, a negative count or a
    ``context_unknown`` without a positive ``tokens`` is dropped (the surface invents nothing)."""
    final = _final()
    view = _view(
        final,
        outside_items=[{"path": "/a", "size": 1}, {"path": 7}, "x", {"path": "/b", "size": -1}],
        outside_more=-2,
        repos_left=[{"path": "/srv/app/s", "kind": "submodule"}, {"kind": "nested"}],
        repos_left_more="3",
        context_unknown={"tokens": 0, "model": "m"},
    )
    assert view["outside_items"] == [{"path": "/a", "size": 1}, {"path": "/b", "size": None}], view["outside_items"]
    assert view["outside_more"] == 0 and view["repos_left_more"] == 0
    assert view["repos_left"] == [{"path": "/srv/app/s", "kind": "submodule"}]
    assert view["context_unknown"] is None
