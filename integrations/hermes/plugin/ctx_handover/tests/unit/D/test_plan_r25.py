"""D, layer 2: suspected-secret marker and the meaning of "yes" (R25, X05/X06, W124/W126), refusals listing every
item (W017) and the target's ``model_change`` form for confirmation reason (e).

Every path in these tests exists in the isolated test home; the policy that decides is the real K0 policy.
"""

from __future__ import annotations

import os
from dataclasses import replace
from pathlib import Path

import pytest

from ctx_handover.engine import plan
from ctx_handover.engine.errors import PolicyDenied
from ctx_handover.engine.model import Candidate, DestMode, Include, ItemKind, Options, Origin, Weight
from ctx_handover.tests.fakes.planning_fake import decision, fake_redact, final_plan, model_change, replace_item
from ctx_handover.tests.unit.D.conftest import write
from ctx_handover.tests.unit.D.test_note import _compose
from ctx_handover.tests.unit.D.test_plan import _build, _extra, _items, _own, _primary

TOKEN_TEXT = "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"


def _files(lay):
    return (
        write(lay.notes / "keys.txt", TOKEN_TEXT),
        write(lay.notes / "agent-keys.txt", TOKEN_TEXT),
        write(lay.notes / "clean.md", "plain notes\n"),
        write(lay.notes / "release.txt", TOKEN_TEXT),
    )


def _agent(path: Path) -> list[dict]:
    return [{"resources": [{"path": str(path), "reason": "the agent needs it"}]}]


# --------------------------------------------------------------------------------------------------------
# X05 / W126: the exact marker
# --------------------------------------------------------------------------------------------------------


def test_suspected_secret_items_carry_exact_marker(lay):
    """★ X05 / W126 / R25: every plan item whose verdict is ``suspected_secret`` carries the exact reason element
    ``plan.SUSPECTED_SECRET`` (the plain text stays) — from transcript evidence and from agent suggestions; a clean
    file and a locally released ``include_secret`` file do not. Without a redactor no file is cleared, so every
    unchecked extra file carries it too (R25: "yes" only confirms what the policy found unsuspicious). The marker
    survives the reason cap and a replan.

    Mutant: ``plan._to_item`` writes only the plain text ``suspected secret: proposed only`` (no marker)."""
    assert plan.SUSPECTED_SECRET == "suspected_secret"
    tok, agent_tok, clean, released = _files(lay)
    options = Options(extra_paths="auto")
    cfg = lay.cfg(options=options, include_secret=[str(released)])
    cands = [_primary(lay), _extra(tok, Weight.WRITE), _extra(clean, Weight.WRITE), _extra(released, Weight.READ)]
    items = _items(_build(lay, cands, suggestions=_agent(agent_tok), options=options, cfg=cfg))
    for path in (tok, agent_tok):
        item = items[str(path)]
        assert plan.SUSPECTED_SECRET in item.reasons, path
        assert item.include is Include.PROPOSED
        assert "suspected secret: proposed only" in item.reasons
    assert plan.SUSPECTED_SECRET not in items[str(clean)].reasons
    assert items[str(clean)].include is Include.YES
    assert plan.SUSPECTED_SECRET not in items[str(released)].reasons
    assert items[str(released)].include is Include.YES

    unchecked = _items(_build(lay, cands, options=options, cfg=cfg, redact=None))
    assert plan.SUSPECTED_SECRET in unchecked[str(clean)].reasons
    assert unchecked[str(clean)].include is Include.PROPOSED
    assert plan.SUSPECTED_SECRET not in unchecked[str(released)].reasons
    assert unchecked[str(released)].include is Include.YES

    noisy = Candidate(
        ItemKind.EXTRA_FILE, str(tok), Weight.WRITE, tuple(f"read: {n}" for n in range(60)), Origin.TRANSCRIPT
    )
    capped = _items(_build(lay, [_primary(lay), noisy], options=options))[str(tok)]
    assert plan.SUSPECTED_SECRET in capped.reasons and len(capped.reasons) <= plan.MAX_REASONS

    own = _build(lay, [_primary(lay)], options=options)
    replanned, needs = plan.replan(own, [_primary(lay), _extra(tok, Weight.WRITE)], (1, 3), cfg, redact=fake_redact)
    late = _items(replanned)[str(tok)]
    assert needs is True and late.include is Include.PROPOSED and plan.SUSPECTED_SECRET in late.reasons


# --------------------------------------------------------------------------------------------------------
# X06 / W124 / R25: "yes" never takes suspected secrets along
# --------------------------------------------------------------------------------------------------------


