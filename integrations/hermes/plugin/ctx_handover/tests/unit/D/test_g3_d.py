"""D, round G‴: a session larger than the target model's context window (R32, lab finding L3-F17).

L3-F17 (lab L24): the source held about 160 000 characters of conversation, the target's model was configured with
``model.context_length: 4000`` — the same model on both sides, so ``model_change`` was None — and the handover ran
to ``done`` without a question; neither the pre-send text nor the note mentioned the window. R32: the planning
compares the estimated size of the *active* context with the target model's window; larger ⇒ the pre-send summary
and the note say so (the target compacts on the first turn) and a confirmation is needed that ``--yes`` does not give
(``YES_EXEMPT``, like ``workspace_excluded``). No abort.

Each test reproduces the finding without Hermes: the planning functions get exactly what the conductor holds after
NEGOTIATED (FinalPlan, source and target facts).
"""

from __future__ import annotations

import dataclasses
import json
from dataclasses import replace

import pytest

from ctx_handover.engine import note, plan
from ctx_handover.engine.model import Options, PlanHints, RefineReport, ResourcePlan
from ctx_handover.tests.fakes.planning_fake import (
    Busy,
    env_facts,
    final_plan,
    model_change,
    plan_item,
    resource_plan,
)
from ctx_handover.tests.unit.D.test_note import _compose

# lab L24: ~160 000 characters of conversation, the target's ``model.context_length: 4000``, same model both sides
L24_TOKENS = 160_000 // 4
FAKE = {"model": "fake-model", "provider": "custom"}


def _l24(tokens: int = L24_TOKENS, *, window: int | None = 4000, **change):
    """The conductor's view of lab step L24: ``(final, source_facts, target_facts)``."""
    final = final_plan(resource_plan(approx_tokens=tokens), model_change=change.get("model_change"))
    source = env_facts("server", host=dict(FAKE, context_length=None))
    target = env_facts("laptop", host=dict(FAKE, context_length=window))
    return final, source, target


# --------------------------------------------------------------------------------------------------------
# R32: confirmation and pre-send summary
# --------------------------------------------------------------------------------------------------------


def test_g3_context_over_window_when_the_model_stays_needs_an_answer():
    """★ L3-F17 / R32: the model stays (``model_change`` None) and the target's own window (its facts) is smaller than
    the session — reason ``context_over`` (letter i), which ``--yes`` does not confirm; the pre-send summary names
    size, window and model. Before G‴: no reason at all (the window was read only from a model change), so the
    handover ran without a question (lab L24, rc 0)."""
    final, source, target = _l24()
    reasons = plan.confirmation_reasons(final, Options(), False, target_facts=target, source_facts=source)
    assert reasons == [plan.CONFIRM_CONTEXT] == ["context_over"]
    assert plan.yes_exempt(reasons) == ["context_over"]
    assert plan.CONFIRM_LETTERS["context_over"] == "i" and plan.REASON_CODES[8] == "context_over"
    summary = plan.presend_summary(final, target_facts=target, source_facts=source)
    assert summary["context_over"] == {"tokens": L24_TOKENS, "context_length": 4000, "model": "fake-model"}
    assert "context_over" in plan.PRESEND_KEYS
    # a session that fits, or a window nobody configured: no reason, no summary entry (no abort either way)
    for fits in (_l24(3999), _l24(4000), _l24(window=None)):  # exactly the window still fits
        f, s, t = fits
        assert plan.confirmation_reasons(f, Options(), False, target_facts=t, source_facts=s) == []
        assert plan.presend_summary(f, target_facts=t, source_facts=s)["context_over"] is None


def test_g3_context_over_window_of_a_changed_model_is_not_confirmed_by_yes():
    """★ R32: with a model change the window is ``model_change.to.context_length`` (X04) — no facts needed. Before G‴
    this was reason (e) ``model``, which a blanket ``--yes`` confirmed (``yes_exempt`` was empty)."""
    big = final_plan(
        resource_plan(approx_tokens=200_000),
        model_change=model_change(to_model="q", to_provider="llamacpp", context_length=131072, offline_capable=True),
    )
    reasons = plan.confirmation_reasons(big, Options(), True)
    assert reasons == [plan.CONFIRM_CONTEXT]
    assert plan.yes_exempt(reasons) == [plan.CONFIRM_CONTEXT]
    assert plan.presend_summary(big)["context_over"] == {"tokens": 200_000, "context_length": 131072, "model": "q"}
    # reason (e) keeps only the offline rule: a local source handed to a model that does not work offline
    cloud = final_plan(resource_plan(approx_tokens=1000), model_change=model_change(offline_capable=False))
    assert plan.confirmation_reasons(cloud, Options(), True) == [plan.CONFIRM_MODEL]
    assert plan.yes_exempt([plan.CONFIRM_MODEL]) == []