def test_apply_choice_yes_never_confirms_suspected_secrets(lay):
    """★ R25 / X06 / W124: ``apply_choice(final, "yes")`` confirms the unsuspicious proposals and turns every item
    carrying ``suspected_secret`` into ``no`` (decision ``skip``, ``need_bytes`` 0, like ``no_proposed``); a local
    ``include_secret`` release is ``yes`` already and stays untouched with its decision. ``no_proposed`` is
    unchanged: every proposal becomes ``no``.

    Mutant: ``plan.apply_choice`` promotes every proposal on ``"yes"`` (the marker is ignored)."""
    tok, agent_tok, clean, released = _files(lay)
    options = Options(extra_paths="auto")
    cfg = lay.cfg(options=options, include_secret=[str(released)])
    cands = [_primary(lay), _extra(tok, Weight.WRITE), _extra(clean, Weight.MENTION)]
    resource_plan = _build(lay, cands, suggestions=_agent(agent_tok), options=options, cfg=cfg)
    decisions = [
        decision(i, f"/home/u/hermes-handover/{Path(i.source_path).name}", need_bytes=100) for i in resource_plan.items
    ]
    final = final_plan(resource_plan, decisions)
    before = _items(final.plan)
    assert before[str(clean)].include is Include.PROPOSED and before[str(released)].include is Include.YES
    assert final.need_bytes == 100 * len(resource_plan.items)

    yes = plan.apply_choice(final, "yes")
    items = _items(yes.plan)
    index = plan.decision_index(yes.decisions)
    for path in (tok, agent_tok):
        item = items[str(path)]
        assert item.include is Include.NO, path
        assert (index[item.item_id].mode, index[item.item_id].need_bytes) == (DestMode.SKIP, 0)
    assert items[str(clean)].include is Include.YES
    assert index[items[str(clean)].item_id].mode is DestMode.FRESH
    kept = items[str(released)]
    assert kept.include is Include.YES and index[kept.item_id] == plan.decision_index(final.decisions)[kept.item_id]
    assert not [i for i in yes.plan.items if i.include is Include.PROPOSED]
    assert yes.need_bytes == final.need_bytes - 200
    assert yes.plan_id == plan.with_plan_id(yes).plan_id and yes.plan_id != final.plan_id
    assert plan.CONFIRM_PROPOSED not in plan.confirmation_reasons(yes, options, False)

    dropped = _items(plan.apply_choice(final, "no_proposed").plan)
    assert {p for p, i in dropped.items() if i.include is Include.YES} == {os.path.realpath(lay.project), str(released)}

    unchecked = final_plan(_build(lay, cands, options=options, cfg=cfg, redact=None))
    assert _items(plan.apply_choice(unchecked, "yes").plan)[str(clean)].include is Include.NO


def test_include_secret_is_yes_at_planning(lay):
    """★ R25: a local ``include_secret`` release is ``yes`` already when planning — also when the collector found the
    file only as a mention (a proposal otherwise) and under ``extra_paths: ask`` — and neither choice touches it.

    Mutant: ``plan.build_plan`` skips ``include_secret`` paths the collector already found
    (``if path not in b.by_path``)."""
    env = write(lay.home / "svc" / ".env", "TOKEN=abc\n")
    keys = write(lay.notes / "keys.txt", TOKEN_TEXT)
    for mode in ("auto", "ask"):
        options = Options(extra_paths=mode)
        cfg = lay.cfg(options=options, include_secret=[str(env), str(keys)])
        cands = [_primary(lay), _extra(env, Weight.MENTION), _extra(keys, Weight.READ)]
        resource_plan = _build(lay, cands, options=options, cfg=cfg)
        for path in (env, keys):
            item = _items(resource_plan)[str(path)]
            assert item.include is Include.YES, (mode, path)
            assert plan.SUSPECTED_SECRET not in item.reasons
        final = final_plan(resource_plan)
        for choice in plan.CHOICES:
            out = _items(plan.apply_choice(final, choice).plan)
            assert out[str(env)].include is Include.YES and out[str(keys)].include is Include.YES, (mode, choice)


# --------------------------------------------------------------------------------------------------------
# W017 and R25 × R18 in restrict_to_releasable
# --------------------------------------------------------------------------------------------------------


def test_restrict_refusal_lists_item_ids(lay):
    """★ W017: ``restrict_to_releasable`` refuses with ``details["items"]`` = every refused item id (in plan order,
    once each), next to the existing details of the first refusal (``reason``, ``item``/``path``).

    Mutant: ``plan.restrict_to_releasable`` raises at the first refused item (``details["items"]`` names only it)."""
    own = _own(lay)
    w0, x0 = own.items
    other = write(lay.notes / "other.md")
    moved = final_plan(replace(own, items=(replace(w0, source_path=str(lay.home / ".ssh")), x0)))
    with pytest.raises(PolicyDenied) as single:
        plan.restrict_to_releasable(moved, own, lay.cfg())
    assert single.value.details["items"] == [w0.item_id]
    assert single.value.details["item"] == w0.item_id and single.value.details["reason"] == "not_allowed_root"

    own_no = replace(own, items=(w0, replace(x0, include=Include.NO)))
    addition = plan.user_item("r0", str(other), ItemKind.EXTRA_FILE)
    many = final_plan(
        replace(
            own_no, items=(replace(w0, kind=ItemKind.EXTRA_DIR), replace(x0, include=Include.YES), addition, addition)
        )
    )
    with pytest.raises(PolicyDenied) as several:
        plan.restrict_to_releasable(many, own_no, lay.cfg())
    assert several.value.details["items"] == [w0.item_id, x0.item_id, "r0"]
    assert several.value.details["item"] == w0.item_id and several.value.details["reason"] == "not_allowed_root"
    assert several.value.to_wire()["error"]["details"]["items"] == [w0.item_id, x0.item_id, "r0"]


def test_restrict_remote_yes_on_suspected_proposal_refused(lay):
    """★ R25 × R18: a remote conductor cannot confirm a proposal the source flagged as a suspected secret (only a
    local ``include_secret`` releases it); narrowing it is fine, an unsuspicious proposal may be confirmed, and the
    remote conductor's own ``apply_choice("yes")`` passes.

    Mutant: ``plan.restrict_to_releasable`` accepts a remote ``yes`` on an own ``suspected_secret`` proposal."""
    tok, _agent_tok, clean, _released = _files(lay)
    options = Options(extra_paths="auto")
    own = _build(lay, [_primary(lay), _extra(tok, Weight.WRITE), _extra(clean, Weight.MENTION)], options=options)
    t, c = _items(own)[str(tok)], _items(own)[str(clean)]
    assert t.include is Include.PROPOSED and plan.SUSPECTED_SECRET in t.reasons
    assert c.include is Include.PROPOSED and plan.SUSPECTED_SECRET not in c.reasons

    with pytest.raises(PolicyDenied) as refused:
        plan.restrict_to_releasable(final_plan(replace_item(own, t.item_id, include=Include.YES)), own, lay.cfg())
    assert refused.value.details["items"] == [t.item_id]
    assert refused.value.details["reason"] == "suspected_secret"

    narrowed = replace_item(replace_item(own, c.item_id, include=Include.YES), t.item_id, include=Include.NO)
    out = _items(plan.restrict_to_releasable(final_plan(narrowed), own, lay.cfg()).plan)
    assert out[str(clean)].include is Include.YES and out[str(tok)].include is Include.NO
    via_yes = _items(plan.restrict_to_releasable(plan.apply_choice(final_plan(own), "yes"), own, lay.cfg()).plan)
    assert via_yes[str(clean)].include is Include.YES and via_yes[str(tok)].include is Include.NO


# --------------------------------------------------------------------------------------------------------
# Point 4: the target's model_change form
# --------------------------------------------------------------------------------------------------------


def test_confirmation_reads_target_model_change_form(lay):
    """Reason (e) and the note read ``FinalPlan.model_change`` in the form the target delivers:
    ``{"from": {model, provider}, "to": {model, provider, context_length}, "changed", "offline_capable", "reason"}``.

    Mutant: ``plan._model_context`` reads only a top-level ``context_length``."""
    w0 = _own(lay).items[0]
    base = replace(_own(lay), items=(w0,), approx_tokens=1000)
    cloud = final_plan(base, model_change=model_change(offline_capable=False))
    assert set(cloud.model_change) == {"from", "to", "changed", "offline_capable", "reason"}
    assert plan.confirmation_reasons(cloud, Options(), True) == [plan.CONFIRM_MODEL]
    assert plan.confirmation_reasons(cloud, Options(), False) == []
    window = final_plan(
        replace(base, approx_tokens=200_000), model_change=model_change(offline_capable=True, context_length=131072)
    )
    assert plan.confirmation_reasons(window, Options(), True) == [plan.CONFIRM_MODEL]
    fits = final_plan(base, model_change=model_change(offline_capable=True, context_length=131072))
    assert plan.confirmation_reasons(fits, Options(), True) == []
    kept = final_plan(base, model_change=model_change(changed=False, offline_capable=None, context_length=None))
    assert plan.confirmation_reasons(kept, Options(), False) == []
    assert plan.summarize(window)["model_change"] == window.model_change

    text, _report = _compose(
        model_change=model_change(
            from_model="gpt-x",
            from_provider="openrouter",
            to_model="qwen3",
            to_provider="llamacpp",
            context_length=65536,
            offline_capable=False,
        )
    )
    assert "Model: gpt-x (openrouter) → qwen3 (llamacpp, 64k context) — works offline: no" in text
    unknown, _ = _compose(
        model_change=model_change(to_model="qwen3", to_provider="llamacpp", context_length=None, offline_capable=None)
    )
    assert "→ qwen3 (llamacpp, 64k context) — works offline: yes" in unknown