@pytest.mark.parametrize(
    ("change", "source_host", "target_host", "window"),
    [
        ("change", FAKE, FAKE, 131072),  # model change: to.context_length, facts not read
        ("change-no-window", FAKE, FAKE, None),  # model change without a configured window: unknown
        (None, FAKE, dict(FAKE, context_length=4000), 4000),  # model stays, same main model: the target's window
        (None, {"model": "other", "provider": "custom"}, dict(FAKE, context_length=4000), None),  # kept elsewhere
        (None, {"model": "fake-model", "provider": "x"}, dict(FAKE, context_length=4000), None),  # other provider
        (None, None, dict(FAKE, context_length=4000), 4000),  # no source facts: the target's own model
        (None, FAKE, dict(FAKE, context_length=0), None),
        (None, FAKE, dict(FAKE, context_length=True), None),
        (None, FAKE, dict(FAKE, context_length=-5), None),
        ("legacy", FAKE, dict(FAKE, context_length=4000), None),  # layer-1 spelling: not X04, window unknown
    ],
)
def test_g3_target_window_rule(change, source_host, target_host, window):
    """The window of the target model (``plan.target_window`` = ``note.context_window``, one rule for plan and note):
    a model change reads only ``to.context_length``; a model that stays is the target's main model only when the
    source's main model and provider are the same (``session_import.context_length`` reads ``model.context_length``
    only for the main model); every other value is unknown (None) — no reason, no note line."""
    mc = {
        "change": model_change(context_length=131072),
        "change-no-window": model_change(context_length=None),
        "legacy": {"old": {}, "new": {}, "context_length": 1},
        None: None,
    }[change]
    source = env_facts("server", host=dict(source_host)) if source_host is not None else None
    target = env_facts("laptop", host=dict(target_host))
    assert plan.target_window(mc, target_facts=target, source_facts=source) == window
    assert note.context_window(mc, target=target, source=source) == window
    assert plan.target_window(None, target_facts=None, source_facts=source) is None


# --------------------------------------------------------------------------------------------------------
# The active context: the tip segment's live rows, not the whole lineage
# --------------------------------------------------------------------------------------------------------


def test_g3_active_context_tokens_count_the_tip_and_its_live_rows():
    """★ R32 "estimated size of the active context": rows of earlier segments of a compression lineage and rows a
    compaction archived (``active`` 0) are not in the model's context on the target — the lineage total
    (``approx_tokens``) is only an upper bound. Tool-call arguments are part of the context."""
    calls = [{"id": "c1", "function": {"name": "write_file", "arguments": "x" * 396}}]
    rows = [
        {"session_id": "root", "active": 1, "content": "a" * 4000},  # earlier segment
        {"session_id": "tip", "active": 0, "compacted": 1, "content": "b" * 4000},  # archived by a compaction
        {"session_id": "tip", "active": 1, "content": "c" * 400},
        {"session_id": "tip", "active": 1, "content": None, "tool_calls": calls},
        {"session_id": "tip", "active": 1, "content": "d" * 40, "tool_calls": json.dumps(calls)},
        {"content": "e" * 80},  # no session id, no flag: counted (cannot be told apart)
        "not a row",
    ]
    tool = len(json.dumps(calls))
    assert plan.active_context_tokens(rows, "tip") == (400 + tool + 40 + tool + 80) // 4
    assert plan.active_context_tokens(rows, None) == (4000 + 400 + tool + 40 + tool + 80) // 4
    assert plan.active_context_tokens([], "tip") == 0


def test_g3_build_plan_carries_the_active_context_estimate(lay):
    """The plan carries the active-context estimate (``build_plan(context_tokens=…)`` → ``ResourcePlan.context_tokens``,
    a K0 field added after the v1 wire form); the comparison uses it, and ``approx_tokens`` only when the plan has no
    estimate (an older source, or before K0 adds the field: then the lineage total is the upper bound)."""
    cfg = lay.cfg()
    rp = plan.build_plan(
        "01JCTESTJOB0000000000000000",
        (1, 2),
        [],
        None,
        RefineReport(used="off"),
        PlanHints(),
        cfg,
        lambda _root: None,
        Busy(),
        [],
        Options(),
        900_000,
        context_tokens=3000,
    )
    has_field = "context_tokens" in {f.name for f in dataclasses.fields(ResourcePlan)}
    assert plan.context_tokens_of(rp) == (3000 if has_field else 900_000)
    assert rp.approx_tokens == 900_000  # the capsule estimate and the refinement keep the lineage total
    if has_field:  # what the estimate decides: the window of 4000 holds 3000 tokens
        final, source, target = _l24()
        fits = replace(final, plan=rp)
        assert plan.context_over(fits, target_facts=target, source_facts=source) is None
    lineage_only = resource_plan(approx_tokens=5000)
    assert plan.context_tokens_of(lineage_only) == 5000


# --------------------------------------------------------------------------------------------------------
# The note on the target (no conductor wiring needed: the target composes it from its own facts)
# --------------------------------------------------------------------------------------------------------


def _note(tokens, *, model_change_=None, source_host=FAKE, target_ctx=4000):
    source = env_facts("server", host=dict(source_host))
    target = env_facts("laptop", host=dict(FAKE, context_length=target_ctx))
    parts = {} if tokens is None else {"context_tokens": tokens}
    return _compose(source=source, target=target, note_parts=parts, model_change=model_change_)


def test_g3_note_says_the_target_compacts_on_the_first_turn():
    """★ L3-F17 / R32: the note on the target says that the conversation is larger than the model's window there and
    that Hermes compacts it on the first turn (before G‴: the note said only "fake-model (custom, 3k context)").
    The size travels as the note part ``context_tokens`` (``plan.note_parts_from``); the window is the target's own."""
    text, report = _note(L24_TOKENS)
    line = (
        "Context: this conversation (about 40,000 tokens) is larger than the context window of fake-model here "
        "(4,000 tokens); Hermes compacts it on the first turn."
    )
    assert line in text and line in report
    assert text.index("Model: fake-model") < text.index("Context: this conversation")
    for quiet in (
        _note(3999),
        _note(None),
        _note(L24_TOKENS, target_ctx=None),
        _note(L24_TOKENS, source_host={"model": "other", "provider": "custom"}),
        _note(L24_TOKENS, model_change_=model_change(to_model="big", to_provider="custom", context_length=131072)),
    ):
        assert "Context: this conversation" not in quiet[0]
    # the size comes from the peer: only a plain positive integer counts
    for bad in (True, "40000", -1, 0, 1e9, float("inf")):
        assert "Context: this conversation" not in _note(bad)[0], bad


def test_g3_note_parts_carry_the_estimate():
    """``plan.note_parts_from`` sends the estimate as ``context_tokens`` (a key ``note.compose`` renders, X16)."""
    rp = resource_plan(approx_tokens=L24_TOKENS)
    parts = plan.note_parts_from(rp)
    assert parts["context_tokens"] == L24_TOKENS
    assert "context_tokens" in note.NOTE_PART_KEYS and note.unknown_note_parts(parts) == ()
    item = plan_item()
    assert plan.note_parts_from(final_plan(resource_plan((item,), approx_tokens=7)))["context_tokens"] == 7


# --------------------------------------------------------------------------------------------------------
# Integrator decision G‴ (question of g3-a §6): ``refine: off`` stops only NEW model calls
# --------------------------------------------------------------------------------------------------------


def test_g3_refine_off_still_takes_the_stored_offer_refinement():
    """★ An offer's refinement was asked in the source's own gateway when the offer was made (R9, DESIGN §8.3); a
    pull with ``refine: off`` (the target's setting reaches the source through ``participant_options``) stops new
    model calls only — the stored refinement is taken as it is (re-validated), no side question and no llm call run.
    Without a stored one, or when it fails validation, ``off`` stays off. Before G‴: ``off`` returned before the
    stored refinement was looked at (``refine.py`` ~470-473), so the offer lost it."""
    from ctx_handover.engine import refine
    from ctx_handover.engine.model import Refinement
    from ctx_handover.tests.fakes.planning_fake import ScriptedModel
    from ctx_handover.tests.unit.D.test_refine import VALID, _run

    stored = refine.validate(VALID)
    model = ScriptedModel(replies=[VALID, VALID])
    ref, report = _run(model, options=Options(refine="off"), stored=stored)
    assert (ref, report.used) == (stored, "offer") and model.seen == []
    ref, report = _run(model, options=Options(refine="off"))
    assert ref is None and report.used == "off" and model.seen == []
    tampered = Refinement(add=(), drop=(), tools_needed=(), env_vars_needed=(), notes="n" * 5000)
    ref, report = _run(model, options=Options(refine="off"), stored=tampered)
    assert ref is None and report.used == "off" and "offer" in (report.error or "") and model.seen == []
    ref, report = _run(model, options=Options(refine="bogus"), stored=stored)  # an unknown mode is off
    assert (ref, report.used) == (stored, "offer") and model.seen == []
